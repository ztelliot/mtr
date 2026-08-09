package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
)

func testBearerHTTPAuth(secret string) config.HTTPAuth {
	return config.HTTPAuth{Type: config.HTTPAuthBearer, Secret: secret}
}

func testNoHTTPAuth() config.HTTPAuth {
	return config.HTTPAuth{Type: config.HTTPAuthNone}
}

func TestHTTPAgentInvokeStreamsResultEvents(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolIPv4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-ndjson" {
		t.Fatalf("content type = %q", got)
	}

	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("expected streamed lines, got %d", len(lines))
	}
	if _, ok := lines[0]["job_id"]; ok {
		t.Fatalf("stream event should not include job_id: %#v", lines[0])
	}
	if _, ok := lines[0]["agent_id"]; ok {
		t.Fatalf("stream event should not include agent_id: %#v", lines[0])
	}
	if _, ok := lines[0]["tool"]; ok {
		t.Fatalf("stream event should not repeat tool: %#v", lines[0])
	}
	if _, ok := lines[0]["target"]; ok {
		t.Fatalf("stream event should not repeat target: %#v", lines[0])
	}
	if lines[0]["type"] != model.JobErrorTargetBlocked || lines[0]["message"] != model.JobErrorTargetBlocked {
		t.Fatalf("first event = %#v", lines[0])
	}
	last := lines[len(lines)-1]
	if _, ok := last["stream"]; ok {
		t.Fatalf("parsed result should not include stream wrapper: %#v", last)
	}
	if _, ok := last["tool"]; ok {
		t.Fatalf("summary should not include stable tool field: %#v", last)
	}
	if last["type"] != "summary" || last["exit_code"].(float64) == 0 {
		t.Fatalf("last result = %#v", last)
	}
	if _, ok := last["metric"].(map[string]any); !ok {
		t.Fatalf("summary should carry metrics in metric: %#v", last)
	}
}

func TestHTTPAgentInvokeRequiresBearerScheme(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("raw token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer token status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAgentCustomHeaderAuthenticatesAllEndpoints(t *testing.T) {
	auth := config.HTTPAuth{Type: config.HTTPAuthHeader, Header: "X-Auth-Token", Secret: "secret"}
	handler := newHTTPAgentHandler(config.Agent{
		ID:           "edge-http-1",
		HTTPAuth:     auth,
		Capabilities: []string{"ping"},
		Protocols:    uint8(model.ProtocolIPv4),
		Speedtest: config.Speedtest{
			DefaultBytes:            8,
			MaxBytes:                8,
			GlobalRequestsPerMinute: 100,
			GlobalBurst:             100,
			IPRequestsPerMinute:     100,
			IPBurst:                 100,
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "healthz", method: http.MethodGet, path: "/healthz"},
		{name: "invoke", method: http.MethodPost, path: "/invoke", body: `{"protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`},
		{name: "speedtest", method: http.MethodGet, path: "/speedtest/random?bytes=8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.RemoteAddr = "203.0.113.80:1234"
			req.Header.Set(auth.Header, auth.Secret)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header[auth.Header] = []string{auth.Secret, auth.Secret}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("duplicate authentication headers status = %d", rec.Code)
	}
}

func TestEventAgentCustomHeaderAuthentication(t *testing.T) {
	auth := config.HTTPAuth{Type: config.HTTPAuthHeader, Header: "X-Function-Token", Secret: "secret"}
	handler := newEventAgentHandler(config.Agent{ID: "edge-event-1", HTTPAuth: auth, Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolIPv4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"action":"healthz","protocol_version":1}`

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing custom header status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set(auth.Header, auth.Secret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom header status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAgentInvokeRejectsMissingIDAndTrailingJSON(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, body := range map[string]string{
		"missing id":    `{"protocol_version":1,"tool":"ping","target":"1.1.1.1"}`,
		"trailing json": `{"protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1"}{}`,
		"unknown field": `{"protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1","legacy_mode":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer secret")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestHTTPAgentRejectsUnsupportedProtocolBeforeRunning(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolIPv4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"protocol_version":1,"id":"job-1","tool":"ping","target":"example.com","resolved_target":"2606:4700:4700::1111","ip_version":6,"args":{"count":"1"},"timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	lines := decodeNDJSONLines(t, rec.Body.String())
	if len(lines) != 2 {
		t.Fatalf("expected rejection event and summary, got %#v", lines)
	}
	if lines[0]["type"] != model.JobErrorUnsupportedProtocol || lines[0]["message"] != model.JobErrorUnsupportedProtocol {
		t.Fatalf("first event = %#v", lines[0])
	}
	metric, ok := lines[1]["metric"].(map[string]any)
	if !ok || lines[1]["type"] != "summary" || lines[1]["exit_code"].(float64) == 0 || metric["status"] != model.JobErrorUnsupportedProtocol {
		t.Fatalf("summary = %#v", lines[1])
	}
}

func TestHTTPAgentInvokeAuthFailuresAreRateLimited(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`

	for i := 0; i < 21; i++ {
		req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
		req.RemoteAddr = "203.0.113.60:1234"
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if i == 20 && rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected auth failures to become rate limited, status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPAgentAllowsPinnedPathTools(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:            "edge-http-1",
		HTTPAuth:      testBearerHTTPAuth("secret"),
		Country:       "CN",
		Region:        "edge",
		Provider:      "kubernetes",
		ISP:           "bgp",
		Capabilities:  []string{"traceroute", "mtr"},
		Protocols:     3,
		HideFirstHops: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tool := range []model.Tool{model.ToolTraceroute, model.ToolMTR} {
		body := `{"protocol_version":1,"id":"job-1","tool":"` + string(tool) + `","target":"127.0.0.1","ip_version":4,"timeout_seconds":1}`
		req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", tool, rec.Code, rec.Body.String())
		}
	}
}

func TestHTTPAgentRejectsToolOutsideConfiguredCapabilities(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:           "edge-http-1",
		HTTPAuth:     testBearerHTTPAuth("secret"),
		Capabilities: []string{"ping"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"protocol_version":1,"id":"job-1","tool":"dns","target":"example.com","args":{"type":"A"}}`
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), model.JobErrorUnsupportedTool) {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPAgentHealthIncludesVersion(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:            "edge-http-1",
		HTTPAuth:      testBearerHTTPAuth("secret"),
		Country:       "CN",
		Region:        "edge",
		Provider:      "kubernetes",
		ISP:           "bgp",
		Capabilities:  []string{"ping", "dns"},
		Protocols:     3,
		HideFirstHops: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected method status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
		Agent struct {
			ProtocolVersion   int          `json:"protocol_version"`
			ID                string       `json:"id"`
			Country           string       `json:"country"`
			Region            string       `json:"region"`
			Provider          string       `json:"provider"`
			ISP               string       `json:"isp"`
			Capabilities      []model.Tool `json:"capabilities"`
			Protocols         int          `json:"protocols"`
			MaxConcurrentJobs int          `json:"max_concurrent_jobs"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version.Version == "" || body.Agent.ProtocolVersion != grpcwire.AgentProtocolVersion || body.Agent.ID != "edge-http-1" || body.Agent.Country != "CN" || body.Agent.Protocols != 3 || len(body.Agent.Capabilities) != 2 {
		t.Fatalf("unexpected health response: %#v", body)
	}
	if body.Agent.MaxConcurrentJobs != agentMaxConcurrentJobs {
		t.Fatalf("max concurrent jobs = %d", body.Agent.MaxConcurrentJobs)
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	agent, ok := raw["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent profile missing: %#v", raw)
	}
	if _, ok := agent["hide_first_hops"]; ok {
		t.Fatalf("health response must not expose hide_first_hops: %#v", agent)
	}
	network, ok := raw["network"].(map[string]any)
	if !ok || network["ping_group_range"] == nil || network["cap_net_raw"] == nil || network["icmp_echo_datagram"] == nil {
		t.Fatalf("health response must expose network diagnostics: %#v", raw)
	}
}

func TestAgentHelloReportsProtocolVersionAndCapacity(t *testing.T) {
	hello := newAgentHello(config.Agent{ID: "edge-grpc", Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolAll)})
	if hello.ID != "edge-grpc" || hello.ProtocolVersion != grpcwire.AgentProtocolVersion || hello.MaxConcurrentJobs != agentMaxConcurrentJobs {
		t.Fatalf("hello = %#v", hello)
	}
}

func TestCapabilitySetContainsNETRAW(t *testing.T) {
	if !capabilitySetContains("0000000000002000", 13) {
		t.Fatal("expected CAP_NET_RAW bit to be detected")
	}
	if capabilitySetContains("00000004010000eb", 13) {
		t.Fatal("restricted runtime capability mask must not report CAP_NET_RAW")
	}
}

func TestAgentNetworkHealthPayloadReturnsIndependentCachedCopies(t *testing.T) {
	first := agentNetworkHealthPayload()
	first["ping_group_range"] = "mutated"
	if nested, ok := first["cap_net_raw"].(map[string]any); ok {
		nested["effective"] = "mutated"
	}
	second := agentNetworkHealthPayload()
	if second["ping_group_range"] == "mutated" {
		t.Fatalf("top-level health cache was exposed: %#v", second)
	}
	if nested, ok := second["cap_net_raw"].(map[string]any); !ok || nested["effective"] == "mutated" {
		t.Fatalf("nested health cache was exposed: %#v", second)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := agentNetworkHealthPayload()
			if payload["cap_net_raw"] == nil {
				t.Error("cached health payload missing cap_net_raw")
			}
		}()
	}
	wg.Wait()
}

func TestHTTPAgentPathPrefix(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:             "edge-http-1",
		HTTPAuth:       testBearerHTTPAuth("secret"),
		HTTPPathPrefix: "/api/v1",
		Capabilities:   []string{"ping"},
		Protocols:      uint8(model.ProtocolIPv4),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
	healthReq.Header.Set("Authorization", "Bearer secret")
	healthRec := httptest.NewRecorder()
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("prefixed health status = %d, body = %s", healthRec.Code, healthRec.Body.String())
	}

	rootReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rootRec := httptest.NewRecorder()
	handler.ServeHTTP(rootRec, rootReq)
	if rootRec.Code != http.StatusNotFound {
		t.Fatalf("unprefixed health status = %d, want 404", rootRec.Code)
	}

	body := `{"protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`
	invokeReq := httptest.NewRequest(http.MethodPost, "/api/v1/invoke", strings.NewReader(body))
	invokeReq.Header.Set("Authorization", "Bearer secret")
	invokeReq.Header.Set("Content-Type", "application/json")
	invokeRec := httptest.NewRecorder()
	handler.ServeHTTP(invokeRec, invokeReq)
	if invokeRec.Code != http.StatusOK {
		t.Fatalf("prefixed invoke status = %d, body = %s", invokeRec.Code, invokeRec.Body.String())
	}
}

func TestEventAgentInvokeWithoutConfiguredToken(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{ID: "edge-event-1", HTTPAuth: testNoHTTPAuth(), Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolIPv4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"action":"invoke","protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", rec.Header().Get("Content-Type"))
	}
	var response eventInvokeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("event invoke must return one JSON document: %v; body=%s", err, rec.Body.String())
	}
	if response.JobID != "job-1" || len(response.Events) != 1 || response.Events[0]["type"] != model.JobErrorTargetBlocked || response.Result["type"] != "summary" {
		t.Fatalf("unexpected function body: %#v", response)
	}
}

func TestEventAgentInvokeRequiresConfiguredBearerToken(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{
		ID:           "edge-event-1",
		HTTPAuth:     testBearerHTTPAuth("secret"),
		Capabilities: []string{"ping"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"action":"invoke","protocol_version":1,"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"}}`

	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("request without token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request with token status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestEventAgentHealthzAction(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{
		ID:           "edge-event-1",
		HTTPAuth:     testNoHTTPAuth(),
		Country:      "CN",
		Region:       "cn-east-3",
		Provider:     "huaweicloud",
		ISP:          "bgp",
		Capabilities: []string{"ping", "dns"},
		Protocols:    1,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{"action":"healthz","protocol_version":1}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content type = %q, body = %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Agent  struct {
			ID                string `json:"id"`
			Region            string `json:"region"`
			Protocols         int    `json:"protocols"`
			MaxConcurrentJobs int    `json:"max_concurrent_jobs"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Agent.ID != "edge-event-1" || body.Agent.Region != "cn-east-3" || body.Agent.Protocols != 1 || body.Agent.MaxConcurrentJobs != agentMaxConcurrentJobs {
		t.Fatalf("unexpected health event: %#v", body)
	}
}

func TestEventAgentRequiresExplicitActionAndCurrentProtocol(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{ID: "edge-event-1", HTTPAuth: testNoHTTPAuth(), Capabilities: []string{"ping"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, body := range map[string]string{
		"missing action":   `{"protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1"}`,
		"job alias":        `{"action":"job","protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1"}`,
		"health no gate":   `{"action":"healthz"}`,
		"invoke old gate":  `{"action":"invoke","protocol_version":0,"id":"job-1","tool":"ping","target":"1.1.1.1"}`,
		"unknown field":    `{"action":"healthz","protocol_version":1,"legacy_mode":true}`,
		"padded action":    `{"action":" healthz ","protocol_version":1}`,
		"uppercase action": `{"action":"HEALTHZ","protocol_version":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestEventResultCollectorEnforcesEventAndByteLimits(t *testing.T) {
	response := eventInvokeResponse{Events: make([]map[string]any, 0)}
	collector := eventResultCollector{response: &response, maxEvents: 1, maxBytes: 1024}
	if err := collector.collect(map[string]any{"type": "message", "message": "first"}); err != nil {
		t.Fatal(err)
	}
	if err := collector.collect(map[string]any{"type": "message", "message": "second"}); err == nil || !strings.Contains(err.Error(), "event limit") {
		t.Fatalf("err = %v, want event limit", err)
	}
	if len(response.Events) != 1 {
		t.Fatalf("events = %#v", response.Events)
	}

	response = eventInvokeResponse{Events: make([]map[string]any, 0)}
	collector = eventResultCollector{response: &response, maxEvents: 10, maxBytes: 32}
	if err := collector.collect(map[string]any{"type": "message", "message": strings.Repeat("x", 64)}); err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("err = %v, want byte limit", err)
	}
	if len(response.Events) != 0 || response.Result != nil {
		t.Fatalf("oversized payload was retained: %#v", response)
	}
}

func TestEventResultCollectorLeavesRoomForResponseEnvelope(t *testing.T) {
	response := eventInvokeResponse{JobID: strings.Repeat("j", agentMaxJobIDLength), Events: make([]map[string]any, 0, agentMaxEventResultEvents)}
	collector := eventResultCollector{
		response:  &response,
		maxEvents: agentMaxEventResultEvents,
		maxBytes:  agentMaxEventResultBytes - agentEventEnvelopeReserve,
	}
	for range agentMaxEventResultEvents {
		if err := collector.collect(map[string]any{"type": "message"}); err != nil {
			t.Fatal(err)
		}
	}
	remaining := collector.maxBytes - collector.bytes
	if err := collector.collect(map[string]any{
		"type":      "summary",
		"exit_code": 0,
		"message":   strings.Repeat("x", remaining-128),
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > agentMaxEventResultBytes {
		t.Fatalf("encoded event response = %d bytes, limit = %d", len(encoded)+1, agentMaxEventResultBytes)
	}
}

func TestEventAgentInit(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{ID: "edge-event-1", HTTPAuth: testBearerHTTPAuth("invoke-secret")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/init", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"ok"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control = %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestEventAgentReturnsValidationErrors(t *testing.T) {
	handler := newEventAgentHandler(config.Agent{ID: "edge-event-1", HTTPAuth: testNoHTTPAuth(), Capabilities: []string{"ping"}, Protocols: uint8(model.ProtocolIPv4)}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{"action":"invoke","protocol_version":1,"id":"job-1","target":"1.1.1.1"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tool") {
		t.Fatalf("unexpected validation response: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRedactHopsUsesTimeout(t *testing.T) {
	result := &model.ToolResult{
		Tool: model.ToolMTR,
		Hops: []model.HopResult{
			{Index: 1, Host: "private-gw", IP: "10.0.0.1"},
			{Index: 2, Host: "edge", IP: "198.51.100.1"},
		},
	}
	redactHops(result, 1)
	if !result.Hops[0].Timeout || result.Hops[0].Host != "" || result.Hops[0].IP != "" {
		t.Fatalf("hidden hop should become timeout: %#v", result.Hops[0])
	}
	if result.Hops[1].Timeout || result.Hops[1].Host == "" {
		t.Fatalf("visible hop should be preserved: %#v", result.Hops[1])
	}

	event := model.StreamEvent{Type: "hop", Hop: &model.HopResult{Index: 1, Host: "private-gw", IP: "10.0.0.1"}}
	redactStreamEvent(&event, model.ToolTraceroute, 1)
	if event.Hop == nil || !event.Hop.Timeout || event.Hop.Host != "" || event.Hop.IP != "" {
		t.Fatalf("hidden stream hop should become timeout: %#v", event.Hop)
	}
}

func TestNextRetryDelayCapsAtFiveMinutes(t *testing.T) {
	if got := nextRetryDelay(0); got != time.Second {
		t.Fatalf("initial retry delay = %s", got)
	}
	if got := nextRetryDelay(time.Second); got != 2*time.Second {
		t.Fatalf("retry delay = %s", got)
	}
	if got := nextRetryDelay(300 * time.Second); got != 300*time.Second {
		t.Fatalf("capped retry delay = %s", got)
	}
}

func TestParseAgentModes(t *testing.T) {
	tests := []struct {
		raw       string
		wantGRPC  bool
		wantHTTP  bool
		wantEvent bool
		wantOK    bool
	}{
		{raw: "grpc", wantGRPC: true, wantOK: true},
		{raw: "http", wantHTTP: true, wantOK: true},
		{raw: "event", wantEvent: true, wantOK: true},
		{raw: "grpc,event", wantGRPC: true, wantEvent: true, wantOK: true},
		{raw: "grpc,http", wantGRPC: true, wantHTTP: true, wantOK: true},
		{raw: "both"},
		{raw: "all"},
		{raw: "http,event"},
		{raw: "bad"},
		{raw: "GRPC"},
		{raw: " grpc"},
		{raw: "grpc,"},
		{raw: "grpc,grpc"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseAgentModes(tt.raw)
			if ok != tt.wantOK || got.grpc != tt.wantGRPC || got.http != tt.wantHTTP || got.event != tt.wantEvent {
				t.Fatalf("parseAgentModes(%q) = %#v, %v", tt.raw, got, ok)
			}
		})
	}
}

func TestValidateServerMessageRequiresStrictUnion(t *testing.T) {
	validJob := &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}
	tests := []struct {
		name string
		msg  *grpcwire.ServerMessage
		ok   bool
	}{
		{name: "job", msg: &grpcwire.ServerMessage{Type: "job", Job: validJob}, ok: true},
		{name: "cancel", msg: &grpcwire.ServerMessage{Type: "cancel", Cancel: &grpcwire.Cancel{JobID: "job-1"}}, ok: true},
		{name: "nil"},
		{name: "unknown", msg: &grpcwire.ServerMessage{Type: "legacy"}},
		{name: "job missing payload", msg: &grpcwire.ServerMessage{Type: "job"}},
		{name: "cancel missing payload", msg: &grpcwire.ServerMessage{Type: "cancel"}},
		{name: "empty cancel id", msg: &grpcwire.ServerMessage{Type: "cancel", Cancel: &grpcwire.Cancel{}}},
		{name: "mixed payload", msg: &grpcwire.ServerMessage{Type: "job", Job: validJob, Cancel: &grpcwire.Cancel{JobID: "job-1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServerMessage(tt.msg)
			if (err == nil) != tt.ok {
				t.Fatalf("validateServerMessage(%#v) error = %v, want success %v", tt.msg, err, tt.ok)
			}
		})
	}
}

func TestAgentTargetVersionsPreferIPv6WhenAvailable(t *testing.T) {
	tests := []struct {
		name      string
		version   model.IPVersion
		protocols model.ProtocolMask
		want      []model.IPVersion
	}{
		{name: "any dual stack", version: model.IPAny, protocols: model.ProtocolAll, want: []model.IPVersion{model.IPv6, model.IPv4}},
		{name: "any ipv4 only", version: model.IPAny, protocols: model.ProtocolIPv4, want: []model.IPVersion{model.IPv4}},
		{name: "any ipv6 only", version: model.IPAny, protocols: model.ProtocolIPv6, want: []model.IPVersion{model.IPv6}},
		{name: "explicit ipv6 unsupported", version: model.IPv6, protocols: model.ProtocolIPv4},
		{name: "explicit ipv4", version: model.IPv4, protocols: model.ProtocolAll, want: []model.IPVersion{model.IPv4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentTargetVersions(tt.version, tt.protocols); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("versions = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestEmptyAgentCapabilitiesEnableNoTools(t *testing.T) {
	if got := agentCapabilities(nil); len(got) != 0 {
		t.Fatalf("capabilities = %#v, want none", got)
	}
	if agentHasCapability(nil, model.ToolPing) {
		t.Fatal("empty capabilities must not enable ping")
	}
}

func TestAgentJobSlotsRejectWithoutBlockingWhenFull(t *testing.T) {
	slots := make(chan struct{}, agentMaxConcurrentJobs)
	for i := 0; i < agentMaxConcurrentJobs; i++ {
		if !tryAcquireAgentJob(slots) {
			t.Fatalf("slot %d should be available", i)
		}
	}
	if tryAcquireAgentJob(slots) {
		t.Fatal("full agent must reject another job")
	}
	releaseAgentJob(slots)
	if !tryAcquireAgentJob(slots) {
		t.Fatal("released slot should become available")
	}
}

func TestHTTPAndEventHandlersShareProcessJobSlots(t *testing.T) {
	jobSlots := make(chan struct{}, 1)
	if !tryAcquireAgentJob(jobSlots) {
		t.Fatal("failed to occupy shared job slot")
	}
	defer releaseAgentJob(jobSlots)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpHandler := newHTTPAgentHandlerWithJobSlots(config.Agent{ID: "edge-http", HTTPAuth: testBearerHTTPAuth("secret"), Capabilities: []string{"ping"}}, log, jobSlots)
	eventHandler := newEventAgentHandlerWithJobSlots(config.Agent{ID: "edge-event", HTTPAuth: testNoHTTPAuth(), Capabilities: []string{"ping"}}, log, jobSlots)

	httpBody := &trackingReadCloser{}
	httpReq := httptest.NewRequest(http.MethodPost, "/invoke", nil)
	httpReq.Body = httpBody
	httpReq.Header.Set("Authorization", "Bearer secret")
	httpRec := httptest.NewRecorder()
	httpHandler.ServeHTTP(httpRec, httpReq)
	if httpRec.Code != http.StatusTooManyRequests {
		t.Fatalf("HTTP status = %d, body = %s", httpRec.Code, httpRec.Body.String())
	}
	if httpBody.reads != 0 {
		t.Fatalf("HTTP handler read full-slot request body %d times", httpBody.reads)
	}

	eventReq := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{"action":"invoke","protocol_version":1,"id":"job-1","tool":"ping","target":"1.1.1.1"}`))
	eventRec := httptest.NewRecorder()
	eventHandler.ServeHTTP(eventRec, eventReq)
	if eventRec.Code != http.StatusTooManyRequests {
		t.Fatalf("event status = %d, body = %s", eventRec.Code, eventRec.Body.String())
	}
	healthReq := httptest.NewRequest(http.MethodPost, "/invoke", strings.NewReader(`{"action":"healthz","protocol_version":1}`))
	healthRec := httptest.NewRecorder()
	eventHandler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("event healthz status = %d, body = %s", healthRec.Code, healthRec.Body.String())
	}
}

type trackingReadCloser struct {
	reads int
}

func (r *trackingReadCloser) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("body must not be read")
}

func (*trackingReadCloser) Close() error { return nil }

type controlledAgentMessageStream struct {
	release   <-chan struct{}
	started   chan struct{}
	startOnce sync.Once
	mu        sync.Mutex
	active    int
	maxActive int
	messages  []*grpcwire.AgentMessage
}

func (s *controlledAgentMessageStream) Send(message *grpcwire.AgentMessage) error {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	s.mu.Lock()
	s.messages = append(s.messages, message)
	s.active--
	s.mu.Unlock()
	return nil
}

func TestAgentGRPCSenderSerializesQueuedMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	stream := &controlledAgentMessageStream{release: release, started: make(chan struct{})}
	sender := newAgentGRPCSender(ctx, cancel, stream, 8, time.Second)
	jobIDs := []string{"job-0", "job-1", "job-2", "job-3"}
	results := make(chan error, len(jobIDs))
	go func() {
		results <- sender.Send(ctx, &grpcwire.AgentMessage{Type: "result", Result: &grpcwire.AgentResult{JobID: jobIDs[0]}})
	}()
	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("first send did not start")
	}
	for i := 1; i < len(jobIDs); i++ {
		jobID := jobIDs[i]
		go func() {
			results <- sender.Send(ctx, &grpcwire.AgentMessage{Type: "result", Result: &grpcwire.AgentResult{JobID: jobID}})
		}()
		deadline := time.Now().Add(time.Second)
		for len(sender.queue) < i {
			if time.Now().After(deadline) {
				t.Fatalf("message %d was not queued", i)
			}
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	for range jobIDs {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.maxActive != 1 {
		t.Fatalf("concurrent stream sends = %d", stream.maxActive)
	}
	if len(stream.messages) != len(jobIDs) {
		t.Fatalf("messages = %d, want %d", len(stream.messages), len(jobIDs))
	}
	for i, message := range stream.messages {
		if message.Result == nil || message.Result.JobID != jobIDs[i] {
			t.Fatalf("message %d = %#v, want job %q", i, message, jobIDs[i])
		}
	}
}

func TestAgentGRPCSenderCancelsSessionOnBlockedSend(t *testing.T) {
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	stream := &controlledAgentMessageStream{release: release, started: make(chan struct{})}
	sender := newAgentGRPCSender(sessionCtx, sessionCancel, stream, 1, 25*time.Millisecond)
	started := time.Now()
	err := sender.Send(sessionCtx, &grpcwire.AgentMessage{Type: "heartbeat", Heartbeat: &grpcwire.Heartbeat{}})
	if !errors.Is(err, errAgentGRPCSendTimeout) {
		close(release)
		t.Fatalf("err = %v, want send timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		close(release)
		t.Fatalf("blocked send returned after %s", elapsed)
	}
	select {
	case <-sessionCtx.Done():
	case <-time.After(time.Second):
		close(release)
		t.Fatal("blocked send did not cancel session")
	}
	close(release)
}

func TestPrepareAgentJobClampsExternallyControlledWork(t *testing.T) {
	spec := grpcwire.JobSpec{
		ProtocolVersion:       grpcwire.AgentProtocolVersion,
		ID:                    "job-1",
		Tool:                  model.ToolMTR,
		Target:                "example.com",
		Args:                  map[string]string{"count": "999999", "max_hops": "999999", "protocol": "icmp"},
		TimeoutSeconds:        999999,
		ProbeTimeoutSeconds:   999999,
		ResolveTimeoutSeconds: 999999,
	}
	p, err := prepareAgentJob(&spec, "edge-1", policy.DefaultPolicies(), []string{"mtr"})
	if err != nil {
		t.Fatal(err)
	}
	runtime := config.DefaultRuntime()
	if spec.Args["count"] != "10" || spec.Args["max_hops"] != "30" {
		t.Fatalf("bounded args = %#v", spec.Args)
	}
	if spec.TimeoutSeconds != runtime.MaxToolTimeoutSec || spec.ProbeTimeoutSeconds != runtime.ProbeStepTimeoutSec || spec.ResolveTimeoutSeconds != runtime.ResolveTimeoutSec {
		t.Fatalf("bounded timeouts = timeout:%d probe:%d resolve:%d", spec.TimeoutSeconds, spec.ProbeTimeoutSeconds, spec.ResolveTimeoutSeconds)
	}
	if p.Timeout != time.Duration(runtime.MaxToolTimeoutSec)*time.Second || p.ProbeTimeout != time.Duration(runtime.ProbeStepTimeoutSec)*time.Second {
		t.Fatalf("policy timeouts = timeout:%s probe:%s", p.Timeout, p.ProbeTimeout)
	}
}

func TestPrepareAgentJobPreservesValuesBelowLocalLimits(t *testing.T) {
	spec := grpcwire.JobSpec{
		ProtocolVersion:       grpcwire.AgentProtocolVersion,
		ID:                    "job-1",
		Tool:                  model.ToolPing,
		Target:                "example.com",
		Args:                  map[string]string{"count": "2"},
		TimeoutSeconds:        2,
		ProbeTimeoutSeconds:   1,
		ResolveTimeoutSeconds: 1,
	}
	_, err := prepareAgentJob(&spec, "edge-1", policy.DefaultPolicies(), []string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Args["count"] != "2" || spec.TimeoutSeconds != 2 || spec.ProbeTimeoutSeconds != 1 || spec.ResolveTimeoutSeconds != 1 {
		t.Fatalf("prepared spec = %#v", spec)
	}
}

func TestPrepareAgentJobRejectsMissingCapability(t *testing.T) {
	spec := grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1", Tool: model.ToolHTTP, Target: "https://example.com"}
	_, err := prepareAgentJob(&spec, "edge-1", policy.DefaultPolicies(), []string{"ping"})
	if !errors.Is(err, errUnsupportedAgentTool) {
		t.Fatalf("err = %v, want unsupported tool", err)
	}
}

func TestPrepareAgentJobRejectsMissingProtocolVersion(t *testing.T) {
	spec := grpcwire.JobSpec{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1"}
	if _, err := prepareAgentJob(&spec, "edge-1", policy.DefaultPolicies(), []string{"ping"}); err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("err = %v, want protocol version rejection", err)
	}
}

func TestPrepareAgentJobRejectsNonCanonicalJobID(t *testing.T) {
	for _, id := range []string{" job-1", "job-1 ", ""} {
		spec := grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: id, Tool: model.ToolPing, Target: "1.1.1.1"}
		if _, err := prepareAgentJob(&spec, "edge-1", policy.DefaultPolicies(), []string{"ping"}); err == nil || !strings.Contains(err.Error(), "job id") {
			t.Fatalf("id %q error = %v", id, err)
		}
	}
}

func TestResolveAgentTargetRejectsResolvedTargetOutsideProtocols(t *testing.T) {
	_, _, err := resolveAgentTarget(context.Background(), model.ToolPing, "example.com", "2606:4700:4700::1111", model.IPv6, false, 1, model.ProtocolIPv4)
	if !errors.Is(err, errUnsupportedAgentProtocol) {
		t.Fatalf("err = %v, want unsupported protocol", err)
	}
}

func TestResolveAgentTargetRejectsMissingProtocols(t *testing.T) {
	_, _, err := resolveAgentTarget(context.Background(), model.ToolPing, "1.1.1.1", "", model.IPAny, false, 1, 0)
	if !errors.Is(err, errUnsupportedAgentProtocol) {
		t.Fatalf("err = %v, want unsupported protocol", err)
	}
}

func TestResolveAgentTargetRejectsAgentResolvedLiteralOutsideProtocols(t *testing.T) {
	_, _, err := resolveAgentTarget(context.Background(), model.ToolPing, "2606:4700:4700::1111", "", model.IPAny, true, 1, model.ProtocolIPv4)
	if !errors.Is(err, errUnsupportedAgentProtocol) {
		t.Fatalf("err = %v, want unsupported protocol", err)
	}
}

func TestResolveAgentTargetRejectsPrivateDomainWithoutResolveOnAgent(t *testing.T) {
	_, _, err := resolveAgentTarget(context.Background(), model.ToolPing, "localhost", "", model.IPAny, false, 1, model.ProtocolIPv4)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v, want blocked localhost resolution", err)
	}
}

func TestResolveAgentTargetDoesNotPreResolveDNSQueries(t *testing.T) {
	for _, target := range []string{"mx-only.invalid", "localhost"} {
		resolvedTarget, version, err := resolveAgentTarget(context.Background(), model.ToolDNS, target, "127.0.0.1", model.IPAny, false, 1, model.ProtocolIPv4)
		if err != nil {
			t.Fatalf("DNS target %q was pre-resolved or blocked: %v", target, err)
		}
		if resolvedTarget != "" || version != model.IPAny {
			t.Fatalf("DNS target %q resolved target=%q version=%d", target, resolvedTarget, version)
		}
	}
	_, _, err := resolveAgentTarget(context.Background(), model.ToolDNS, "mx-only.invalid", "", model.IPv6, false, 1, model.ProtocolIPv4)
	if !errors.Is(err, errUnsupportedAgentProtocol) {
		t.Fatalf("DNS version protocol err=%v, want unsupported protocol", err)
	}
}

func TestResolveAgentTargetPinsLiteralWithoutResolveOnAgent(t *testing.T) {
	resolvedTarget, version, err := resolveAgentTarget(context.Background(), model.ToolPing, "1.1.1.1", "", model.IPAny, false, 1, model.ProtocolIPv4)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedTarget != "1.1.1.1" || version != model.IPv4 {
		t.Fatalf("resolved target = %q version = %d", resolvedTarget, version)
	}
}

func TestResolveAgentTargetFallsBackToIPv4WhenAgentIsIPv4Only(t *testing.T) {
	resolvedTarget, version, err := resolveAgentTarget(context.Background(), model.ToolPing, "1.1.1.1", "", model.IPAny, true, 1, model.ProtocolIPv4)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedTarget != "1.1.1.1" || version != model.IPv4 {
		t.Fatalf("resolved target = %q version = %d", resolvedTarget, version)
	}
}

func TestResolveAgentTargetUsesIPv6WhenAgentIsIPv6Only(t *testing.T) {
	resolvedTarget, version, err := resolveAgentTarget(context.Background(), model.ToolPing, "2606:4700:4700::1111", "", model.IPAny, true, 1, model.ProtocolIPv6)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedTarget != "2606:4700:4700::1111" || version != model.IPv6 {
		t.Fatalf("resolved target = %q version = %d", resolvedTarget, version)
	}
}

func TestSleepContextStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepContext(ctx, time.Minute) {
		t.Fatal("sleep should stop on canceled context")
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled sleep took too long")
	}
}

func TestAgentJobContextBoundsPreflightAndRunnerTogether(t *testing.T) {
	started := time.Now()
	ctx, cancel := agentJobContext(context.Background(), 100*time.Millisecond)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(started.Add(90*time.Millisecond)) || deadline.After(started.Add(200*time.Millisecond)) {
		t.Fatalf("job deadline = %s, ok=%t", deadline, ok)
	}
}

func decodeNDJSONLines(t *testing.T, body string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

func TestStartHTTPAgentServerRequiresHTTPAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := startHTTPAgentServer(ctx, "127.0.0.1:0", config.Agent{ID: "edge-http"}, slog.New(slog.NewTextHandler(io.Discard, nil)), make(chan struct{}, agentMaxConcurrentJobs))
	if err == nil {
		t.Fatal("expected missing http_auth to reject HTTP agent mode")
	}
}

func TestStartEventAgentServerRequiresExplicitHTTPAuthType(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, err := startEventAgentServer(ctx, "127.0.0.1:0", config.Agent{ID: "edge-event"}, slog.New(slog.NewTextHandler(io.Discard, nil)), make(chan struct{}, agentMaxConcurrentJobs))
	if err == nil {
		t.Fatal("expected missing http_auth.type to reject event agent mode")
	}
}

func TestSpeedtestRandomUsesStrictLimitAndSizeCap(t *testing.T) {
	ipLimited := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     1,
		IPBurst:                 1,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	rec := httptest.NewRecorder()
	ipLimited.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected token rejection, status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	ipLimited.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected invalid token to consume IP limit, status = %d, body = %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12", nil)
	req.RemoteAddr = "203.0.113.2:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	ipLimited.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 12 {
		t.Fatalf("body len = %d", rec.Body.Len())
	}

	globalLimited := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 1,
		GlobalBurst:             1,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12", nil)
	req.RemoteAddr = "203.0.113.3:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	globalLimited.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first global-limited request status = %d, body = %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=32", nil)
	req.RemoteAddr = "203.0.113.4:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	globalLimited.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected global limit to apply before size cap, status = %d", rec.Code)
	}

	capped := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=32", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	capped.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected size cap rejection, status = %d", rec.Code)
	}
}

func TestSpeedtestRandomIgnoresForwardedFor(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     1,
		IPBurst:                 1,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/speedtest/random", nil)
	req.RemoteAddr = "203.0.113.50:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.10")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random", nil)
	req.RemoteAddr = "203.0.113.50:4321"
	req.Header.Set("X-Forwarded-For", "198.51.100.11")
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected remote IP limit despite different XFF, status = %d", rec.Code)
	}
}

func TestSpeedtestRandomAllowsHeadWithBearerToken(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodHead, "/speedtest/random", nil)
	req.RemoteAddr = "203.0.113.4:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("head body len = %d", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != "8" {
		t.Fatalf("content length = %q", rec.Header().Get("Content-Length"))
	}
}

func TestSpeedtestRandomPrefersBearerAuthentication(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=8", nil)
	req.RemoteAddr = "203.0.113.60:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 8 {
		t.Fatalf("bearer request status=%d body=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Deprecation") != "" {
		t.Fatalf("bearer authentication marked deprecated: %#v", rec.Header())
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=8&token=secret", nil)
	req.RemoteAddr = "203.0.113.61:1234"
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token overrode invalid bearer, status=%d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=8&token=secret", nil)
	req.RemoteAddr = "203.0.113.62:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token was accepted without bearer, status=%d", rec.Code)
	}
}

func TestSpeedtestRandomRejectsWhenConcurrencySlotsAreFull(t *testing.T) {
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	handler := newSpeedtestHandlerWithSlots(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)), slots)
	req := httptest.NewRequest(http.MethodGet, "/speedtest/random", nil)
	req.RemoteAddr = "203.0.113.70:1234"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("full speedtest slots status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestSpeedtestRandomDisabledWhenMaxBytesIsZero(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                0,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, testBearerHTTPAuth("secret"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=1&token=secret", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestCompactParsedResultRemovesStreamedHops(t *testing.T) {
	result := &model.ToolResult{
		Tool: model.ToolTraceroute,
		Hops: []model.HopResult{{Index: 1}},
	}
	compactParsedResult(result)
	if result.Type != "summary" {
		t.Fatalf("summary type not set: %#v", result)
	}
	if len(result.Hops) != 0 {
		t.Fatalf("hops not removed: %#v", result.Hops)
	}

	mtr := &model.ToolResult{
		Tool: model.ToolMTR,
		Hops: []model.HopResult{{Index: 1, Probes: []model.ProbeResult{{IP: "fc80::1"}}}},
	}
	compactParsedResult(mtr)
	if mtr.Type != "summary" || len(mtr.Hops) != 0 {
		t.Fatalf("mtr summary not compacted: %#v", mtr)
	}

	ping := &model.ToolResult{
		Tool: model.ToolPing,
		Hops: []model.HopResult{{Index: 1}},
	}
	compactParsedResult(ping)
	if ping.Type != "summary" {
		t.Fatalf("summary type not set: %#v", ping)
	}
	if len(ping.Hops) != 1 {
		t.Fatalf("non-hop-streaming tool should keep result shape: %#v", ping.Hops)
	}
}

func TestAgentWriteDeadlinePassesThroughLoggingWriter(t *testing.T) {
	underlying := &deadlineResponseWriter{header: make(http.Header)}
	w := &loggingResponseWriter{ResponseWriter: underlying, status: http.StatusOK}
	controller := http.NewResponseController(w)
	started := time.Now()
	if err := setAgentWriteDeadline(controller); err != nil {
		t.Fatal(err)
	}
	clearAgentWriteDeadline(controller)
	if len(underlying.deadlines) != 2 {
		t.Fatalf("deadlines = %#v", underlying.deadlines)
	}
	if underlying.deadlines[0].Before(started.Add(agentHTTPWriteTimeout-time.Second)) || underlying.deadlines[0].After(started.Add(agentHTTPWriteTimeout+time.Second)) {
		t.Fatalf("write deadline = %s", underlying.deadlines[0])
	}
	if !underlying.deadlines[1].IsZero() {
		t.Fatalf("write deadline was not cleared: %s", underlying.deadlines[1])
	}
}

func TestRefreshingDeadlineWriterRenewsEachWrite(t *testing.T) {
	underlying := &deadlineResponseWriter{header: make(http.Header)}
	writer := &refreshingDeadlineWriter{writer: underlying, controller: http.NewResponseController(underlying)}
	if _, err := writer.Write([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if len(underlying.deadlines) != 2 || underlying.body.String() != "onetwo" {
		t.Fatalf("deadlines=%#v body=%q", underlying.deadlines, underlying.body.String())
	}
}

type deadlineResponseWriter struct {
	header    http.Header
	deadlines []time.Time
	body      strings.Builder
}

func (w *deadlineResponseWriter) Header() http.Header { return w.header }
func (w *deadlineResponseWriter) WriteHeader(int)     {}
func (w *deadlineResponseWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}
func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
