package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestHTTPAccessLogIsStructuredAndDoesNotLeakQuerySecrets(t *testing.T) {
	const token = "sensitive-access-token"
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := New(store.NewTestMemory(), policy.DefaultPolicies(), nil, nil, "", logger,
		TokenConfig{Token: token, Scope: TokenScope{All: true}},
	)
	req := httptest.NewRequest(http.MethodGet, "/v1/permissions?access_token="+token+"&secret=do-not-log", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-ID", "external-request-123")
	req.Header.Set("User-Agent", "mtr-access-test/1.0")
	req.RemoteAddr = "203.0.113.8:4321"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Request-ID"); got != "external-request-123" {
		t.Fatalf("response request ID = %q", got)
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), "do-not-log") || strings.Contains(logs.String(), "access_token") {
		t.Fatalf("access log leaked query data: %s", logs.String())
	}
	entry := singleAccessLogEntry(t, logs.String())
	assertAccessLogValue(t, entry, "msg", "http access")
	assertAccessLogValue(t, entry, "request_id", "external-request-123")
	assertAccessLogValue(t, entry, "method", http.MethodGet)
	assertAccessLogValue(t, entry, "route", "/v1/permissions")
	assertAccessLogValue(t, entry, "path", "/v1/permissions")
	assertAccessLogValue(t, entry, "status", float64(http.StatusOK))
	assertAccessLogValue(t, entry, "client_ip", "203.0.113.8")
	assertAccessLogValue(t, entry, "auth_result", accessAuthAuthenticated)
	assertAccessLogValue(t, entry, "subject", "token:"+managedSecretHash(token))
	assertAccessLogValue(t, entry, "user_agent", "mtr-access-test/1.0")
	if value, ok := entry["response_bytes"].(float64); !ok || value <= 0 {
		t.Fatalf("response_bytes = %#v", entry["response_bytes"])
	}
	if value, ok := entry["duration_ms"].(float64); !ok || value < 0 {
		t.Fatalf("duration_ms = %#v", entry["duration_ms"])
	}
}

func TestHTTPAccessMetricsIncludeTrafficAuthenticationAndRuntime(t *testing.T) {
	const token = "metrics-reader"
	handler := New(store.NewTestMemory(), policy.DefaultPolicies(), nil, nil, "", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		TokenConfig{Token: token, Scope: TokenScope{All: true}},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("permissions status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set("Authorization", "Basic hidden")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed auth status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	assertPrometheusMetric(t, body, "mtr_http_requests_total", map[string]string{
		"method": "GET", "route": "/v1/permissions", "status_code": "200", "auth_result": accessAuthAuthenticated,
	}, "1")
	assertPrometheusMetric(t, body, "mtr_http_requests_total", map[string]string{
		"method": "GET", "route": "/v1/permissions", "status_code": "401", "auth_result": accessAuthMalformed,
	}, "1")
	assertPrometheusMetric(t, body, "mtr_http_request_duration_seconds_count", map[string]string{
		"method": "GET", "route": "/v1/permissions",
	}, "2")
	assertPrometheusMetric(t, body, "mtr_http_request_size_bytes_count", map[string]string{
		"method": "GET", "route": "/v1/permissions",
	}, "2")
	assertPrometheusMetric(t, body, "mtr_http_response_size_bytes_count", map[string]string{
		"method": "GET", "route": "/v1/permissions",
	}, "2")
	if !strings.Contains(body, "go_goroutines ") || !strings.Contains(body, "process_start_time_seconds ") {
		t.Fatalf("runtime metrics missing:\n%s", body)
	}
}

func TestHTTPAccessMetricsCollapseUnknownMethodsAndRoutes(t *testing.T) {
	handler := New(store.NewTestMemory(), policy.DefaultPolicies(), nil, nil, "", slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("CUSTOM", "/unknown/resource/123", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unmatched status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/jobs/sensitive-job-id", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dynamic route status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assertPrometheusMetric(t, body, "mtr_http_requests_total", map[string]string{
		"method": "OTHER", "route": "unmatched", "status_code": "405", "auth_result": accessAuthPublic,
	}, "1")
	assertPrometheusMetric(t, body, "mtr_http_requests_total", map[string]string{
		"method": "GET", "route": "/v1/jobs/{id}", "status_code": "404", "auth_result": accessAuthAnonymous,
	}, "1")
	if strings.Contains(body, "sensitive-job-id") {
		t.Fatalf("raw route parameter leaked into metrics:\n%s", body)
	}
}

func TestNormalizedRequestIDRejectsUnsafeValues(t *testing.T) {
	if got := normalizedRequestID("trusted-id_123"); got != "trusted-id_123" {
		t.Fatalf("safe request ID = %q", got)
	}
	if got := normalizedRequestID("unsafe request id"); got == "unsafe request id" || got == "" {
		t.Fatalf("unsafe request ID was not replaced: %q", got)
	}
	if got := normalizedRequestID(strings.Repeat("a", 129)); len(got) > 128 || got == "" {
		t.Fatalf("oversized request ID replacement = %q", got)
	}
}

func singleAccessLogEntry(t *testing.T, raw string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) != 1 {
		t.Fatalf("access log lines = %d: %s", len(lines), raw)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode access log: %v: %s", err, raw)
	}
	return entry
}

func assertAccessLogValue(t *testing.T, entry map[string]any, key string, want any) {
	t.Helper()
	if got := entry[key]; got != want {
		t.Fatalf("access log %s = %#v, want %#v; entry = %#v", key, got, want, entry)
	}
}
