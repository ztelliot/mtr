package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
)

func TestStreamJobEventsSendsHistoryAndLive(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "http://geoip.test", slog.Default())
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.CreateJob(ctx, model.Job{
		ID:        "job-1",
		Tool:      model.ToolPing,
		Target:    "1.1.1.1",
		Status:    model.JobRunning,
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	history := model.JobEvent{
		ID:     "event-history",
		JobID:  "job-1",
		Stream: "progress",
		Event:  &model.StreamEvent{Type: "progress", Message: "started"},
	}
	if err := st.AddJobEvent(ctx, history); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/stream", nil).WithContext(reqCtx)
	rec := newSSERecorder()
	go handler.ServeHTTP(rec, req)

	deadline := time.After(2 * time.Second)
	historyEvent := waitSSEEvent(t, rec, "event-history", deadline)
	if historyEvent.Event != "progress" {
		t.Fatalf("history SSE event type = %q", historyEvent.Event)
	}

	live := model.JobEvent{
		ID:      "event-live",
		JobID:   "job-1",
		AgentID: "edge-1",
		Stream:  "metric",
		Event:   &model.StreamEvent{Type: "metric", Metric: map[string]any{"latency_ms": 1.2, "error": "lookup failed"}},
	}
	hub.PublishEvent(live)
	liveEvent := waitSSEEvent(t, rec, "event-live", deadline)
	if liveEvent.Event != "metric" {
		t.Fatalf("live SSE event type = %q", liveEvent.Event)
	}

	for _, item := range []sseEvent{historyEvent, liveEvent} {
		var payload map[string]any
		if err := json.Unmarshal([]byte(item.Data), &payload); err != nil {
			t.Fatalf("invalid sse data json: %v", err)
		}
		for _, forbidden := range []string{"id", "job_id", "event", "created_at", "stream", "parsed"} {
			if _, ok := payload[forbidden]; ok {
				t.Fatalf("SSE data should only expose event payload fields, got %q in %#v", forbidden, payload)
			}
		}
		if payload["type"] != item.Event {
			t.Fatalf("SSE data type = %#v, event = %q, payload = %#v", payload["type"], item.Event, payload)
		}
		if metric, ok := payload["metric"].(map[string]any); ok {
			if _, ok := metric["error"]; ok {
				t.Fatalf("SSE metric should not expose error: %#v", payload)
			}
		}
	}
	var livePayload map[string]any
	if err := json.Unmarshal([]byte(liveEvent.Data), &livePayload); err != nil {
		t.Fatalf("invalid live sse data json: %v", err)
	}
	if livePayload["agent_id"] != "edge-1" {
		t.Fatalf("agent-level SSE event should include agent_id: %#v", livePayload)
	}
}

func TestStreamJobEventsRejectsWhenCapacityIsExhausted(t *testing.T) {
	server := &Server{streamSlots: make(chan struct{}, 1)}
	server.streamSlots <- struct{}{}
	recorder := httptest.NewRecorder()
	server.streamJobEvents(recorder, httptest.NewRequest(http.MethodGet, "/v1/jobs/job-1/stream", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestGeoIPHTTPClientDisablesProxyAndRedirects(t *testing.T) {
	client := newGeoIPHTTPClient()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("unexpected GeoIP transport: %#v", client.Transport)
	}
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v", err)
	}
}

func upsertTestAgent(t *testing.T, ctx context.Context, st store.Store, id string, labels []string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           id,
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if len(labels) > 0 {
		upsertTestAgentConfigLabels(t, ctx, st, id, labels)
	}
}

func upsertTestAgentConfigLabels(t *testing.T, ctx context.Context, st store.Store, id string, labels []string) {
	t.Helper()
	cfg, err := st.GetAgentConfig(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Labels = labels
	if err := st.UpsertAgentConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestJobEventJSONUsesUnifiedEventEnvelope(t *testing.T) {
	event := model.JobEvent{
		ID:      "event-1",
		JobID:   "job-1",
		AgentID: "edge-1",
		Stream:  "parsed",
		Parsed: &model.ToolResult{
			Type:      "summary",
			Tool:      model.ToolPing,
			Target:    "1.0.0.1",
			IPVersion: model.IPv4,
			ExitCode:  0,
			Summary: map[string]any{
				"packets_received":    float64(10),
				"packets_transmitted": float64(10),
				"error":               "lookup failed",
			},
		},
		CreatedAt: time.Unix(1, 0).UTC(),
	}

	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["stream"]; ok {
		t.Fatalf("stream should not be exposed: %s", b)
	}
	if _, ok := got["parsed"]; ok {
		t.Fatalf("parsed should not be exposed: %s", b)
	}
	if _, ok := got["exit_code"]; ok {
		t.Fatalf("outer exit_code should not be exposed: %s", b)
	}
	payload, ok := got["event"].(map[string]any)
	if !ok {
		t.Fatalf("event payload missing: %#v", got)
	}
	metric, ok := payload["metric"].(map[string]any)
	if payload["type"] != "summary" || !ok || metric["packets_received"] != float64(10) {
		t.Fatalf("summary payload metric missing: %#v", payload)
	}
	if _, ok := payload["packets_received"]; ok {
		t.Fatalf("summary metric should not be flattened: %#v", payload)
	}
	if _, ok := payload["tool"]; ok {
		t.Fatalf("summary tool should not be exposed: %#v", payload)
	}
	if _, ok := payload["target"]; ok {
		t.Fatalf("summary target should not be exposed: %#v", payload)
	}
	if _, ok := payload["summary"]; ok {
		t.Fatalf("nested summary should not be exposed: %#v", payload)
	}
	if _, ok := metric["error"]; ok {
		t.Fatalf("summary metric should not expose error: %#v", payload)
	}
}

func TestHealthIncludesVersion(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true}})

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
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version.Version == "" {
		t.Fatalf("unexpected health response: %#v", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("version without auth status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/version", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("version with auth status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var versionBody struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &versionBody); err != nil {
		t.Fatal(err)
	}
	if versionBody.Version == "" {
		t.Fatalf("unexpected version response: %#v", versionBody)
	}
}

func TestWriteErrorDoesNotExposeInternalDetails(t *testing.T) {
	const sensitive = "failed to connect to postgres://db-user:db-password@db.internal:5432/mtr"
	tests := []struct {
		name       string
		status     int
		message    string
		wantPublic string
	}{
		{name: "internal server error", status: http.StatusInternalServerError, message: sensitive, wantPublic: "internal server error"},
		{name: "bad gateway", status: http.StatusBadGateway, message: sensitive, wantPublic: "bad gateway"},
		{name: "client error", status: http.StatusBadRequest, message: "invalid target", wantPublic: "invalid target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeError(rec, tt.status, tt.message)

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["error"] != tt.wantPublic {
				t.Fatalf("error = %q, want %q", body["error"], tt.wantPublic)
			}
			if tt.status >= http.StatusInternalServerError && strings.Contains(rec.Body.String(), "db-password") {
				t.Fatalf("response exposed internal details: %s", rec.Body.String())
			}
		})
	}
}

func TestAuthFailureCountsAgainstRequestLimit(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1, Burst: 1},
	}), nil, "", slog.Default(), TokenConfig{Token: "secret", Scope: TokenScope{All: true}})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("first status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.RemoteAddr = "203.0.113.10:4321"
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("valid retry should be rate limited, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthorizationHeaderRequiresBearerScheme(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "secret", Scope: TokenScope{All: true}})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("raw token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/permissions?access_token=secret", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query access token must be rejected, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestClientIPIgnoresForwardedHeadersFromUntrustedPeer(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")
	req.Header.Set("X-Real-IP", "198.51.100.21")

	if got := resolver.Resolve(req); got != "203.0.113.10" {
		t.Fatalf("client ip = %q, want remote peer", got)
	}
}

func TestClientIPUsesForwardedForFromTrustedProxy(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.20")

	if got := resolver.Resolve(req); got != "198.51.100.20" {
		t.Fatalf("client ip = %q, want forwarded client", got)
	}
}

func TestClientIPWalksForwardedForFromRight(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8", "172.16.0.0/12"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "192.0.2.99, 198.51.100.20, 172.16.1.5")

	if got := resolver.Resolve(req); got != "198.51.100.20" {
		t.Fatalf("client ip = %q, want nearest untrusted forwarded address", got)
	}
}

func TestClientIPUsesXRealIPFromTrustedProxy(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Real-IP", "198.51.100.21")

	if got := resolver.Resolve(req); got != "198.51.100.21" {
		t.Fatalf("client ip = %q, want x-real-ip client", got)
	}
}

func TestClientIPIgnoresConfiguredHeaderFromUntrustedPeer(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, []string{"Eo-Connecting-Ip"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Eo-Connecting-Ip", "198.51.100.22")

	if got := resolver.Resolve(req); got != "203.0.113.10" {
		t.Fatalf("client ip = %q, want untrusted remote peer", got)
	}
}

func TestClientIPUsesConfiguredSingleIPHeaderOrder(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, []string{"Cf-Connecting-Ip", "Eo-Connecting-Ip"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("Eo-Connecting-Ip", "198.51.100.22")
	req.Header.Set("Cf-Connecting-Ip", "198.51.100.23")

	if got := resolver.Resolve(req); got != "198.51.100.23" {
		t.Fatalf("client ip = %q, want first configured single-ip header", got)
	}
}

func TestClientIPHeadersExcludeXRealIP(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, []string{"X-Real-IP", "Eo-Connecting-Ip"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Real-IP", "198.51.100.21")
	req.Header.Set("Eo-Connecting-Ip", "198.51.100.22")

	if got := resolver.Resolve(req); got != "198.51.100.22" {
		t.Fatalf("client ip = %q, want configured header after ignoring x-real-ip", got)
	}
}

func TestNewWithOptionsRejectsInvalidTrustedProxy(t *testing.T) {
	st := store.NewTestMemory()
	_, err := NewWithOptions(st, policy.DefaultPolicies(), nil, nil, "", slog.Default(), Options{
		TrustedProxies: []string{"not-an-ip"},
	})
	if err == nil {
		t.Fatal("expected invalid trusted proxy to be rejected")
	}
}

func TestTokenPermissionsRestrictToolsArgsAgentsAndSchedules(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-1", Capabilities: []model.Tool{model.ToolPing, model.ToolHTTP, model.ToolPort}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-2", Capabilities: []model.Tool{model.ToolPing, model.ToolHTTP, model.ToolPort}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	policies := policy.FromConfig(map[string]config.Policy{
		"port": {Enabled: true, AllowedArgs: map[string]string{"port": "1-2000"}},
	}, config.DefaultRuntime())
	headOnly := map[string]string{"method": "HEAD"}
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "token-1",
		Scope: TokenScope{
			Agents: []string{"edge-1"},
			Tools: map[model.Tool]ToolScope{
				model.ToolPing: {},
				model.ToolHTTP: {AllowedArgs: headOnly},
				model.ToolPort: {AllowedArgs: map[string]string{"port": "1-1000"}},
			},
		},
	}, TokenConfig{Token: "token-2", Scope: TokenScope{All: true}})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var perms PermissionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &perms); err != nil {
		t.Fatal(err)
	}
	if _, ok := perms.Tools[model.ToolTraceroute]; ok || perms.ScheduleAccess != ScheduleAccessNone {
		t.Fatalf("restricted token permissions = %#v", perms)
	}
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "HEAD" || perms.Tools[model.ToolPort].AllowedArgs["port"] != "1-1000" {
		t.Fatalf("restricted token permissions = %#v", perms)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agents status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agentViews []model.AgentView
	if err := json.Unmarshal(rec.Body.Bytes(), &agentViews); err != nil {
		t.Fatal(err)
	}
	if len(agentViews) != 1 || agentViews[0].ID != "edge-1" || agentViews[0].Tools[model.ToolHTTP].AllowedArgs["method"] != "HEAD" {
		t.Fatalf("restricted token agents = %#v", agentViews)
	}

	body := `{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ping status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	children, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].AgentID != "edge-1" {
		t.Fatalf("permission should filter fanout agents: %#v", children)
	}

	body = `{"tool":"http","target":"https://1.1.1.1","args":{"method":"GET"},"resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET should be forbidden, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = `{"tool":"port","target":"1.1.1.1","args":{"port":"443"},"resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("allowed port should be accepted, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = `{"tool":"port","target":"1.1.1.1","args":{"port":"1500"},"resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disallowed port should be forbidden, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = `{"tool":"traceroute","target":"1.1.1.1","agent_id":"edge-1","resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("traceroute should be forbidden, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/schedules", nil)
	req.Header.Set("Authorization", "Bearer token-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("schedules should be forbidden, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/schedules", nil)
	req.Header.Set("Authorization", "Bearer token-2")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("all token should view schedules, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestPermissionsResponseShowsEffectiveAllowedArgsForInheritedTokenScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{ID: "edge-1", Capabilities: []model.Tool{model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	policies := policy.FromConfig(map[string]config.Policy{
		"http": {Enabled: true, AllowedArgs: map[string]string{"method": "GET", "http_version": "1.1"}},
	}, config.DefaultRuntime())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "reader",
		Scope: TokenScope{
			Tools: map[model.Tool]ToolScope{
				model.ToolHTTP: {},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var perms PermissionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &perms); err != nil {
		t.Fatal(err)
	}
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "GET" || perms.Tools[model.ToolHTTP].AllowedArgs["http_version"] != "1.1" {
		t.Fatalf("inherited allowed args should show effective policy, got %#v", perms.Tools[model.ToolHTTP].AllowedArgs)
	}

	body := `{"tool":"http","target":"https://1.1.1.1","args":{"method":"POST"},"resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("policy should still reject POST, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = `{"tool":"http","target":"https://1.1.1.1","args":{"method":"GET"},"resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("policy should allow inherited GET, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleReadPermissionCanReadHistoryButNotCreate(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	now := time.Now().UTC()
	sched := model.ScheduledJob{
		ID:             "sched-1",
		Revision:       1,
		Name:           "readonly",
		Enabled:        true,
		Tool:           model.ToolTraceroute,
		Target:         "example.com",
		CronExpression: "* * * * *",
		NextRunAt:      now,
		ScheduleTargets: []model.ScheduleTarget{{
			Label:      "agent",
			AgentScope: model.ScheduleAgentScope{AllowedAgentIDs: []string{"edge-1"}},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := st.CreateScheduledJob(ctx, sched); err != nil {
		t.Fatal(err)
	}
	historyJob := model.Job{
		ID:                "history-1",
		ScheduledID:       sched.ID,
		ScheduledRevision: sched.Revision,
		Tool:              model.ToolTraceroute,
		Target:            "example.com",
		AgentID:           "edge-2",
		Status:            model.JobSucceeded,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := st.CreateJob(ctx, historyJob); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "reader",
		Scope: TokenScope{
			ScheduleAccess: ScheduleAccessRead,
			Agents:         []string{"edge-1"},
			Tools:          map[model.Tool]ToolScope{model.ToolPing: {}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var perms PermissionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &perms); err != nil {
		t.Fatal(err)
	}
	if perms.ScheduleAccess != ScheduleAccessRead {
		t.Fatalf("permissions = %#v", perms)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","cron_expression":"* * * * *","schedule_targets":[{"label":"agent"}]}`))
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token should not create schedules, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req = httptest.NewRequest(method, "/v1/schedules/"+sched.ID, strings.NewReader(`{"tool":"ping","target":"1.1.1.1","cron_expression":"* * * * *","schedule_targets":[{"label":"agent"}]}`))
		req.Header.Set("Authorization", "Bearer reader")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read-only token should not %s schedules, status = %d, body = %s", method, rec.Code, rec.Body.String())
		}
	}

	for _, path := range []string{"/v1/schedules", "/v1/schedules/" + sched.ID} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer reader")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("read-only schedule GET %s status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
			t.Fatalf("schedule GET %s exposed internal agent scope: %s", path, rec.Body.String())
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/schedules/"+sched.ID+"/history", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var history []model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].ID != historyJob.ID || history[0].Tool != model.ToolTraceroute || history[0].AgentID != "edge-2" {
		t.Fatalf("read-only token should see all schedule history: %#v", history)
	}
}

func TestCreateScheduleScopesGroupTargetToAllowedAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-1", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-2", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
		upsertTestAgentConfigLabels(t, ctx, st, agent.ID, []string{"blue"})
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "restricted",
		Scope: TokenScope{
			ScheduleAccess: ScheduleAccessWrite,
			Agents:         []string{"edge-1"},
			Tools:          map[model.Tool]ToolScope{model.ToolPing: {}},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"blue"}]}`))
	req.Header.Set("Authorization", "Bearer restricted")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
		t.Fatalf("create schedule exposed internal agent scope: %s", rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if len(sched.ScheduleTargets) != 1 || sched.ScheduleTargets[0].Label != "blue" {
		t.Fatalf("schedule targets = %#v", sched.ScheduleTargets)
	}
	storedSchedule, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(storedSchedule.ScheduleTargets[0].AgentScope, model.ScheduleAgentScope{AllowedAgentIDs: []string{"edge-1"}}) {
		t.Fatalf("stored agent scope = %#v", storedSchedule.ScheduleTargets[0].AgentScope)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"id:edge-1"}]}`))
	req.Header.Set("Authorization", "Bearer restricted")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("explicit id target status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateSchedulePersistsDynamicTagAndDenyScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-1", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-2", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-3", Capabilities: []model.Tool{model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	upsertTestAgentConfigLabels(t, ctx, st, "edge-1", []string{"blue"})
	upsertTestAgentConfigLabels(t, ctx, st, "edge-2", []string{"blue", "blocked"})
	upsertTestAgentConfigLabels(t, ctx, st, "edge-3", []string{"blue"})
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "tagged",
		Scope: TokenScope{
			ScheduleAccess: ScheduleAccessWrite,
			AgentTags:      []string{"blue"},
			DeniedTags:     []string{"blocked"},
			Tools:          map[model.Tool]ToolScope{model.ToolPing: {}},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"blue"}]}`))
	req.Header.Set("Authorization", "Bearer tagged")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
		t.Fatalf("create schedule exposed internal agent scope: %s", rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if len(sched.ScheduleTargets) != 1 {
		t.Fatalf("schedule targets = %#v", sched.ScheduleTargets)
	}
	storedSchedule, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantScope := model.ScheduleAgentScope{AllowedLabels: []string{"blue"}, DeniedLabels: []string{"blocked"}}
	if !reflect.DeepEqual(storedSchedule.ScheduleTargets[0].AgentScope, wantScope) {
		t.Fatalf("dynamic tag scope = %#v, want %#v", storedSchedule.ScheduleTargets[0].AgentScope, wantScope)
	}
}

func TestServerManagedLabelsGrantTokenScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-spoof",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "tagged",
		Scope: TokenScope{
			AgentTags: []string{"blue"},
			Tools:     map[model.Tool]ToolScope{model.ToolPing: {}},
		},
	})

	body, _ := json.Marshal(model.CreateJobRequest{Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-spoof"})
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tagged")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent without server-managed label should not satisfy token scope, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	upsertTestAgentConfigLabels(t, ctx, st, "edge-spoof", []string{"blue"})
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tagged")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("server-managed label should satisfy token scope, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleAndHistory(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-1", nil)
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","args":{"count":"1"},"cron_expression":"*/5 * * * *","schedule_targets":[{"label":"agent"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if sched.ID == "" || !sched.Enabled || sched.CronExpression != "*/5 * * * *" {
		t.Fatalf("unexpected schedule: %#v", sched)
	}
	if sched.Revision != 1 {
		t.Fatalf("schedule revision = %d, want 1", sched.Revision)
	}

	now := time.Now().UTC()
	job := model.Job{
		ID:                "job-history-1",
		ScheduledID:       sched.ID,
		ScheduledRevision: sched.Revision,
		Tool:              model.ToolPing,
		Target:            "1.1.1.1",
		Args:              sched.Args,
		IPVersion:         sched.IPVersion,
		ResolveOnAgent:    sched.ResolveOnAgent,
		Status:            model.JobSucceeded,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := st.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if err := st.AddJobEvent(context.Background(), model.JobEvent{
		ID:        "event-summary",
		JobID:     job.ID,
		Stream:    "summary",
		Parsed:    &model.ToolResult{Tool: model.ToolPing, Target: "1.1.1.1", ExitCode: 0, Summary: map[string]any{"rtt_avg_ms": 4.5}},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddJobEvent(context.Background(), model.JobEvent{
		ID:        "event-metric",
		JobID:     job.ID,
		Stream:    "metric",
		Event:     &model.StreamEvent{Type: "metric", Metric: map[string]any{"latency_ms": 4.2}},
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	emptyJob := model.Job{
		ID:                "job-history-empty",
		ScheduledID:       sched.ID,
		ScheduledRevision: sched.Revision,
		Tool:              model.ToolPing,
		Target:            "1.1.1.1",
		Args:              sched.Args,
		IPVersion:         sched.IPVersion,
		ResolveOnAgent:    sched.ResolveOnAgent,
		Status:            model.JobFailed,
		Error:             "runner unavailable",
		CreatedAt:         now.Add(time.Second),
		UpdatedAt:         now.Add(time.Second),
	}
	if err := st.CreateJob(context.Background(), emptyJob); err != nil {
		t.Fatal(err)
	}
	foreignJob := model.Job{
		ID:                "job-history-foreign",
		ScheduledID:       sched.ID,
		ScheduledRevision: sched.Revision + 1,
		Tool:              model.ToolDNS,
		Target:            "example.com",
		Args:              map[string]string{"type": "A"},
		Status:            model.JobSucceeded,
		CreatedAt:         now.Add(2 * time.Second),
		UpdatedAt:         now.Add(2 * time.Second),
	}
	if err := st.CreateJob(context.Background(), foreignJob); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/schedules/"+sched.ID+"/history", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var history []model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].ScheduledID != sched.ID {
		t.Fatalf("unexpected history: %#v", history)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/schedules/"+sched.ID+"/summary", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var summary []model.JobEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary) != 2 {
		t.Fatalf("unexpected summary history: %#v", summary)
	}
	summaryByJobID := map[string]model.JobEvent{}
	for _, event := range summary {
		summaryByJobID[event.JobID] = event
	}
	if summaryByJobID[job.ID].ID != "event-summary" {
		t.Fatalf("expected one summary event for job with events, got %#v", summaryByJobID[job.ID])
	}
	if synthetic := summaryByJobID[emptyJob.ID]; synthetic.ID != emptyJob.ID+"-summary" || synthetic.Event == nil || synthetic.Event.Type != "summary" {
		t.Fatalf("expected synthetic summary event for job without events, got %#v", synthetic)
	}
}

func TestUpdateAndDeleteSchedule(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-1", nil)
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"cron_expression":"*/5 * * * *","schedule_targets":[{"label":"agent"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
		t.Fatalf("create schedule exposed internal agent scope: %s", rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"edited","enabled":false,"tool":"ping","target":"8.8.8.8","args":{"protocol":"icmp"},"cron_expression":"0 * * * *","schedule_targets":[{"label":"agent"}]}`
	req = httptest.NewRequest(http.MethodPut, "/v1/schedules/"+sched.ID, strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
		t.Fatalf("update schedule exposed internal agent scope: %s", rec.Body.String())
	}
	var updated model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.ID != sched.ID || updated.Name != "edited" || updated.Enabled || updated.Target != "8.8.8.8" || updated.CronExpression != "0 * * * *" || updated.Revision != sched.Revision+1 || updated.CreatedAt.IsZero() {
		t.Fatalf("unexpected updated schedule: %#v", updated)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/schedules/"+sched.ID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/schedules/"+sched.ID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted schedule should 404, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateScheduleCronRefreshesNextRunWithoutBumpingRevision(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-1", nil)
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"cron_expression":"* * * * *","schedule_targets":[{"label":"agent"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"cron_expression":"0 0 1 1 *","schedule_targets":[{"label":"agent"}]}`
	req = httptest.NewRequest(http.MethodPut, "/v1/schedules/"+sched.ID, strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var updated model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Revision != sched.Revision {
		t.Fatalf("cron-only update bumped revision: got %d, want %d", updated.Revision, sched.Revision)
	}
	if updated.CronExpression != "0 0 1 1 *" {
		t.Fatalf("cron expression not updated: %#v", updated)
	}
	if updated.NextRunAt.Before(sched.NextRunAt) || updated.NextRunAt.Equal(sched.NextRunAt) {
		t.Fatalf("cron-only update should refresh next run time: before=%s after=%s", sched.NextRunAt, updated.NextRunAt)
	}
}

func TestCreateScheduleAcceptsMultipleLabelTargetsWithOneCron(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"15,45 * * * *","schedule_targets":[{"label":"blue"},{"label":"red"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if sched.CronExpression != "15,45 * * * *" || len(sched.ScheduleTargets) != 2 {
		t.Fatalf("unexpected schedule targets: %#v", sched)
	}
	if sched.ScheduleTargets[0].Label != "blue" || sched.ScheduleTargets[1].Label != "red" {
		t.Fatalf("unexpected schedule target payload: %#v", sched.ScheduleTargets)
	}
}

func TestCreateScheduleRejectsMissingTargets(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"* * * * *"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["error"] != "schedule_targets is required" {
		t.Fatalf("error = %#v", response)
	}
}

func TestScheduleTargetsRequireCanonicalBoundedSelectors(t *testing.T) {
	if _, err := scheduleTargetsFromRequest(model.CreateScheduledJobRequest{
		ScheduleTargets: []model.ScheduleTargetRequest{{Label: " blue "}},
	}); err == nil || !strings.Contains(err.Error(), "surrounding whitespace") {
		t.Fatalf("padded label error = %v", err)
	}
	targets := make([]model.ScheduleTargetRequest, model.MaxFanoutTargets+1)
	for index := range targets {
		targets[index].Label = fmt.Sprintf("label-%02d", index)
	}
	if _, err := scheduleTargetsFromRequest(model.CreateScheduledJobRequest{ScheduleTargets: targets}); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("selector limit error = %v", err)
	}
}

func TestCreateScheduleRejectsAggregateFanoutBeyondHardLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	for index := 0; index <= model.MaxFanoutTargets; index++ {
		label := "blue"
		if index >= model.MaxFanoutTargets/2 {
			label = "green"
		}
		upsertTestAgent(t, ctx, st, fmt.Sprintf("schedule-edge-%02d", index), []string{label})
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())
	body := `{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"blue"},{"label":"green"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "schedule fanout target limit exceeded") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleRejectsInvalidCronExpression(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","cron_expression":"61 * * * *","schedule_targets":[{"label":"agent"}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response["error"], "cron_expression must be a valid 5-field crontab expression:") {
		t.Fatalf("error = %#v", response)
	}
}

func TestCreateJobNormalizesServerOwnedArgsAndResolveLocation(t *testing.T) {
	st := store.NewTestMemory()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-1",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","agent_id":"edge-1","args":{"count":"1","protocol":"icmp"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var job model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Args["count"] != "10" {
		t.Fatalf("expected server-owned count to be fixed at 10: %#v", job.Args)
	}
	if job.ResolvedTarget != "1.1.1.1" {
		t.Fatalf("expected server-side resolution to populate resolved_target: %#v", job)
	}

	body = `{"tool":"ping","target":"localhost","agent_id":"edge-1","resolve_on_agent":true}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("resolve_on_agent should defer hostname DNS to agent, status = %d, body = %s", rec.Code, rec.Body.String())
	}
	job = model.Job{}
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if !job.ResolveOnAgent || job.ResolvedTarget != "" {
		t.Fatalf("agent-side resolution should keep target unresolved for dispatch: %#v", job)
	}

	body = `{"tool":"ping","target":"localhost"}`
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("server-side DNS should reject localhost, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateJobRequiresPinnedAgentForRouteTools(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	for _, tool := range []model.Tool{model.ToolTraceroute, model.ToolMTR} {
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"`+string(tool)+`","target":"1.1.1.1","resolve_on_agent":true}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s without agent status = %d, body = %s", tool, rec.Code, rec.Body.String())
		}
		var response map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response["error"] != string(tool)+" requires agent_id" {
			t.Fatalf("%s error = %#v", tool, response)
		}
	}
}

func TestCreateJobRejectsUnknownOrOfflinePinnedAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-offline",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOffline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	for _, tc := range []struct {
		name  string
		agent string
		want  string
	}{
		{name: "unknown", agent: "missing-agent", want: "agent not found"},
		{name: "offline", agent: "edge-offline", want: "agent is offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","agent_id":"`+tc.agent+`","resolve_on_agent":true}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["error"] != tc.want {
				t.Fatalf("error = %#v, want %q", response, tc.want)
			}
		})
	}
}

func TestCreateJobWithEmptyAgentIDFansOutToMatchingAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-v4", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv4, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-v6", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv6, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-dns", Capabilities: []model.Tool{model.ToolDNS}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-offline", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOffline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"2606:4700:4700::1111","resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	if parent.ID == "" || parent.AgentID != "" || parent.ParentID != "" || parent.Status != model.JobRunning {
		t.Fatalf("unexpected parent job: %#v", parent)
	}
	if parent.IPVersion != model.IPv6 {
		t.Fatalf("parent ip version = %d", parent.IPVersion)
	}
	if parent.StartedAt == nil {
		t.Fatalf("fanout parent should be marked started: %#v", parent)
	}
	events, err := st.ListJobEvents(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AgentID != "" || events[0].Event == nil || events[0].Event.Message != "started" {
		t.Fatalf("fanout parent should get one job-level started event: %#v", events)
	}
	jobs, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("children = %#v", jobs)
	}
	got := map[string]bool{}
	for _, job := range jobs {
		got[job.AgentID] = true
		if job.ParentID != parent.ID || job.Status != model.JobQueued {
			t.Fatalf("unexpected fanout child: %#v", job)
		}
		if job.StartedAt != nil || job.CompletedAt != nil || job.Error != "" {
			t.Fatalf("queued child should not inherit parent completion fields: %#v", job)
		}
		if job.ResolveOnAgent != true || job.ResolvedTarget != "" {
			t.Fatalf("unexpected fanout job: %#v", job)
		}
		if job.IPVersion != model.IPv6 {
			t.Fatalf("child should keep inferred IPv6 version: %#v", job)
		}
	}
	if got["edge-v4"] || !got["edge-v6"] || got["edge-dns"] || got["edge-offline"] {
		t.Fatalf("unexpected fanout agents: %#v", got)
	}
}

func TestCreateJobFiltersFanoutByAgentTags(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue", "shared"})
	upsertTestAgent(t, ctx, st, "edge-green", []string{"green", "shared"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","agent_tags":["blue","green"],"resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	children, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, child := range children {
		got[child.AgentID] = true
	}
	if len(children) != 2 || !got["edge-blue"] || !got["edge-green"] || got["edge-red"] {
		t.Fatalf("tag-filtered fanout agents = %#v", children)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","agent_id":"edge-blue","agent_tags":["blue"],"resolve_on_agent":true}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "agent_tags cannot be used with agent_id") {
		t.Fatalf("pinned tag request status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, invalidTag := range []string{"agent", "Agent", "id:edge-blue", "agent:grpc", "africa-grpc"} {
		t.Run("rejects reserved tag "+invalidTag, func(t *testing.T) {
			body, err := json.Marshal(model.CreateJobRequest{
				Tool:           model.ToolPing,
				Target:         "1.1.1.1",
				AgentTags:      []string{invalidTag},
				ResolveOnAgent: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "agent_tags contains reserved tag") {
				t.Fatalf("reserved tag %q status = %d, body = %s", invalidTag, rec.Code, rec.Body.String())
			}
		})
	}
	for name, tags := range map[string][]string{
		"empty":      {""},
		"whitespace": {" blue "},
		"duplicate":  {"blue", "blue"},
	} {
		t.Run("rejects noncanonical tags "+name, func(t *testing.T) {
			body, err := json.Marshal(model.CreateJobRequest{
				Tool: model.ToolPing, Target: "1.1.1.1", AgentTags: tags, ResolveOnAgent: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("tags %#v status = %d, body = %s", tags, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCreateJobRejectsFanoutBeyondHardLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	for index := 0; index <= model.MaxFanoutTargets; index++ {
		upsertTestAgent(t, ctx, st, fmt.Sprintf("edge-%02d", index), nil)
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "fanout target limit exceeded") {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	jobs, err := st.ListActiveJobs(ctx, model.MaxFanoutTargets+2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("over-limit fanout created jobs: %#v", jobs)
	}
}

func TestCreateJobWithAgentResolveAutoDomainDoesNotFilterProtocols(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-v4", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv4, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-v6", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv6, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"example.com","resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("children = %#v", jobs)
	}
	for _, job := range jobs {
		if job.IPVersion != model.IPAny || job.ResolvedTarget != "" {
			t.Fatalf("agent-resolved auto domain should be dispatched unchanged: %#v", job)
		}
	}
}

func TestCreateJobWithAgentResolveTypedDomainFiltersProtocols(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-v4", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv4, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-v6", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv6, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"example.com","ip_version":6,"resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].AgentID != "edge-v6" || jobs[0].IPVersion != model.IPv6 {
		t.Fatalf("children = %#v", jobs)
	}
}

func TestSelectDispatchTargetChoosesSupportedResolvedAddress(t *testing.T) {
	options := []dispatchTarget{
		{ResolvedTarget: "2606:4700:4700::1111", IPVersion: model.IPv6},
		{ResolvedTarget: "1.1.1.1", IPVersion: model.IPv4},
	}
	dual := model.Agent{ID: "dual", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline}
	if target, ok := selectDispatchTarget(dual, model.ToolPing, options, true); !ok || target.IPVersion != model.IPv6 {
		t.Fatalf("dual-stack target = %#v ok=%v", target, ok)
	}
	v4 := model.Agent{ID: "v4", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv4, Status: model.AgentOnline}
	if target, ok := selectDispatchTarget(v4, model.ToolPing, options, true); !ok || target.IPVersion != model.IPv4 {
		t.Fatalf("v4 target = %#v ok=%v", target, ok)
	}
	v6 := model.Agent{ID: "v6", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolIPv6, Status: model.AgentOnline}
	if target, ok := selectDispatchTarget(v6, model.ToolPing, options[1:], true); ok {
		t.Fatalf("v6-only agent should not accept ipv4-only option: %#v", target)
	}
}

func TestCreateJobWithPinnedIncompatibleAgentReportsFailureType(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-v4",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolIPv4,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"2606:4700:4700::1111","agent_id":"edge-v4","resolve_on_agent":true}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var job model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.AgentID != "edge-v4" || job.Status != model.JobFailed || job.IPVersion != model.IPv6 || job.Error != "" {
		t.Fatalf("unexpected job: %#v", job)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["error_type"] != model.JobErrorUnsupportedProtocol {
		t.Fatalf("expected unsupported protocol response, got %#v", response)
	}
	storedJob, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedJob.Error != model.JobErrorUnsupportedProtocol {
		t.Fatalf("stored job should keep internal error: %#v", storedJob)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AgentID != "edge-v4" || events[0].Event == nil || events[0].Event.Message != model.JobErrorUnsupportedProtocol {
		t.Fatalf("expected pinned unsupported protocol event: %#v", events)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"traceroute","target":"1.1.1.1","agent_id":"edge-v4","resolve_on_agent":true}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unsupported tool status = %d, body = %s", rec.Code, rec.Body.String())
	}
	response = map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["status"] != string(model.JobFailed) || response["error_type"] != model.JobErrorUnsupportedTool {
		t.Fatalf("expected unsupported tool response, got %#v", response)
	}
}

func TestGeoIPProxyReturnsCompactPayload(t *testing.T) {
	oldGeoIPHTTPClient := geoIPHTTPClient
	geoIPHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "http://geoip.test/1.0.0.1" {
			t.Fatalf("upstream url = %q", r.URL.String())
		}
		body := `{
			"asn":{"number":13335,"organization":"Cloudflare, Inc."},
			"country":{"code":"AU","name":"Australia"},
			"region":"Queensland",
			"city":"Brisbane",
			"isp":"Cloudflare",
			"reverse":"one.one.one.one.",
			"provider":"ip2location",
			"confidence":60.5,
			"asn_confidence":100
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
	defer func() { geoIPHTTPClient = oldGeoIPHTTPClient }()

	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "http://geoip.test", slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/v1/geoip/1.0.0.1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["reverse"] != "one.one.one.one." || got["country"] != "Australia" || got["region"] != "Queensland" || got["city"] != "Brisbane" || got["isp"] != "Cloudflare" {
		t.Fatalf("unexpected geoip response: %#v", got)
	}
	if got["asn"] != float64(13335) || got["org"] != "Cloudflare, Inc." {
		t.Fatalf("unexpected asn fields: %#v", got)
	}
	for _, dropped := range []string{"country_name", "continent", "location", "provider", "confidence", "asn_confidence"} {
		if _, ok := got[dropped]; ok {
			t.Fatalf("unexpected upstream field %q in response: %#v", dropped, got)
		}
	}
}

func TestGeoIPProxyAcceptsEscapedIPv6(t *testing.T) {
	oldGeoIPHTTPClient := geoIPHTTPClient
	geoIPHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/2606:4700:4700::1111" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		body := `{"asn":{"number":13335,"organization":"Cloudflare, Inc."},"country":{"name":"Australia"}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
	defer func() { geoIPHTTPClient = oldGeoIPHTTPClient }()

	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "http://geoip.test", slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/v1/geoip/2606%3A4700%3A4700%3A%3A1111", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeoIPProxyIsDisabledWhenURLIsEmpty(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/v1/geoip/1.0.0.1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeoIPProxyRejectsInvalidIP(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "http://geoip.test", slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/v1/geoip/example.com", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestGeoIPProxyUsesDedicatedGlobalRateLimit(t *testing.T) {
	oldGeoIPHTTPClient := geoIPHTTPClient
	geoIPHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"asn":{"number":13335},"country":{"name":"Australia"}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	defer func() { geoIPHTTPClient = oldGeoIPHTTPClient }()

	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1, Burst: 1},
		IP:     abuse.Limit{RequestsPerMinute: 1, Burst: 1},
		CIDR:   abuse.CIDRLimit{Limit: abuse.Limit{RequestsPerMinute: 1, Burst: 1}, IPv4Prefix: 24, IPv6Prefix: 64},
		GeoIP:  abuse.Limit{RequestsPerMinute: 10, Burst: 2},
	}), nil, "http://geoip.test", slog.Default())

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/geoip/1.0.0.1", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("geoip request %d status = %d, body = %s", i+1, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/geoip/1.0.0.1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third geoip request status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("regular request should still have its own global bucket, status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedRuntimeSettingsApplyToNewJobs(t *testing.T) {
	st := store.NewTestMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	settings, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	agentLabel := settings.LabelConfigs[config.AgentAllLabel]
	runtime := config.DefaultRuntime()
	runtime.Count = 2
	agentLabel.Runtime = &runtime
	settings.LabelConfigs[config.AgentAllLabel] = agentLabel
	body, _ := json.Marshal(managedLabelsPayload{LabelConfigs: settings.LabelConfigs})
	req := httptest.NewRequest(http.MethodPut, "/v1/manage/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body = %s", rec.Code, rec.Body.String())
	}

	now := time.Now().UTC()
	if err := st.UpsertAgent(context.Background(), model.Agent{
		ID:           "edge-1",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(model.CreateJobRequest{Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1"})
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("job status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var job model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if got := job.Args["count"]; got != "2" {
		t.Fatalf("job count arg = %q, want updated runtime count 2", got)
	}
}

func TestLabelRuntimeSettingsApplyToPinnedJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runtimeA := config.DefaultRuntime()
	runtimeA.Count = 4
	runtimeB := config.DefaultRuntime()
	runtimeB.Count = 2
	settings.LabelConfigs = map[string]config.LabelConfig{
		"blue": {Runtime: &runtimeA},
		"fast": {Runtime: &runtimeB},
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	upsertTestAgent(t, ctx, st, "edge-1", []string{"blue", "fast"})
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	hub.ApplySettings(settings)
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default(), TokenConfig{Token: settings.APITokens[0].Secret, Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body, _ := json.Marshal(model.CreateJobRequest{Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var job model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if got := job.Args["count"]; got != "2" {
		t.Fatalf("label runtime count = %q, want min 2", got)
	}
}

func TestLabelToolPolicyFiltersFanoutNodes(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.LabelConfigs = map[string]config.LabelConfig{
		"tcp-only": {
			ToolPolicies: map[string]config.Policy{
				"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": "tcp", "port": "443"}},
			},
		},
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	upsertTestAgent(t, ctx, st, "edge-icmp", []string{"icmp"})
	upsertTestAgent(t, ctx, st, "edge-tcp", []string{"tcp-only"})
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	hub.ApplySettings(settings)
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default(), TokenConfig{Token: settings.APITokens[0].Secret, Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"}}`))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var parent model.Job
	if err := json.Unmarshal(rec.Body.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	children, err := st.ListChildJobs(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].AgentID != "edge-icmp" {
		t.Fatalf("label policy should filter fanout nodes: %#v", children)
	}
}

func TestListAgentsIncludesPerAgentEffectiveTools(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.LabelConfigs = map[string]config.LabelConfig{
		"head-only": {
			ToolPolicies: map[string]config.Policy{
				"http": {Enabled: true, AllowedArgs: map[string]string{"method": "HEAD", "http_version": "1.1"}},
			},
		},
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-get", Capabilities: []model.Tool{model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-head", Capabilities: []model.Tool{model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-empty", Capabilities: []model.Tool{model.ToolDNS}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-head", Labels: []string{"head-only"}}); err != nil {
		t.Fatal(err)
	}
	policies := policy.FromConfig(map[string]config.Policy{
		"http": {Enabled: true, AllowedArgs: map[string]string{"method": "GET,HEAD"}},
	}, config.DefaultRuntime())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{
		Token: "token",
		Scope: TokenScope{
			Agents: []string{"*"},
			Tools:  map[model.Tool]ToolScope{model.ToolHTTP: {}},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var perms PermissionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &perms); err != nil {
		t.Fatal(err)
	}
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "GET,HEAD" {
		t.Fatalf("global http permissions = %#v", perms.Tools[model.ToolHTTP])
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agents status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agents []model.AgentView
	if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 || strings.Contains(rec.Body.String(), "capabilities") {
		t.Fatalf("agents should include only nodes with effective tools and hide capabilities: %s", rec.Body.String())
	}
	byID := map[string]model.AgentView{}
	for _, agent := range agents {
		byID[agent.ID] = agent
	}
	if byID["edge-get"].Tools[model.ToolHTTP].AllowedArgs["method"] != "GET,HEAD" {
		t.Fatalf("edge-get permissions = %#v", byID["edge-get"])
	}
	if byID["edge-head"].Tools[model.ToolHTTP].AllowedArgs["method"] != "HEAD" {
		t.Fatalf("edge-head permissions = %#v", byID["edge-head"])
	}
}

func TestScheduleTargetsRespectPerAgentLabelPolicies(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.LabelConfigs = map[string]config.LabelConfig{
		"no-ping": {
			ToolPolicies: map[string]config.Policy{
				"ping": {Enabled: false},
			},
		},
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	upsertTestAgent(t, ctx, st, "edge-denied", []string{"blue", "no-ping"})
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: settings.APITokens[0].Secret, Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite, ScheduleAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","cron_expression":"* * * * *","schedule_targets":[{"label":"blue"}]}`))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("schedule target with label-disabled tool status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestScheduleTargetsValidatePerAgentPolicyArgs(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.LabelConfigs = map[string]config.LabelConfig{
		"head-only": {
			ToolPolicies: map[string]config.Policy{
				"http": {Enabled: true, AllowedArgs: map[string]string{"method": "HEAD"}},
			},
		},
		"get-ok": {
			ToolPolicies: map[string]config.Policy{
				"http": {Enabled: true, AllowedArgs: map[string]string{"method": "GET", "http_version": "1.1"}},
			},
		},
	}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, agent := range []struct {
		id     string
		labels []string
	}{
		{id: "edge-head", labels: []string{"head-only"}},
		{id: "edge-get", labels: []string{"get-ok"}},
	} {
		if err := st.UpsertAgent(ctx, model.Agent{
			ID:           agent.id,
			Capabilities: []model.Tool{model.ToolHTTP},
			Protocols:    model.ProtocolAll,
			Status:       model.AgentOnline,
			LastSeenAt:   now,
			CreatedAt:    now,
		}); err != nil {
			t.Fatal(err)
		}
		upsertTestAgentConfigLabels(t, ctx, st, agent.id, agent.labels)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: settings.APITokens[0].Secret, Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite, ScheduleAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"http","target":"https://1.1.1.1","args":{"method":"GET"},"resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"head-only"}]}`))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("schedule target with label-invalid args status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"http","target":"https://1.1.1.1","args":{"method":"GET"},"resolve_on_agent":true,"cron_expression":"* * * * *","schedule_targets":[{"label":"agent"}]}`))
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("schedule target with mixed policy agents status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("agent_scope")) {
		t.Fatalf("create schedule exposed internal agent scope: %s", rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	storedSchedule, err := st.GetScheduledJob(ctx, sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedSchedule.ScheduleTargets) != 1 || !storedSchedule.ScheduleTargets[0].AgentScope.All {
		t.Fatalf("schedule should preserve unrestricted dynamic scope: %#v", storedSchedule.ScheduleTargets)
	}
}

func TestManagedRateLimitEndpointUsesLowercaseJSONAndDoesNotPersistInvalidConfig(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	before, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/manage/rate-limit", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rate-limit status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"Global", "IP", "CIDR", "Tools", "ExemptCIDRs"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("rate-limit response contains uppercase field %q: %#v", key, raw)
		}
	}
	rateLimitRaw, ok := raw["rate_limit"].(map[string]any)
	if !ok {
		t.Fatalf("rate-limit response missing rate_limit wrapper: %#v", raw)
	}
	for _, key := range []string{"global", "ip", "cidr"} {
		if _, ok := rateLimitRaw[key]; !ok {
			t.Fatalf("rate-limit response missing lowercase field %q: %#v", key, raw)
		}
	}
	if geoIPRaw, ok := rateLimitRaw["geoip"].(map[string]any); !ok || geoIPRaw["requests_per_minute"] != float64(120) || geoIPRaw["burst"] != float64(60) {
		t.Fatalf("rate-limit response missing geoip defaults: %#v", rateLimitRaw["geoip"])
	}

	next := before
	next.RateLimit.ExemptCIDRs = []string{"not-a-cidr"}
	body, _ := json.Marshal(managedRateLimitPayload{Revision: before.Revision, RateLimit: next.RateLimit})
	req = httptest.NewRequest(http.MethodPut, "/v1/manage/rate-limit", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid rate-limit status = %d, body = %s", rec.Code, rec.Body.String())
	}

	after, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.RateLimit.ExemptCIDRs) != len(before.RateLimit.ExemptCIDRs) {
		t.Fatalf("invalid settings persisted: before=%#v after=%#v", before.RateLimit.ExemptCIDRs, after.RateLimit.ExemptCIDRs)
	}
}

func TestManagedSettingsEndpointsRejectFailOpenSemantics(t *testing.T) {
	newHandler := func(t *testing.T) (*store.Memory, http.Handler) {
		t.Helper()
		st := store.NewTestMemory()
		handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
			Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
		return st, handler
	}

	t.Run("negative rate", func(t *testing.T) {
		st, handler := newHandler(t)
		before, err := st.GetManagedSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rateLimit := before.RateLimit
		rateLimit.Global.RequestsPerMinute = -1
		body, err := json.Marshal(managedRateLimitPayload{Revision: before.Revision, RateLimit: rateLimit})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPut, "/v1/manage/rate-limit", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		after, err := st.GetManagedSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != before.Revision {
			t.Fatalf("invalid rate update changed revision from %d to %d", before.Revision, after.Revision)
		}
	})

	labelTests := []struct {
		name    string
		configs map[string]config.LabelConfig
	}{
		{
			name: "unknown tool",
			configs: map[string]config.LabelConfig{"edge": {ToolPolicies: map[string]config.Policy{
				"speedtest": {Enabled: true},
			}}},
		},
		{
			name: "invalid HTTP version",
			configs: map[string]config.LabelConfig{"edge": {ToolPolicies: map[string]config.Policy{
				"http": {Enabled: true, AllowedArgs: map[string]string{"http_version": "h2"}},
			}}},
		},
	}
	for _, tt := range labelTests {
		t.Run(tt.name, func(t *testing.T) {
			st, handler := newHandler(t)
			before, err := st.GetManagedSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(managedLabelsPayload{Revision: before.Revision, LabelConfigs: tt.configs})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/v1/manage/labels", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			after, err := st.GetManagedSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision {
				t.Fatalf("invalid label update changed revision from %d to %d", before.Revision, after.Revision)
			}
		})
	}
}

func TestRequiredAuthRejectsRequestsWhenTokenSetIsEmpty(t *testing.T) {
	st := store.NewTestMemory()
	handler, err := NewWithOptions(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), Options{RequireAuth: true})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty required auth token set status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedTokenResourcesOnlyRevealValuesOnCreateAndRotate(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
	settings, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/manage/tokens", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens get status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var apiTokensResp managedAPITokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiTokensResp); err != nil {
		t.Fatal(err)
	}
	visibleAPITokens := apiTokensResp.Tokens
	if visibleAPITokens[0].Secret != "" || visibleAPITokens[0].ID == "" {
		t.Fatalf("existing api token should be redacted with id: %#v", visibleAPITokens[0])
	}

	body := []byte(`{"name":"bad-rename","secret":"client-secret","all":true}`)
	req = httptest.NewRequest(http.MethodPatch, "/v1/manage/tokens/"+visibleAPITokens[0].ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("api token patch with client secret status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"name":"renamed-admin"}`)
	req = httptest.NewRequest(http.MethodPatch, "/v1/manage/tokens/"+visibleAPITokens[0].ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redacted api token rename status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var apiTokenResp managedAPITokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiTokenResp); err != nil {
		t.Fatal(err)
	}
	savedAPI := apiTokenResp.Token
	if savedAPI.Secret != "" || savedAPI.ID != visibleAPITokens[0].ID || savedAPI.Name != "renamed-admin" {
		t.Fatalf("api token patch should not reveal secret: %#v", savedAPI)
	}
	if !savedAPI.All || savedAPI.ManageAccess != "write" {
		t.Fatalf("api token patch should preserve omitted permissions: %#v", savedAPI)
	}

	settings, err = st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldAPISecret := settings.APITokens[0].Secret
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/tokens/"+settings.APITokens[0].ID+"/rotate", nil)
	req.Header.Set("Authorization", "Bearer "+oldAPISecret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("api token rotation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	apiTokenResp = managedAPITokenResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiTokenResp); err != nil {
		t.Fatal(err)
	}
	savedAPI = apiTokenResp.Token
	if savedAPI.Secret == "" || savedAPI.Secret == oldAPISecret || savedAPI.Rotate || savedAPI.ID == "" {
		t.Fatalf("api token was not rotated: old=%q saved=%#v", oldAPISecret, savedAPI)
	}
	writeToken := savedAPI.Secret

	body = []byte(`{"name":"bad-create","secret":"client-secret","all":true}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("api token create with client secret status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, invalidBody := range [][]byte{
		[]byte(`{"name":"bad-agent","agents":[" edge-1"]}`),
		[]byte(`{"name":"bad-deny","denied_agents":["edge-1","edge-1"]}`),
	} {
		req = httptest.NewRequest(http.MethodPost, "/v1/manage/tokens", bytes.NewReader(invalidBody))
		req.Header.Set("Authorization", "Bearer "+writeToken)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid API token Agent ID scope status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	body = []byte(`{"name":"ops","all":true,"schedule_access":"write","manage_access":"write","agents":["*"]}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("api token create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	apiTokenResp = managedAPITokenResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiTokenResp); err != nil {
		t.Fatal(err)
	}
	createdAPI := apiTokenResp.Token
	if createdAPI.ID == "" || createdAPI.Secret == "" || createdAPI.Name != "ops" {
		t.Fatalf("api token create should reveal generated secret once: %#v", createdAPI)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/manage/tokens/"+createdAPI.ID, nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("api token delete status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"name":"edge","token":"manual-register-token"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/register-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register token create with client token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"name":"edge","allowed_agent_ids":[" edge-1 ","edge-1","edge-2"]}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/register-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical register token Agent IDs status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"name":"edge","allowed_agent_ids":["edge-1","edge-2"]}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/register-tokens", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("generated register token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var registerTokenResp managedRegisterTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registerTokenResp); err != nil {
		t.Fatal(err)
	}
	savedRegister := registerTokenResp.Token
	if savedRegister.ID == "" || savedRegister.Token == "" || savedRegister.Name != "edge" || savedRegister.RemainingUses != 1 {
		t.Fatalf("register token was not generated: %#v", savedRegister)
	}
	if len(savedRegister.AllowedAgentIDs) != 2 || savedRegister.AllowedAgentIDs[0] != "edge-1" || savedRegister.AllowedAgentIDs[1] != "edge-2" {
		t.Fatalf("register token allowed Agent IDs changed: %#v", savedRegister)
	}

	oldRegisterToken := savedRegister.Token
	body = []byte(`{"name":"bad-rename","token":"manual-register-token"}`)
	req = httptest.NewRequest(http.MethodPatch, "/v1/manage/register-tokens/"+savedRegister.ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register token patch with client token status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"name":"edge-renamed"}`)
	req = httptest.NewRequest(http.MethodPatch, "/v1/manage/register-tokens/"+savedRegister.ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redacted register token rename status = %d, body = %s", rec.Code, rec.Body.String())
	}
	registerTokenResp = managedRegisterTokenResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &registerTokenResp); err != nil {
		t.Fatal(err)
	}
	savedRegister = registerTokenResp.Token
	if savedRegister.Token != "" || savedRegister.ID == "" || savedRegister.Name != "edge-renamed" || savedRegister.RemainingUses != 1 {
		t.Fatalf("unchanged register token should stay redacted: %#v", savedRegister)
	}
	if len(savedRegister.AllowedAgentIDs) != 2 || savedRegister.AllowedAgentIDs[0] != "edge-1" || savedRegister.AllowedAgentIDs[1] != "edge-2" {
		t.Fatalf("rename should preserve allowed agent ids: %#v", savedRegister)
	}

	body = []byte(`{"remaining_uses":2,"allowed_agent_ids":["edge-3"]}`)
	req = httptest.NewRequest(http.MethodPatch, "/v1/manage/register-tokens/"+savedRegister.ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register token remaining uses update status = %d, body = %s", rec.Code, rec.Body.String())
	}
	registerTokenResp = managedRegisterTokenResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &registerTokenResp); err != nil {
		t.Fatal(err)
	}
	savedRegister = registerTokenResp.Token
	if savedRegister.Name != "edge-renamed" || savedRegister.RemainingUses != 2 || len(savedRegister.AllowedAgentIDs) != 1 || savedRegister.AllowedAgentIDs[0] != "edge-3" {
		t.Fatalf("register token partial update = %#v", savedRegister)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/manage/register-tokens/"+savedRegister.ID+"/rotate", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register token rotation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	registerTokenResp = managedRegisterTokenResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &registerTokenResp); err != nil {
		t.Fatal(err)
	}
	savedRegister = registerTokenResp.Token
	if savedRegister.Token == "" || savedRegister.Token == oldRegisterToken || savedRegister.Rotate {
		t.Fatalf("register token was not rotated: old=%q saved=%#v", oldRegisterToken, savedRegister)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/manage/tokens", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tokens get after rotation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	apiTokensResp = managedAPITokensResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &apiTokensResp); err != nil {
		t.Fatal(err)
	}
	visibleAPITokens = apiTokensResp.Tokens
	if visibleAPITokens[0].Secret != "" {
		t.Fatalf("api tokens should only be visible in write result: %#v", visibleAPITokens)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/manage/register-tokens", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register tokens get after rotation status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var registerTokensResp managedRegisterTokensResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &registerTokensResp); err != nil {
		t.Fatal(err)
	}
	visibleRegisterTokens := registerTokensResp.Tokens
	if visibleRegisterTokens[len(visibleRegisterTokens)-1].Token != "" {
		t.Fatalf("register tokens should only be visible in write result: %#v", visibleRegisterTokens)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/manage/register-tokens/"+savedRegister.ID, nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register token delete status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedTokenPayloadsAreSizeLimited(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := bytes.NewReader([]byte(`{"name":"` + strings.Repeat("x", int(defaultJSONBodyLimit)) + `"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/tokens", body)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("large api token body status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedLabelsEndpointKeepsAgentRuntimeAndRemovesSettingsRoute(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	payload := managedLabelsPayload{LabelConfigs: map[string]config.LabelConfig{
		"blue": {
			ToolPolicies: map[string]config.Policy{
				"ping": {Enabled: true},
			},
		},
	}}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/v1/manage/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("labels status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var saved managedLabelsPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	agentConfig := saved.LabelConfigs[config.AgentAllLabel]
	if agentConfig.Runtime == nil || agentConfig.Scheduler == nil {
		t.Fatalf("agent label should own runtime and scheduler defaults: %#v", agentConfig)
	}
	if _, ok := saved.LabelConfigs["blue"]; !ok {
		t.Fatalf("custom label was not preserved: %#v", saved.LabelConfigs)
	}

	settings, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/manage/settings", nil)
	req.Header.Set("Authorization", "Bearer "+settings.APITokens[0].Secret)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("old settings route status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedAgentLabelsBatchUpdatesWithoutTransportAndRejectsBadIDs(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-grpc", []string{"old"})
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-grpc", Disabled: true, Labels: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{ID: "edge-http", Transport: config.OutboundTransportHTTP, Enabled: false, URL: "https://agent.local", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthBearer, Secret: "secret"}, Labels: []string{"old-http"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body, _ := json.Marshal(managedAgentLabelsPayload{Agents: []managedAgentLabelPatch{
		{ID: "edge-grpc", Labels: []string{"new"}},
		{ID: "edge-http", Labels: []string{"new-http"}},
	}})
	req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch labels status = %d, body = %s", rec.Code, rec.Body.String())
	}

	cfg, err := st.GetAgentConfig(ctx, "edge-grpc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Disabled != true || !reflect.DeepEqual(cfg.Labels, []string{"new"}) {
		t.Fatalf("grpc cfg = %#v", cfg)
	}
	node, err := st.GetOutboundAgent(ctx, "edge-http")
	if err != nil {
		t.Fatal(err)
	}
	if node.Enabled != false || node.URL != "https://agent.local" || node.Auth.Secret != "secret" || !reflect.DeepEqual(node.Labels, []string{"new-http"}) {
		t.Fatalf("http node = %#v", node)
	}

	invalidLabels := []struct {
		name   string
		labels []string
	}{
		{name: "empty", labels: []string{""}},
		{name: "whitespace", labels: []string{" new "}},
		{name: "duplicate", labels: []string{"new", "new"}},
		{name: "reserved", labels: []string{config.AgentGRPCLabel}},
		{name: "reserved id", labels: []string{"id:edge-grpc"}},
	}
	for _, tt := range invalidLabels {
		t.Run("rejects "+tt.name+" label", func(t *testing.T) {
			body, err := json.Marshal(managedAgentLabelsPayload{Agents: []managedAgentLabelPatch{{ID: "edge-grpc", Labels: tt.labels}}})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/labels", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			cfg, err := st.GetAgentConfig(ctx, "edge-grpc")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Labels, []string{"new"}) {
				t.Fatalf("invalid labels mutated config: %#v", cfg.Labels)
			}
		})
	}

	body, _ = json.Marshal(managedAgentLabelsPayload{Agents: []managedAgentLabelPatch{
		{ID: "edge-grpc", Labels: []string{"one"}},
		{ID: "edge-grpc", Labels: []string{"two"}},
	}})
	req = httptest.NewRequest(http.MethodPut, "/v1/manage/agents/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate id status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body, err = json.Marshal(managedAgentLabelsPayload{Agents: []managedAgentLabelPatch{{ID: " edge-grpc ", Labels: []string{"new"}}}})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPut, "/v1/manage/agents/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical agent id status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestManagedLabelsAndAgentLabelsUpdateAtomically(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-grpc", []string{"old"})
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-grpc", Labels: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	payload := managedLabelsAndAgentsPayload{
		LabelConfigs: map[string]config.LabelConfig{"blue": {ToolPolicies: map[string]config.Policy{"ping": {Enabled: true}}}},
		Agents:       []managedAgentLabelPatch{{ID: "missing", Labels: []string{"blue"}}},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/v1/manage/labels/state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("atomic bad agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.LabelConfigs["blue"]; ok {
		t.Fatalf("label config persisted despite failed agent update: %#v", settings.LabelConfigs)
	}

	payload.Agents = []managedAgentLabelPatch{{ID: "edge-grpc", Labels: []string{"blue"}}}
	body, _ = json.Marshal(payload)
	req = httptest.NewRequest(http.MethodPut, "/v1/manage/labels/state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("atomic labels status = %d, body = %s", rec.Code, rec.Body.String())
	}
	settings, err = st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.LabelConfigs["blue"]; !ok {
		t.Fatalf("label config was not persisted: %#v", settings.LabelConfigs)
	}
	cfg, err := st.GetAgentConfig(ctx, "edge-grpc")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Labels, []string{"blue"}) {
		t.Fatalf("agent labels = %#v", cfg.Labels)
	}
}

func TestManagedLabelStateRejectsStaleRevision(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	upsertTestAgent(t, ctx, st, "edge-1", nil)
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	staleRevision := settings.Revision
	settings.LabelConfigs["blue"] = config.LabelConfig{}
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}

	payload := managedLabelsAndAgentsPayload{
		Revision:     staleRevision,
		LabelConfigs: settings.LabelConfigs,
		Agents:       []managedAgentLabelPatch{{ID: "edge-1", Labels: []string{"blue"}}},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/v1/manage/labels/state", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale revision status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateOutboundAgentRejectsDuplicateID(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"edge-http","transport":"http","enabled":true,"url":"http://one.local","auth":{"type":"none"},"tls":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create http agent status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"id":"edge-http","transport":"http","enabled":true,"url":"http://two.local","auth":{"type":"none"},"tls":{}}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate http agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	node, err := st.GetOutboundAgent(context.Background(), "edge-http")
	if err != nil {
		t.Fatal(err)
	}
	if node.URL != "http://one.local" {
		t.Fatalf("duplicate create should not overwrite existing http agent: %#v", node)
	}

	body = []byte(`{"id":" edge-http ","transport":"http","enabled":true,"url":"http://attacker.local","auth":{"type":"none"},"tls":{}}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical id status = %d, body = %s", rec.Code, rec.Body.String())
	}
	node, err = st.GetOutboundAgent(context.Background(), "edge-http")
	if err != nil || node.URL != "http://one.local" {
		t.Fatalf("non-canonical id overwrote canonical record: node=%#v err=%v", node, err)
	}
}

func TestCreateOutboundAgentRejectsRemovedPrivateNetworkOverride(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"edge-http","transport":"http","enabled":false,"url":"http://agent.internal","allow_private_network":true,"auth":{"type":"none"},"tls":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("removed field status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetOutboundAgent(context.Background(), "edge-http"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("request containing removed field was persisted: %v", err)
	}
}

func TestCreateOutboundAgentAllowsPreconfiguredCredentialsOverHTTPAndRedactsResponse(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"edge-http-auth","transport":"http","enabled":false,"url":"http://agent.example","headers":{"X-Provider-Version":"v1"},"auth":{"type":"bearer","secret":"managed-secret"},"tls":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response config.OutboundAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Auth.Secret != "" || response.Headers["X-Provider-Version"] != "v1" {
		t.Fatalf("response was not correctly redacted: %#v", response)
	}
	stored, err := st.GetOutboundAgent(context.Background(), "edge-http-auth")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth.Secret != "managed-secret" || stored.URL != "http://agent.example" {
		t.Fatalf("stored managed credentials = %#v", stored)
	}
}

func TestCreateOutboundAgentAcceptsHuaweiCloudAKSKAndRedactsSecretKey(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"functiongraph-edge","transport":"event","enabled":false,"url":"https://functiongraph.example/invoke","headers":{"X-CFF-Request-Version":"v1"},"auth":{"type":"huaweicloud_aksk","access_key":"test-access-key","project_id":"test-project-id","secret":"test-secret-key"},"event_response":"result_field","tls":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "test-secret-key") {
		t.Fatalf("create response leaked secret key: %s", rec.Body.String())
	}
	var response config.OutboundAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Auth.Secret != "" || response.Auth.AccessKey != "test-access-key" || response.Auth.ProjectID != "test-project-id" {
		t.Fatalf("response auth = %#v", response.Auth)
	}
	stored, err := st.GetOutboundAgent(context.Background(), "functiongraph-edge")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth.Secret != "test-secret-key" || stored.Auth.AccessKey != "test-access-key" || stored.Auth.ProjectID != "test-project-id" {
		t.Fatalf("stored auth = %#v", stored.Auth)
	}
}

func TestCreateOutboundAgentRejectsUnsupportedHuaweiCloudSecurityToken(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"functiongraph-edge","transport":"event","enabled":false,"url":"https://functiongraph.example/invoke","auth":{"type":"huaweicloud_aksk","access_key":"test-access-key","project_id":"test-project-id","secret":"test-secret-key","security_token":"unsupported"},"event_response":"result_field","tls":{}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetOutboundAgent(context.Background(), "functiongraph-edge"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unsupported temporary credential was persisted: %v", err)
	}
}

func TestCreateOutboundAgentRejectsInvalidHuaweiCloudAccessKeySyntax(t *testing.T) {
	for name, accessKey := range map[string]string{
		"space": "access key",
		"tab":   "access\tkey",
		"comma": "access,key",
	} {
		t.Run(name, func(t *testing.T) {
			st := store.NewTestMemory()
			handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
				Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
				IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
			node := config.OutboundAgent{
				ID: "functiongraph-edge", Transport: config.OutboundTransportEvent, Enabled: false,
				URL: "https://functiongraph.example/invoke",
				Auth: config.OutboundAgentAuth{
					Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: accessKey,
					ProjectID: "test-project-id", Secret: "test-secret-key",
				},
				EventResponse: config.EventResponseResultField,
			}
			body, err := json.Marshal(node)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if _, err := st.GetOutboundAgent(context.Background(), node.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invalid access key was persisted: %v", err)
			}
		})
	}
}

type upsertOutboundAgentErrorStore struct {
	store.Store
	err error
}

func (s *upsertOutboundAgentErrorStore) UpsertOutboundAgent(context.Context, config.OutboundAgent) error {
	return s.err
}

func TestWriteOutboundAgentRejectsInvalidTLSAndMasksStoreErrors(t *testing.T) {
	newHandler := func(st store.Store) http.Handler {
		return New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
			Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
	}

	t.Run("invalid TLS material", func(t *testing.T) {
		st := store.NewTestMemory()
		body := []byte(`{"id":"edge-tls","transport":"http","enabled":true,"url":"https://agent.example","auth":{"type":"none"},"tls":{"enabled":true,"ca_files":["/missing/mtr-ca.pem"]}}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		newHandler(st).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid outbound agent TLS configuration") || strings.Contains(rec.Body.String(), "/missing/") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if _, err := st.GetOutboundAgent(context.Background(), "edge-tls"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("invalid TLS configuration was persisted: %v", err)
		}
	})

	t.Run("store error", func(t *testing.T) {
		base := store.NewTestMemory()
		st := &upsertOutboundAgentErrorStore{Store: base, err: errors.New("database secret detail")}
		body := []byte(`{"id":"edge-store-error","transport":"http","enabled":false,"url":"http://agent.example","auth":{"type":"none"},"tls":{}}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		newHandler(st).ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "database secret detail") || !strings.Contains(rec.Body.String(), "internal server error") {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		events := base.AuditEvents()
		if len(events) != 1 || events[0].Decision != "deny" || events[0].Reason != "internal server error" {
			t.Fatalf("audit events = %#v", events)
		}
	})
}

func TestUpdateOutboundAgentOnlyPreservesSecretForUnchangedCredentialScope(t *testing.T) {
	ctx := context.Background()
	newHandler := func(st store.Store) http.Handler {
		return New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
			Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
	}
	base := config.OutboundAgent{
		ID: "edge-http", Transport: config.OutboundTransportHTTP, URL: "https://agent.example/invoke",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthBearer, Secret: "existing-secret"},
	}

	t.Run("unchanged scope", func(t *testing.T) {
		st := store.NewTestMemory()
		if err := st.UpsertOutboundAgent(ctx, base); err != nil {
			t.Fatal(err)
		}
		update := base
		update.Auth.Secret = ""
		update.Labels = []string{"blue"}
		body, _ := json.Marshal(update)
		req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/edge-http", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		newHandler(st).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		saved, err := st.GetOutboundAgent(ctx, base.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Auth.Secret != "existing-secret" || !reflect.DeepEqual(saved.Labels, []string{"blue"}) {
			t.Fatalf("saved = %#v", saved)
		}
	})

	changes := map[string]func(*config.OutboundAgent){
		"url": func(node *config.OutboundAgent) { node.URL = "https://other.example/invoke" },
		"transport": func(node *config.OutboundAgent) {
			node.Transport = config.OutboundTransportEvent
			node.EventResponse = config.EventResponseDirect
		},
		"request headers": func(node *config.OutboundAgent) { node.Headers = map[string]string{"X-Provider-Version": "2"} },
		"tls identity":    func(node *config.OutboundAgent) { node.TLS = config.TLS{Enabled: true, ServerName: "other.example"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			st := store.NewTestMemory()
			if err := st.UpsertOutboundAgent(ctx, base); err != nil {
				t.Fatal(err)
			}
			update := base
			update.Auth.Secret = ""
			change(&update)
			body, _ := json.Marshal(update)
			req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/edge-http", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			newHandler(st).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			saved, err := st.GetOutboundAgent(ctx, base.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.URL != base.URL || saved.Transport != base.Transport || saved.Auth.Secret != "existing-secret" {
				t.Fatalf("rejected update changed stored credentials: %#v", saved)
			}
		})
	}
}

func TestUpdateOutboundAgentOnlyPreservesHuaweiCloudSecretKeyForUnchangedCredentials(t *testing.T) {
	ctx := context.Background()
	newHandler := func(st store.Store) http.Handler {
		return New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
			Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
			IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})
	}
	base := config.OutboundAgent{
		ID: "functiongraph-edge", Transport: config.OutboundTransportEvent,
		URL:     "https://functiongraph.example/invoke",
		Headers: map[string]string{"X-Cff-Request-Version": "v1"},
		Auth: config.OutboundAgentAuth{
			Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "existing-access-key",
			ProjectID: "existing-project-id", Secret: "existing-secret-key",
		},
		EventResponse: config.EventResponseResultField,
	}

	t.Run("unchanged credentials", func(t *testing.T) {
		st := store.NewTestMemory()
		if err := st.UpsertOutboundAgent(ctx, base); err != nil {
			t.Fatal(err)
		}
		update := base
		update.Auth.Secret = ""
		update.Labels = []string{"blue"}
		body, _ := json.Marshal(update)
		req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/functiongraph-edge", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		newHandler(st).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "existing-secret-key") {
			t.Fatalf("update response leaked secret key: %s", rec.Body.String())
		}
		saved, err := st.GetOutboundAgent(ctx, base.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Auth.Secret != "existing-secret-key" || !reflect.DeepEqual(saved.Labels, []string{"blue"}) {
			t.Fatalf("saved = %#v", saved)
		}
	})

	changes := map[string]func(*config.OutboundAgent){
		"auth type": func(node *config.OutboundAgent) {
			node.Auth = config.OutboundAgentAuth{Type: config.OutboundAuthBearer}
		},
		"access key": func(node *config.OutboundAgent) { node.Auth.AccessKey = "other-access-key" },
		"project id": func(node *config.OutboundAgent) { node.Auth.ProjectID = "other-project-id" },
		"url":        func(node *config.OutboundAgent) { node.URL = "https://other-functiongraph.example/invoke" },
		"transport": func(node *config.OutboundAgent) {
			node.Transport = config.OutboundTransportHTTP
			node.EventResponse = ""
		},
		"request headers": func(node *config.OutboundAgent) {
			node.Headers = map[string]string{"X-Cff-Request-Version": "v2"}
		},
		"tls": func(node *config.OutboundAgent) { node.TLS = config.TLS{Enabled: true} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			st := store.NewTestMemory()
			if err := st.UpsertOutboundAgent(ctx, base); err != nil {
				t.Fatal(err)
			}
			update := base
			update.Auth.Secret = ""
			change(&update)
			body, _ := json.Marshal(update)
			req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/functiongraph-edge", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer admin")
			rec := httptest.NewRecorder()
			newHandler(st).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			saved, err := st.GetOutboundAgent(ctx, base.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(saved.Auth, base.Auth) {
				t.Fatalf("rejected update changed stored credentials: %#v", saved.Auth)
			}
		})
	}
}

func TestDeleteManagedAgentRemovesConfigOnlyGRPCAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-config", Labels: []string{"blue"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodGet, "/v1/manage/agents", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list config-only grpc agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agents []ManagedAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ID != "edge-config" || agents[0].Status != model.AgentOffline || !reflect.DeepEqual(agents[0].Labels, []string{"agent", "agent:grpc", "id:edge-config", "blue"}) {
		t.Fatalf("config-only grpc agent listing = %#v", agents)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/manage/agents/edge-config", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete config-only grpc agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	configs, err := st.ListAgentConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.ID == "edge-config" {
			t.Fatalf("config-only agent was not deleted: %#v", configs)
		}
	}
}

func TestDeleteManagedGRPCAgentPropagatesConfigDeleteFailure(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	now := time.Now().UTC()
	if err := base.UpsertAgent(ctx, model.Agent{ID: "edge-delete-error", Status: model.AgentOffline, LastSeenAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := base.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-delete-error", Labels: []string{"blue"}}); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("delete agent config failed")
	st := &deleteAgentConfigErrorStore{Store: base, err: deleteErr}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodDelete, "/v1/manage/agents/edge-delete-error", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := base.GetAgentConfig(ctx, "edge-delete-error"); err != nil {
		t.Fatalf("failed config deletion should leave retryable config: %v", err)
	}
}

type deleteAgentConfigErrorStore struct {
	store.Store
	err error
}

func (s *deleteAgentConfigErrorStore) DeleteAgentConfig(context.Context, string) error {
	return s.err
}

func TestDeleteManagedOutboundAgentRemovesRuntimeAgent(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{
		ID:        "edge-http",
		Transport: config.OutboundTransportHTTP,
		Enabled:   true,
		URL:       "http://agent.local",
		Auth:      config.OutboundAgentAuth{Type: config.OutboundAuthNone},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-http",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodDelete, "/v1/manage/agents/edge-http", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete http agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetOutboundAgent(ctx, "edge-http"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("http agent configuration still exists: %v", err)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.ID == "edge-http" {
			t.Fatalf("http agent runtime record still exists: %#v", agents)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/manage/agents", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("deleted http agent reappeared in managed agents: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/manage/agents/edge-http", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestUpdateManagedAgentRequiresExplicitTransport(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/missing", strings.NewReader(`{"labels":["blue"]}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing transport update status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateGRPCAgentRejectsNonCanonicalPathID(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{
		ID: "edge", Transport: config.OutboundTransportHTTP, URL: "http://agent.example",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodPut, "/v1/manage/agents/%20edge%20", strings.NewReader(`{"id":" edge ","transport":"grpc","disabled":false,"labels":[]}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if configs, err := st.ListAgentConfigs(ctx); err != nil || len(configs) != 0 {
		t.Fatalf("non-canonical path created grpc config: configs=%#v err=%v", configs, err)
	}
	if node, err := st.GetOutboundAgent(ctx, "edge"); err != nil || node.URL != "http://agent.example" {
		t.Fatalf("canonical outbound agent changed: node=%#v err=%v", node, err)
	}
}

func TestManageWriteAuditsAllowedAndDeniedActions(t *testing.T) {
	st := store.NewTestMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(),
		TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}},
		TokenConfig{Token: "reader", Scope: TokenScope{ManageAccess: ScheduleAccessRead}},
	)

	req := httptest.NewRequest(http.MethodPut, "/v1/manage/rate-limit", strings.NewReader(`{"global":{"requests_per_minute":600,"burst":200},"ip":{"requests_per_minute":60,"burst":20},"cidr":{"requests_per_minute":300,"burst":100,"ipv4_prefix":32,"ipv6_prefix":128}}`))
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied rate limit status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/v1/manage/labels", strings.NewReader(`{"label_configs":{"blue":{"tool_policies":{"ping":{"enabled":true}}}}}`))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed labels status = %d, body = %s", rec.Code, rec.Body.String())
	}

	events := st.AuditEvents()
	if len(events) != 2 {
		t.Fatalf("audit event count = %d, events = %#v", len(events), events)
	}
	if events[0].Action != "manage.rate_limit.update" || events[0].Decision != "deny" || events[0].Subject != "token:"+managedSecretHash("reader") {
		t.Fatalf("unexpected denied audit event: %#v", events[0])
	}
	if events[1].Action != "manage.labels.update" || events[1].Decision != "allow" || events[1].Subject != "token:"+managedSecretHash("admin") {
		t.Fatalf("unexpected allowed audit event: %#v", events[1])
	}
}

func TestManageReadCannotAccessTokenEndpoint(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.APITokens = append(settings.APITokens, config.APITokenPermission{
		Name:         "reader-visible",
		Secret:       "reader-visible-secret",
		ManageAccess: "read",
		Agents:       []string{"*"},
	})
	settings.RegisterTokens = append(settings.RegisterTokens, config.RegisterToken{Name: "register", Token: "register-secret"})
	if settings, err = st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{
		ID:           "edge-http",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolAll,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{
		ID: "edge-http", Transport: config.OutboundTransportHTTP, Enabled: true, URL: "https://agent.local",
		Auth: config.OutboundAgentAuth{
			Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "test-access-key",
			ProjectID: "test-project-id", Secret: "test-secret-key",
		},
	}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "reader", Scope: TokenScope{ManageAccess: ScheduleAccessRead}})

	req := httptest.NewRequest(http.MethodGet, "/v1/manage/tokens", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokens status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/manage/agents", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agents status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agents []ManagedAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Outbound == nil || agents[0].Outbound.Auth.Secret != "" ||
		agents[0].Outbound.Auth.AccessKey != "test-access-key" || agents[0].Outbound.Auth.ProjectID != "test-project-id" ||
		strings.Contains(rec.Body.String(), "test-secret-key") {
		t.Fatalf("read-only agents leaked secret key or lost public credential fields: %#v", agents)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/manage/agents/edge-http", nil)
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("agent detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agent ManagedAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &agent); err != nil {
		t.Fatal(err)
	}
	if agent.Outbound == nil || agent.Outbound.Auth.Secret != "" ||
		agent.Outbound.Auth.AccessKey != "test-access-key" || agent.Outbound.Auth.ProjectID != "test-project-id" ||
		strings.Contains(rec.Body.String(), "test-secret-key") {
		t.Fatalf("read-only agent detail leaked secret key or lost public credential fields: %#v", agent)
	}
}

func TestManagedAgentsSeparateHTTPAndGRPCTransports(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-grpc", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-http", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{ID: "edge-http", Transport: config.OutboundTransportHTTP, Enabled: true, URL: "https://agent.local", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthBearer, Secret: "secret"}, Labels: []string{"configured"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-grpc", Labels: []string{"configured-grpc"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	req := httptest.NewRequest(http.MethodGet, "/v1/manage/agents", nil)
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var agents []ManagedAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &agents); err != nil {
		t.Fatal(err)
	}
	var rawAgents []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rawAgents); err != nil {
		t.Fatal(err)
	}
	for _, raw := range rawAgents {
		if _, ok := raw["type"]; ok {
			t.Fatalf("managed agent response should not include duplicate type field: %s", rec.Body.String())
		}
	}
	byTransport := map[string][]ManagedAgent{}
	for _, agent := range agents {
		byTransport[agent.Transport] = append(byTransport[agent.Transport], agent)
	}
	grpcAgents := byTransport["grpc"]
	if len(grpcAgents) != 1 || grpcAgents[0].ID != "edge-grpc" || grpcAgents[0].Transport != "grpc" {
		t.Fatalf("grpc agents = %#v", grpcAgents)
	}
	if !reflect.DeepEqual(grpcAgents[0].Labels, []string{"agent", "agent:grpc", "id:edge-grpc", "configured-grpc"}) {
		t.Fatalf("grpc managed labels should come from server config: %#v", grpcAgents[0].Labels)
	}
	outboundAgents := byTransport["http"]
	if len(outboundAgents) != 1 || outboundAgents[0].ID != "edge-http" || outboundAgents[0].Transport != "http" {
		t.Fatalf("http agents = %#v", outboundAgents)
	}
	if !reflect.DeepEqual(outboundAgents[0].Labels, []string{"agent", "agent:http", "id:edge-http", "configured"}) {
		t.Fatalf("http labels = %#v", outboundAgents[0].Labels)
	}
	if outboundAgents[0].Outbound == nil || !reflect.DeepEqual(outboundAgents[0].Outbound.Labels, []string{"configured"}) || outboundAgents[0].Outbound.Auth.Secret != "" {
		t.Fatalf("http config = %#v", outboundAgents[0].Outbound)
	}
}

func TestDNSDispatchDoesNotRequireAddressResolution(t *testing.T) {
	server := &Server{}
	options, err := server.dispatchOptions(context.Background(), model.CreateJobRequest{
		Tool:      model.ToolDNS,
		Target:    "mx-only.invalid",
		Args:      map[string]string{"type": "MX"},
		IPVersion: model.IPAny,
	}, policy.DefaultPolicies())
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].ResolvedTarget != "" || options[0].IPVersion != model.IPAny {
		t.Fatalf("DNS dispatch options = %#v", options)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func waitSSEEvent(t *testing.T, rec *sseRecorder, id string, deadline <-chan time.Time) sseEvent {
	t.Helper()
	for {
		for _, event := range rec.events() {
			if event.ID == id {
				return event
			}
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for SSE data")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

type sseEvent struct {
	ID    string
	Event string
	Data  string
}

type sseRecorder struct {
	mu     sync.Mutex
	header http.Header
	body   bytes.Buffer
	status int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}}
}

func (r *sseRecorder) Header() http.Header {
	return r.header
}

func (r *sseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *sseRecorder) Flush() {}

func (r *sseRecorder) events() []sseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []sseEvent
	for _, block := range strings.Split(r.body.String(), "\n\n") {
		var event sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				event.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				event.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event.ID != "" || event.Event != "" || event.Data != "" {
			out = append(out, event)
		}
	}
	return out
}
