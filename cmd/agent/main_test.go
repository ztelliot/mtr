package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func TestHTTPAgentInvokeStreamsResultEvents(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{ID: "edge-fc-1", HTTPToken: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := `{"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`
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
	if lines[0]["type"] != "message" || lines[0]["message"] != "target_blocked" {
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

func TestHTTPAgentAllowsPinnedPathTools(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:            "edge-http-1",
		HTTPToken:     "secret",
		Country:       "CN",
		Region:        "edge",
		Provider:      "kubernetes",
		ISP:           "bgp",
		Capabilities:  []string{"ping", "dns"},
		Protocols:     3,
		HideFirstHops: 2,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tool := range []model.Tool{model.ToolTraceroute, model.ToolMTR} {
		body := `{"id":"job-1","tool":"` + string(tool) + `","target":"127.0.0.1","ip_version":4,"timeout_seconds":1}`
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

func TestHTTPAgentHealthIncludesVersion(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:            "edge-http-1",
		HTTPToken:     "secret",
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
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Version struct {
			Version string `json:"version"`
		} `json:"version"`
		Agent struct {
			ID           string       `json:"id"`
			Country      string       `json:"country"`
			Region       string       `json:"region"`
			Provider     string       `json:"provider"`
			ISP          string       `json:"isp"`
			Capabilities []model.Tool `json:"capabilities"`
			Protocols    int          `json:"protocols"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version.Version == "" || body.Agent.ID != "edge-http-1" || body.Agent.Country != "CN" || body.Agent.Protocols != 3 || len(body.Agent.Capabilities) != 2 {
		t.Fatalf("unexpected health response: %#v", body)
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
}

func TestHTTPAgentPathPrefix(t *testing.T) {
	handler := newHTTPAgentHandler(config.Agent{
		ID:             "edge-http-1",
		HTTPToken:      "secret",
		HTTPPathPrefix: "/api/v1",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	healthReq := httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil)
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

	body := `{"id":"job-1","tool":"ping","target":"127.0.0.1","ip_version":4,"args":{"count":"1"},"timeout_seconds":1}`
	invokeReq := httptest.NewRequest(http.MethodPost, "/api/v1/invoke", strings.NewReader(body))
	invokeReq.Header.Set("Authorization", "Bearer secret")
	invokeReq.Header.Set("Content-Type", "application/json")
	invokeRec := httptest.NewRecorder()
	handler.ServeHTTP(invokeRec, invokeReq)
	if invokeRec.Code != http.StatusOK {
		t.Fatalf("prefixed invoke status = %d, body = %s", invokeRec.Code, invokeRec.Body.String())
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
		raw      string
		wantGRPC bool
		wantHTTP bool
		wantOK   bool
	}{
		{raw: "grpc", wantGRPC: true, wantOK: true},
		{raw: "http", wantHTTP: true, wantOK: true},
		{raw: "grpc,http", wantGRPC: true, wantHTTP: true, wantOK: true},
		{raw: "both", wantGRPC: true, wantHTTP: true, wantOK: true},
		{raw: "bad"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, ok := parseAgentModes(tt.raw)
			if ok != tt.wantOK || got.grpc != tt.wantGRPC || got.http != tt.wantHTTP {
				t.Fatalf("parseAgentModes(%q) = %#v, %v", tt.raw, got, ok)
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

func TestSpeedtestRandomUsesStrictLimitAndSizeCap(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 1,
		GlobalBurst:             1,
		IPRequestsPerMinute:     1,
		IPBurst:                 1,
	}, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected token rejection, status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=12&token=secret", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 12 {
		t.Fatalf("body len = %d", rec.Body.Len())
	}

	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=32&token=secret", nil)
	req.RemoteAddr = "203.0.113.2:1234"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
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
	}, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	req = httptest.NewRequest(http.MethodGet, "/speedtest/random?bytes=32&token=secret", nil)
	req.RemoteAddr = "203.0.113.3:1234"
	rec = httptest.NewRecorder()
	capped.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected size cap rejection, status = %d", rec.Code)
	}
}

func TestSpeedtestRandomAllowsHeadWithQueryToken(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                16,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodHead, "/speedtest/random?token=secret", nil)
	req.RemoteAddr = "203.0.113.4:1234"
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

func TestSpeedtestRandomDisabledWhenMaxBytesIsZero(t *testing.T) {
	handler := newSpeedtestHandler(config.Speedtest{
		DefaultBytes:            8,
		MaxBytes:                0,
		GlobalRequestsPerMinute: 100,
		GlobalBurst:             100,
		IPRequestsPerMinute:     100,
		IPBurst:                 100,
	}, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))

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
