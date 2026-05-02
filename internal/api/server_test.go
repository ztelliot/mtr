package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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

func TestAuthFailureCountsAgainstRequestLimit(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	if rec.Code != http.StatusOK {
		t.Fatalf("query access token status = %d, body = %s", rec.Code, rec.Body.String())
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

func TestClientIPUsesConfiguredHeaderFromUntrustedPeer(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, []string{"Eo-Connecting-Ip"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("Eo-Connecting-Ip", "198.51.100.22")

	if got := resolver.Resolve(req); got != "198.51.100.22" {
		t.Fatalf("client ip = %q, want configured header client", got)
	}
}

func TestClientIPUsesConfiguredSingleIPHeaderOrder(t *testing.T) {
	resolver, err := newClientIPResolver([]string{"10.0.0.0/8"}, []string{"Cf-Connecting-Ip", "Eo-Connecting-Ip"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
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
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Real-IP", "198.51.100.21")
	req.Header.Set("Eo-Connecting-Ip", "198.51.100.22")

	if got := resolver.Resolve(req); got != "198.51.100.22" {
		t.Fatalf("client ip = %q, want configured header after ignoring x-real-ip", got)
	}
}

func TestNewWithOptionsRejectsInvalidTrustedProxy(t *testing.T) {
	st := store.NewMemory()
	_, err := NewWithOptions(st, policy.DefaultPolicies(), nil, nil, "", slog.Default(), Options{
		TrustedProxies: []string{"not-an-ip"},
	})
	if err == nil {
		t.Fatal("expected invalid trusted proxy to be rejected")
	}
}

func TestTokenPermissionsRestrictToolsArgsAgentsAndSchedules(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
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
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "HEAD" || perms.Tools[model.ToolPort].AllowedArgs["port"] != "1-1000" || len(perms.Agents) != 1 || perms.Agents[0] != "edge-1" {
		t.Fatalf("restricted token permissions = %#v", perms)
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
	st := store.NewMemory()
	now := time.Now().UTC()
	if err := st.UpsertAgent(ctx, model.Agent{ID: "edge-1", Capabilities: []model.Tool{model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	policies := policy.FromConfig(map[string]config.Policy{
		"http": {Enabled: true, AllowedArgs: map[string]string{"method": "GET"}},
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
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "GET" {
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
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	now := time.Now().UTC()
	sched := model.ScheduledJob{
		ID:              "sched-1",
		Revision:        1,
		Name:            "readonly",
		Enabled:         true,
		Tool:            model.ToolTraceroute,
		Target:          "example.com",
		IntervalSeconds: 60,
		NextRunAt:       now,
		CreatedAt:       now,
		UpdatedAt:       now,
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

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","schedule_targets":[{"label":"agent","interval_seconds":60}]}`))
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token should not create schedules, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req = httptest.NewRequest(method, "/v1/schedules/"+sched.ID, strings.NewReader(`{"tool":"ping","target":"1.1.1.1","schedule_targets":[{"label":"agent","interval_seconds":60}]}`))
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
	st := store.NewMemory()
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

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"schedule_targets":[{"label":"blue","interval_seconds":60}]}`))
	req.Header.Set("Authorization", "Bearer restricted")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if len(sched.ScheduleTargets) != 1 || sched.ScheduleTargets[0].Label != "blue" {
		t.Fatalf("schedule targets = %#v", sched.ScheduleTargets)
	}
	if !reflect.DeepEqual(sched.ScheduleTargets[0].AllowedAgentIDs, []string{"edge-1"}) {
		t.Fatalf("allowed agents = %#v", sched.ScheduleTargets[0].AllowedAgentIDs)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"schedule_targets":[{"label":"id:edge-1","interval_seconds":60}]}`))
	req.Header.Set("Authorization", "Bearer restricted")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("explicit id target status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateScheduleFreezesTagAndDenyScopesToCurrentAgents(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
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

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"schedule_targets":[{"label":"blue","interval_seconds":60}]}`))
	req.Header.Set("Authorization", "Bearer tagged")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}
	if len(sched.ScheduleTargets) != 1 {
		t.Fatalf("schedule targets = %#v", sched.ScheduleTargets)
	}
	if !reflect.DeepEqual(sched.ScheduleTargets[0].AllowedAgentIDs, []string{"edge-1"}) {
		t.Fatalf("allowed agents should be frozen to matching current scope: %#v", sched.ScheduleTargets[0].AllowedAgentIDs)
	}
}

func TestServerManagedLabelsGrantTokenScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
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
	st := store.NewMemory()
	upsertTestAgent(t, ctx, st, "edge-1", []string{"agent"})
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","args":{"count":"1"},"schedule_targets":[{"label":"agent","interval_seconds":30}]}`
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
	if sched.ID == "" || !sched.Enabled || sched.IntervalSeconds != 30 {
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
	st := store.NewMemory()
	upsertTestAgent(t, ctx, st, "edge-1", []string{"agent"})
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"schedule_targets":[{"label":"agent","interval_seconds":30}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"edited","enabled":false,"tool":"ping","target":"8.8.8.8","args":{"protocol":"icmp"},"schedule_targets":[{"label":"agent","interval_seconds":120}]}`
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
	if updated.ID != sched.ID || updated.Name != "edited" || updated.Enabled || updated.Target != "8.8.8.8" || updated.IntervalSeconds != 120 || updated.Revision != sched.Revision+1 || updated.CreatedAt.IsZero() {
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

func TestUpdateScheduleIntervalRefreshesNextRunWithoutBumpingRevision(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	upsertTestAgent(t, ctx, st, "edge-1", []string{"agent"})
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"schedule_targets":[{"label":"agent","interval_seconds":30}]}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"schedule_targets":[{"label":"agent","interval_seconds":120}]}`
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
		t.Fatalf("interval-only update bumped revision: got %d, want %d", updated.Revision, sched.Revision)
	}
	if updated.IntervalSeconds != 120 {
		t.Fatalf("interval not updated: %#v", updated)
	}
	if updated.NextRunAt.Before(sched.NextRunAt) || updated.NextRunAt.Equal(sched.NextRunAt) {
		t.Fatalf("interval-only update should refresh next run time: before=%s after=%s", sched.NextRunAt, updated.NextRunAt)
	}
}

func TestCreateScheduleAcceptsLabelTargetsWithIntervals(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	upsertTestAgent(t, ctx, st, "edge-blue", []string{"blue"})
	upsertTestAgent(t, ctx, st, "edge-red", []string{"red"})
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true,"schedule_targets":[{"label":"blue","interval_seconds":30},{"label":"red","interval_seconds":120}]}`
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
	if sched.IntervalSeconds != 30 || len(sched.ScheduleTargets) != 2 {
		t.Fatalf("unexpected schedule targets: %#v", sched)
	}
	if sched.ScheduleTargets[0].Label != "blue" || sched.ScheduleTargets[1].IntervalSeconds != 120 {
		t.Fatalf("unexpected schedule target payload: %#v", sched.ScheduleTargets)
	}
}

func TestCreateScheduleRejectsMissingTargets(t *testing.T) {
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","resolve_on_agent":true}`))
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

func TestCreateJobNormalizesServerOwnedArgsAndResolveLocation(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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

func TestCreateJobWithAgentResolveAutoDomainDoesNotFilterProtocols(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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

	st := store.NewMemory()
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

	st := store.NewMemory()
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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

func TestManagedRuntimeSettingsApplyToNewJobs(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	st := store.NewMemory()
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

func TestManagedRateLimitEndpointUsesLowercaseJSONAndDoesNotPersistInvalidConfig(t *testing.T) {
	st := store.NewMemory()
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

func TestRequiredAuthRejectsRequestsWhenTokenSetIsEmpty(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
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

	body = []byte(`{"name":"edge"}`)
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
	if savedRegister.ID == "" || savedRegister.Token == "" || savedRegister.Name != "edge" {
		t.Fatalf("register token was not generated: %#v", savedRegister)
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
	if savedRegister.Token != "" || savedRegister.ID == "" || savedRegister.Name != "edge-renamed" {
		t.Fatalf("unchanged register token should stay redacted: %#v", savedRegister)
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

func TestManagedLabelsEndpointKeepsAgentRuntimeAndRemovesSettingsRoute(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
	upsertTestAgent(t, ctx, st, "edge-grpc", []string{"old"})
	if err := st.UpsertAgentConfig(ctx, config.AgentConfig{ID: "edge-grpc", Disabled: true, Labels: []string{"old"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertHTTPAgent(ctx, config.HTTPAgent{ID: "edge-http", Enabled: false, BaseURL: "http://agent.local", HTTPToken: "secret", Labels: []string{"old-http"}}); err != nil {
		t.Fatal(err)
	}
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body, _ := json.Marshal(managedAgentLabelsPayload{Agents: []managedAgentLabelPatch{
		{ID: "edge-grpc", Labels: []string{"new", config.AgentAllLabel, "id:edge-grpc"}},
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
	node, err := st.GetHTTPAgent(ctx, "edge-http")
	if err != nil {
		t.Fatal(err)
	}
	if node.Enabled != false || node.BaseURL != "http://agent.local" || node.HTTPToken != "secret" || !reflect.DeepEqual(node.Labels, []string{"new-http"}) {
		t.Fatalf("http node = %#v", node)
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
}

func TestManagedLabelsAndAgentLabelsUpdateAtomically(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
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
	st := store.NewMemory()
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

func TestCreateHTTPAgentRejectsDuplicateID(t *testing.T) {
	st := store.NewMemory()
	handler := New(st, policy.DefaultPolicies(), abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default(), TokenConfig{Token: "admin", Scope: TokenScope{All: true, ManageAccess: ScheduleAccessWrite}})

	body := []byte(`{"id":"edge-http","transport":"http","enabled":true,"base_url":"http://one.local","http_token":"secret"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create http agent status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body = []byte(`{"id":"edge-http","transport":"http","enabled":true,"base_url":"http://two.local","http_token":"secret"}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/manage/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate http agent status = %d, body = %s", rec.Code, rec.Body.String())
	}
	node, err := st.GetHTTPAgent(context.Background(), "edge-http")
	if err != nil {
		t.Fatal(err)
	}
	if node.BaseURL != "http://one.local" {
		t.Fatalf("duplicate create should not overwrite existing http agent: %#v", node)
	}
}

func TestManageWriteAuditsAllowedAndDeniedActions(t *testing.T) {
	st := store.NewMemory()
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
	st := store.NewMemory()
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
	if err := st.UpsertHTTPAgent(ctx, config.HTTPAgent{ID: "edge-http", Enabled: true, BaseURL: "http://agent.local", HTTPToken: "http-secret"}); err != nil {
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
	if len(agents) != 1 || agents[0].HTTP == nil || agents[0].HTTP.HTTPToken != "" {
		t.Fatalf("read-only agents leaked http token: %#v", agents)
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
	if agent.HTTP == nil || agent.HTTP.HTTPToken != "" {
		t.Fatalf("read-only agent detail leaked http token: %#v", agent)
	}
}

func TestManagedAgentsSeparateHTTPAndGRPCTransports(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Now().UTC()
	for _, agent := range []model.Agent{
		{ID: "edge-grpc", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-http", Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertHTTPAgent(ctx, config.HTTPAgent{ID: "edge-http", Enabled: true, BaseURL: "http://agent.local", HTTPToken: "secret", Labels: []string{"configured"}}); err != nil {
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
	byType := map[string][]ManagedAgent{}
	for _, agent := range agents {
		byType[agent.Type] = append(byType[agent.Type], agent)
	}
	grpcAgents := byType["grpc"]
	if len(grpcAgents) != 1 || grpcAgents[0].ID != "edge-grpc" || grpcAgents[0].Transport != "grpc" {
		t.Fatalf("grpc agents = %#v", grpcAgents)
	}
	if !stringListContains(grpcAgents[0].Labels, "configured-grpc") || stringListContains(grpcAgents[0].Labels, "grpc-live") {
		t.Fatalf("grpc managed labels should come from server config: %#v", grpcAgents[0].Labels)
	}
	httpAgents := byType["http"]
	if len(httpAgents) != 1 || httpAgents[0].ID != "edge-http" || httpAgents[0].Transport != "http" {
		t.Fatalf("http agents = %#v", httpAgents)
	}
	if !reflect.DeepEqual(httpAgents[0].Labels, []string{"agent", "id:edge-http", "configured"}) {
		t.Fatalf("http labels = %#v", httpAgents[0].Labels)
	}
	if httpAgents[0].HTTP == nil || !reflect.DeepEqual(httpAgents[0].HTTP.Labels, []string{"configured"}) {
		t.Fatalf("http config = %#v", httpAgents[0].HTTP)
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
