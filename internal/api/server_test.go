package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/abuse"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/scheduler"
	"github.com/ztelliot/mtr/internal/store"
)

func TestStreamJobEventsSendsHistoryAndLive(t *testing.T) {
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	}), nil, "", slog.Default())

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
		t.Fatalf("version status = %d, body = %s", rec.Code, rec.Body.String())
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
		{ID: "edge-1", Capabilities: []model.Tool{model.ToolPing, model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
		{ID: "edge-2", Capabilities: []model.Tool{model.ToolPing, model.ToolHTTP}, Protocols: model.ProtocolAll, Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now},
	} {
		if err := st.UpsertAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	policies := policy.DefaultPolicies()
	headOnly := map[string]string{"method": "^(HEAD)$"}
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
	if perms.Tools[model.ToolHTTP].AllowedArgs["method"] != "^(HEAD)$" || len(perms.Agents) != 1 || perms.Agents[0] != "edge-1" {
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

	req = httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","interval_seconds":60}`))
	req.Header.Set("Authorization", "Bearer reader")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token should not create schedules, status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		req = httptest.NewRequest(method, "/v1/schedules/"+sched.ID, strings.NewReader(`{"tool":"ping","target":"1.1.1.1","interval_seconds":60}`))
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

func TestCreateScheduleAndHistory(t *testing.T) {
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), hub, "", slog.Default())

	body := `{"tool":"ping","target":"1.1.1.1","args":{"count":"1"},"interval_seconds":30}`
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
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"interval_seconds":30}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"name":"edited","enabled":false,"tool":"ping","target":"8.8.8.8","args":{"protocol":"icmp"},"interval_seconds":120}`
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
	st := store.NewMemory()
	policies := policy.DefaultPolicies()
	handler := New(st, policies, abuse.NewConfiguredLimiter(abuse.RateLimitConfig{
		Global: abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
		IP:     abuse.Limit{RequestsPerMinute: 1000, Burst: 1000},
	}), nil, "", slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"interval_seconds":30}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var sched model.ScheduledJob
	if err := json.Unmarshal(rec.Body.Bytes(), &sched); err != nil {
		t.Fatal(err)
	}

	body := `{"tool":"ping","target":"1.1.1.1","args":{"protocol":"icmp"},"interval_seconds":120}`
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

func TestCreateScheduleRejectsUnknownOrOfflinePinnedAgent(t *testing.T) {
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
			req := httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(`{"tool":"ping","target":"1.1.1.1","agent_id":"`+tc.agent+`","resolve_on_agent":true,"interval_seconds":60}`))
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
	hub := scheduler.NewHub(st, policies, "", 30*time.Second, 10*time.Millisecond, 4, slog.Default())
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
