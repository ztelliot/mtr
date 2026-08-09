package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
	"github.com/ztelliot/mtr/internal/tlsutil"
)

func TestCallOutboundAgentConsumesStreamingEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/x-ndjson" {
			t.Fatalf("accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		lines := []any{
			map[string]any{"type": "message", "message": "started"},
			map[string]any{"type": "summary", "exit_code": 0, "metric": map[string]any{"packets_received": 1}},
		}
		for _, line := range lines {
			if err := json.NewEncoder(w).Encode(line); err != nil {
				t.Fatal(err)
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	var got []grpcwire.ResultEvent
	err := callOutboundAgent(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()}, &grpcwire.JobSpec{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1"}, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d", len(got))
	}
	if got[0].Event["type"] != "message" || got[1].Event["type"] != "summary" {
		t.Fatalf("events = %#v", got)
	}
	if got[0].JobID != "job-1" || got[0].AgentID != "edge-http" {
		t.Fatalf("server did not restore metadata: %#v", got[0])
	}
}

func TestAuthorizeOutboundRequestEmitsAgentHTTPAuthHeaders(t *testing.T) {
	tests := []struct {
		name       string
		auth       config.OutboundAgentAuth
		headerName string
		want       string
	}{
		{
			name:       "bearer",
			auth:       config.OutboundAgentAuth{Type: config.OutboundAuthBearer, Secret: "shared-secret"},
			headerName: "Authorization",
			want:       "Bearer shared-secret",
		},
		{
			name:       "custom header",
			auth:       config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "X-MTR-Agent-Token", Secret: "shared-secret"},
			headerName: "X-MTR-Agent-Token",
			want:       "shared-secret",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "http://agent.example/invoke", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := authorizeOutboundRequest(req, test.auth); err != nil {
				t.Fatal(err)
			}
			if got := req.Header.Get(test.headerName); got != test.want {
				t.Fatalf("%s = %q, want %q", test.headerName, got, test.want)
			}
		})
	}
}

func TestApplyOutboundHeadersAllowsManagedHTTPAgent(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://agent.example/invoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyOutboundHeaders(req, map[string]string{"X-Provider-Version": "v1"}); err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("X-Provider-Version"); got != "v1" {
		t.Fatalf("X-Provider-Version = %q", got)
	}
}

func TestAuthorizeOutboundRequestRejectsProxyIdentityAuthHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://agent.example/invoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = authorizeOutboundRequest(req, config.OutboundAgentAuth{
		Type: config.OutboundAuthHeader, Header: "X-Forwarded-For", Secret: "shared-secret",
	})
	if err == nil {
		t.Fatal("proxy identity auth header was accepted")
	}
}

func TestDecodeOutboundAgentLineRejectsNonCanonicalType(t *testing.T) {
	if _, err := decodeOutboundAgentLine([]byte(`{"type":" summary ","exit_code":0}`), "job-1", "edge-http"); err == nil {
		t.Fatal("expected padded event type to fail")
	}
}

func TestCallOutboundAgentDoesNotReplayAcceptedRequestAfterDisconnect(t *testing.T) {
	oldDelay := httpTransportInvokeRetryDelay
	httpTransportInvokeRetryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	defer func() { httpTransportInvokeRetryDelay = oldDelay }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatal(err)
		}
		if atomic.AddInt32(&attempts, 1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer cannot hijack")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
	}))
	defer srv.Close()

	var got []grpcwire.ResultEvent
	err := callOutboundAgent(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()}, &grpcwire.JobSpec{ID: "job-1"}, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err == nil || !isOutboundAgentConnectionError(err) {
		t.Fatalf("error = %v, want connection error", err)
	}
	if count := atomic.LoadInt32(&attempts); count != 1 || len(got) != 0 {
		t.Fatalf("attempts=%d events=%d", count, len(got))
	}
}

func TestCallOutboundAgentRetriesOnlyBeforeRequestStarts(t *testing.T) {
	oldDelay := httpTransportInvokeRetryDelay
	httpTransportInvokeRetryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { httpTransportInvokeRetryDelay = oldDelay }()

	var handlerCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&handlerCalls, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
	}))
	defer srv.Close()

	var dialAttempts int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		if atomic.AddInt32(&dialAttempts, 1) == 1 {
			return nil, errors.New("dial failed before request write")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client := &http.Client{Transport: transport}
	var got []grpcwire.ResultEvent
	err := callOutboundAgentWithAttempts(context.Background(), OutboundAgent{
		Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		ID: "edge-http", URL: srv.URL, HTTPClient: client,
	}, &grpcwire.JobSpec{ID: "job-1"}, 2, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if dials, calls := atomic.LoadInt32(&dialAttempts), atomic.LoadInt32(&handlerCalls); dials != 2 || calls != 1 || len(got) != 1 {
		t.Fatalf("dial attempts=%d handler calls=%d events=%d", dials, calls, len(got))
	}
}

func TestCallOutboundAgentDoesNotRetryWithoutWriteTrace(t *testing.T) {
	var attempts int32
	client := &http.Client{Transport: eventRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		if req.GetBody != nil {
			t.Error("invoke request remained replayable")
		}
		return nil, errors.New("custom transport connection failed")
	})}

	err := callOutboundAgentWithAttempts(context.Background(), OutboundAgent{
		Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		ID: "edge-http", URL: "https://agent.example", HTTPClient: client,
	}, &grpcwire.JobSpec{ID: "job-1"}, 3, func(grpcwire.ResultEvent) {})
	if err == nil || !isOutboundAgentConnectionError(err) {
		t.Fatalf("error = %v, want connection error", err)
	}
	if count := atomic.LoadInt32(&attempts); count != 1 {
		t.Fatalf("attempts = %d, want 1", count)
	}
}

func TestInvokeOutboundAgentFailsAcceptedRequestAfterDisconnectWithoutReplay(t *testing.T) {
	oldDelay := httpTransportInvokeRetryDelay
	httpTransportInvokeRetryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { httpTransportInvokeRetryDelay = oldDelay }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Fatal(err)
		}
		atomic.AddInt32(&calls, 1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer cannot hijack")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	job := model.Job{ID: "http-disconnect", AgentID: "edge-http", Tool: model.ToolPing, Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.SetOutboundAgentRuntime(time.Minute, 3)
	unlockAgent := hub.lockAgentLifecycles(job.AgentID)
	hub.mu.Lock()
	_, generation := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
	hub.mu.Unlock()
	unlockAgent()
	attempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
	if !ok {
		t.Fatal("mark outbound job inflight")
	}
	outcomes := make(chan outboundInvokeOutcome, 1)
	hub.invokeOutboundAgent(ctx, OutboundAgent{
		Transport: config.OutboundTransportHTTP,
		Auth:      config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: job.AgentID, URL: srv.URL, HTTPClient: srv.Client(),
	}, &grpcwire.JobSpec{ID: job.ID, TimeoutSeconds: 1}, outcomes, 1, generation, attempt)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("invoke calls = %d, want 1", got)
	}
	stored, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.JobFailed || stored.Error != model.JobErrorAgentDisconnected {
		t.Fatalf("job after disconnect = %#v", stored)
	}
	if hub.inflightCount(job.AgentID) != 0 {
		t.Fatal("disconnected job remained inflight")
	}
	select {
	case outcome := <-outcomes:
		if outcome.kind != outboundInvokeConnectionFailed {
			t.Fatalf("outcome = %v, want connection failure", outcome.kind)
		}
	default:
		t.Fatal("missing outbound outcome")
	}
}

func TestInvokeOutboundAgentRequeuesBusyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	ctx := context.Background()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	job := model.Job{ID: "http-busy", AgentID: "edge-http", Tool: model.ToolPing, Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	unlockAgent := hub.lockAgentLifecycles(job.AgentID)
	hub.mu.Lock()
	_, generation := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
	hub.mu.Unlock()
	unlockAgent()
	attempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
	if !ok {
		t.Fatal("mark outbound job inflight")
	}
	hub.invokeOutboundAgent(ctx, OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: job.AgentID, URL: srv.URL, HTTPClient: srv.Client()}, &grpcwire.JobSpec{ID: job.ID, TimeoutSeconds: 1}, make(chan outboundInvokeOutcome, 1), 1, generation, attempt)
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobQueued || got.Error != "" {
		t.Fatalf("busy HTTP job was not requeued: %#v", got)
	}
}

func TestCallOutboundAgentRejectsUntypedEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
	}))
	defer srv.Close()

	err := callOutboundAgent(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("expected untyped event to fail")
	}
}

func TestCallOutboundAgentRequiresSummaryBeforeEOF(t *testing.T) {
	for name, lines := range map[string][]map[string]any{
		"empty":    nil,
		"progress": {{"type": "message", "message": "still running"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				for _, line := range lines {
					_ = json.NewEncoder(w).Encode(line)
				}
			}))
			defer srv.Close()
			err := callOutboundAgent(context.Background(), OutboundAgent{
				Transport: config.OutboundTransportHTTP,
				Auth:      config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client(),
			}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
			if err == nil || isOutboundAgentConnectionError(err) {
				t.Fatalf("error = %v, want non-connection protocol error", err)
			}
		})
	}
}

func TestOutboundAgentNDJSONLineLimit(t *testing.T) {
	reader := bufio.NewReader(bytes.NewReader(bytes.Repeat([]byte("x"), httpTransportNDJSONLineLimit+1)))
	if _, err := readOutboundAgentNDJSONLine(reader); err == nil {
		t.Fatal("expected oversized event line to fail")
	}
}

func TestCallHTTPTransportRejectsBlankAndPaddedNDJSONLines(t *testing.T) {
	for name, body := range map[string]string{
		"blank":  "\n",
		"padded": " {\"type\":\"summary\",\"exit_code\":0}\n",
	} {
		t.Run(name, func(t *testing.T) {
			agent := OutboundAgent{
				ID: "edge-http", Transport: config.OutboundTransportHTTP, URL: "https://agent.example",
				Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
				HTTPClient: &http.Client{Transport: eventRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
				})},
			}
			err := callHTTPTransportOnce(context.Background(), agent, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
			if err == nil || !strings.Contains(err.Error(), "non-canonical") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOutboundAgentApplyHealthRejectsIdentityMismatch(t *testing.T) {
	agent := OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http"}
	if err := agent.applyHealth(outboundAgentHealth{}); err == nil {
		t.Fatal("expected missing identity")
	}
	if err := agent.applyHealth(outboundAgentHealth{AgentID: "other"}); err == nil {
		t.Fatal("expected identity mismatch")
	}
	if err := agent.applyHealth(outboundAgentHealth{
		ProtocolVersion:   grpcwire.AgentProtocolVersion,
		AgentID:           "edge-http",
		Capabilities:      []model.Tool{model.ToolPing},
		Protocols:         model.ProtocolAll,
		MaxConcurrentJobs: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if agent.ProtocolVersion != grpcwire.AgentProtocolVersion || len(agent.Capabilities) != 1 || agent.Capabilities[0] != model.ToolPing || agent.MaxConcurrentJobs != 4 {
		t.Fatalf("health not applied: %#v", agent)
	}
	if err := agent.applyHealth(outboundAgentHealth{AgentID: "edge-http"}); err != nil {
		return
	}
	t.Fatal("missing protocol metadata must be rejected")
}

func TestCheckOutboundAgentHealthRequiresMatchingAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"version": map[string]any{"version": "test"},
			"agent": map[string]any{
				"protocol_version":    grpcwire.AgentProtocolVersion,
				"id":                  "other",
				"capabilities":        []string{"ping"},
				"protocols":           3,
				"max_concurrent_jobs": 4,
			},
		})
	}))
	defer srv.Close()

	err := checkOutboundAgentHealth(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()})
	if err == nil {
		t.Fatal("expected mismatched health identity to fail")
	}
}

func TestCheckOutboundAgentHealthRejectsInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	err := checkOutboundAgentHealth(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()})
	if err == nil {
		t.Fatal("expected invalid health JSON to fail")
	}
}

func TestDecodeOutboundAgentHealthRejectsMissingProtocolVersion(t *testing.T) {
	if _, err := decodeOutboundAgentHealth([]byte(`{"status":"ok","version":{"version":"test"},"agent":{"id":"old-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`)); err == nil {
		t.Fatal("missing protocol version must be rejected")
	}
}

func TestDecodeOutboundAgentHealthRejectsStringVersion(t *testing.T) {
	if _, err := decodeOutboundAgentHealth([]byte(`{"status":"ok","version":"legacy","agent":{"protocol_version":1,"id":"old-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`)); err == nil {
		t.Fatal("string health version must be rejected")
	}
}

func TestDecodeOutboundAgentHealthRejectsNonCanonicalStatus(t *testing.T) {
	if _, err := decodeOutboundAgentHealth([]byte(`{"status":" ok ","version":{"version":"test"},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`)); err == nil {
		t.Fatal("padded health status must be rejected")
	}
}

func TestDecodeOutboundAgentHealthRejectsUnknownFields(t *testing.T) {
	tests := []string{
		`{"status":"ok","unknown":true,"version":{"version":"test"},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`,
		`{"status":"ok","version":{"version":"test","unknown":true},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`,
		`{"status":"ok","version":{"version":"test"},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4,"unknown":true}}`,
		`{"status":"ok","version":{"version":"test"},"network":{"unknown":true},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`,
	}
	for _, body := range tests {
		if _, err := decodeOutboundAgentHealth([]byte(body)); err == nil {
			t.Fatalf("unknown health field was accepted: %s", body)
		}
	}
}

func TestDecodeOutboundAgentHealthAcceptsCurrentNetworkPayload(t *testing.T) {
	body := []byte(`{"status":"ok","version":{"version":"test"},"network":{"cap_net_raw":{"effective":true,"bounded":true},"icmp_echo_datagram":{"ipv4":{"available":true},"ipv6":{"available":false,"error":"not permitted"}},"ping_group_range":"0 2147483647"},"agent":{"protocol_version":1,"id":"edge-http","capabilities":["ping"],"protocols":3,"max_concurrent_jobs":4}}`)
	if _, err := decodeOutboundAgentHealth(body); err != nil {
		t.Fatal(err)
	}
}

func TestCallOutboundAgentHTTPErrorIsNotConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := callOutboundAgent(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if isOutboundAgentConnectionError(err) {
		t.Fatalf("HTTP status error should not mark agent offline: %v", err)
	}
}

func TestOutboundAgentHTTPURLRejectsQuery(t *testing.T) {
	if _, err := httpTransportURL("https://agent.example.com/path?token=nope", "/healthz"); err == nil {
		t.Fatal("expected query-bearing outbound URL to be rejected")
	}
}

func TestCheckOutboundAgentHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"version": map[string]any{
				"version": "v1.2.3",
				"commit":  "abc123",
			},
			"agent": map[string]any{
				"protocol_version":    grpcwire.AgentProtocolVersion,
				"id":                  "edge-http",
				"country":             "CN",
				"region":              "edge",
				"provider":            "kubernetes",
				"isp":                 "bgp",
				"capabilities":        []string{"ping", "dns"},
				"protocols":           3,
				"max_concurrent_jobs": 4,
			},
		})
	}))
	defer srv.Close()

	health, err := probeOutboundAgentHealth(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if health.Version != "v1.2.3 abc123" {
		t.Fatalf("version = %q", health.Version)
	}
	if health.Country != "CN" || health.Region != "edge" || health.Provider != "kubernetes" || health.ISP != "bgp" || health.Protocols != model.ProtocolAll || len(health.Capabilities) != 2 || health.MaxConcurrentJobs != 4 {
		t.Fatalf("agent profile not decoded: %#v", health)
	}
	if health.ProtocolVersion != grpcwire.AgentProtocolVersion {
		t.Fatalf("protocol version = %d", health.ProtocolVersion)
	}
}

func TestOutboundAgentHTTPSMutualTLSTrustsChainWithoutEndpointSAN(t *testing.T) {
	files := writeOutboundAgentMTLSFiles(t)
	assertOutboundAgentHTTPSMutualTLS(t, files, "")
}

func TestOutboundAgentHTTPSMutualTLSUsesServerNameOnlyAsSNI(t *testing.T) {
	files := writeOutboundAgentMTLSFilesWithServerNames(t, nil, []string{"shared-certificate.invalid"})
	assertOutboundAgentHTTPSMutualTLS(t, files, "agent.mtr.svc")
}

func assertOutboundAgentHTTPSMutualTLS(t *testing.T, files outboundMTLSFiles, serverName string) {
	t.Helper()
	serverCert, err := tls.LoadX509KeyPair(files.serverCert, files.serverKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	caPEM, err := os.ReadFile(files.ca)
	if err != nil {
		t.Fatal(err)
	}
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load test ca")
	}
	srv := newUnstartedPrivateManagedAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Fatal("expected client certificate")
		}
		if r.TLS.ServerName != serverName {
			t.Errorf("TLS SNI = %q, want %q", r.TLS.ServerName, serverName)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	client, err := NewOutboundAgentHTTPClient(OutboundAgentTLS{
		CAFiles:    []string{files.ca},
		CertFile:   files.clientCert,
		KeyFile:    files.clientKey,
		ServerName: serverName,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []grpcwire.ResultEvent
	err = callOutboundAgent(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: srv.URL, HTTPClient: client}, &grpcwire.JobSpec{ID: "job-1"}, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Event["type"] != "summary" {
		t.Fatalf("events = %#v", got)
	}
}

func TestOutboundAgentHTTPSRejectsUntrustedCertificateChain(t *testing.T) {
	serverFiles := writeOutboundAgentMTLSFiles(t)
	untrustedFiles := writeOutboundAgentMTLSFiles(t)
	serverCert, err := tls.LoadX509KeyPair(serverFiles.serverCert, serverFiles.serverKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCAs := x509.NewCertPool()
	caPEM, err := os.ReadFile(serverFiles.ca)
	if err != nil {
		t.Fatal(err)
	}
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to load test ca")
	}
	srv := newUnstartedPrivateManagedAgentTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached Agent with an untrusted server chain")
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	client, err := NewOutboundAgentHTTPClient(OutboundAgentTLS{
		CAFiles:  []string{untrustedFiles.ca},
		CertFile: serverFiles.clientCert,
		KeyFile:  serverFiles.clientKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = callOutboundAgent(context.Background(), OutboundAgent{
		Transport:  config.OutboundTransportHTTP,
		Auth:       config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		ID:         "edge-http",
		URL:        srv.URL,
		HTTPClient: client,
	}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("untrusted outbound Agent certificate chain was accepted")
	}
}

func TestCheckOutboundAgentHealthRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := checkOutboundAgentHealth(context.Background(), OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, URL: srv.URL, HTTPClient: srv.Client()})
	if err == nil {
		t.Fatal("expected non-2xx health check to fail")
	}
}

func TestOutboundAgentClientDoesNotFollowHealthOrInvokeRedirects(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedRequests.Add(1)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	customClient := &http.Client{}
	agent := OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "edge-http", URL: redirector.URL, HTTPClient: customClient}
	if _, err := probeOutboundAgentHealth(context.Background(), agent); err == nil {
		t.Fatal("expected redirected health check to fail")
	}
	if err := callOutboundAgent(context.Background(), agent, &grpcwire.JobSpec{ID: "job-redirect"}, func(grpcwire.ResultEvent) {}); err == nil {
		t.Fatal("expected redirected invoke to fail")
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("followed %d untrusted redirects", got)
	}
}

func TestOutboundAgentClientDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	client, err := NewOutboundAgentHTTPClient(OutboundAgentTLS{})
	if err != nil {
		t.Fatal(err)
	}
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatalf("expected proxy-disabled transport, got %#v", client.Transport)
	}
}

func TestManagedAgentEndpointAddressSetFailsClosed(t *testing.T) {
	allowed := []netip.Addr{
		netip.MustParseAddr("10.0.0.2"),
		netip.MustParseAddr("fd12:3456::2"),
		netip.MustParseAddr("1.1.1.1"),
	}
	if err := validateManagedAgentEndpointAddresses(allowed); err != nil {
		t.Fatalf("trusted VPC and public endpoints were rejected: %v", err)
	}
	for _, forbidden := range []string{"127.0.0.1", "169.254.169.254", "100.100.100.200", "fd00:ec2::254"} {
		addresses := append(append([]netip.Addr(nil), allowed...), netip.MustParseAddr(forbidden))
		if err := validateManagedAgentEndpointAddresses(addresses); err == nil {
			t.Fatalf("mixed DNS result containing %s was accepted", forbidden)
		}
	}
	if err := validateManagedAgentEndpointAddresses(nil); err == nil {
		t.Fatal("empty DNS result was accepted")
	}
}

func TestOutboundAgentFallbackClientDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	client := outboundHTTPClient(OutboundAgent{Transport: config.OutboundTransportHTTP})
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatalf("expected proxy-disabled fallback transport, got %#v", client.Transport)
	}
}

func TestOutboundAgentLoopChecksHealthAndKeepsStartupVersion(t *testing.T) {
	var healthCalls int32
	var invokeCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			atomic.AddInt32(&healthCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"version": map[string]any{"version": "v9.8.7"},
				"agent": map[string]any{
					"protocol_version":    grpcwire.AgentProtocolVersion,
					"id":                  "edge-http",
					"country":             "CN",
					"region":              "edge",
					"provider":            "kubernetes",
					"isp":                 "bgp",
					"capabilities":        []string{"ping"},
					"protocols":           3,
					"max_concurrent_jobs": 4,
				},
			})
		case "/invoke":
			atomic.AddInt32(&invokeCalls, 1)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, 10*time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-http", Status: model.JobQueued, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	go hub.outboundAgentLoop(ctx, OutboundAgent{Transport: config.OutboundTransportHTTP,
		ID:         "edge-http",
		URL:        srv.URL,
		Auth:       config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		HTTPClient: srv.Client(),
	})

	deadline := time.After(2 * time.Second)
	for {
		got, err := st.GetJob(ctx, "job-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == model.JobSucceeded {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job status = %s", got.Status)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if got := atomic.LoadInt32(&invokeCalls); got != 1 {
		t.Fatalf("invoke calls = %d", got)
	}
	if got := atomic.LoadInt32(&healthCalls); got != 1 {
		t.Fatalf("health checks = %d, want one startup probe before the refresh interval", got)
	}
	agents, err := st.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Version != "v9.8.7" || agents[0].Country != "CN" || agents[0].Region != "edge" || agents[0].Provider != "kubernetes" || agents[0].ISP != "bgp" || agents[0].Protocols != model.ProtocolAll || len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != model.ToolPing {
		t.Fatalf("agent profile not stored: %#v", agents)
	}
}

func TestReloadOutboundAgentsRestartsChangedIDWithoutStaleOfflineCleanup(t *testing.T) {
	newInvokeCalls := atomic.Int32{}
	oldInvokeCalls := atomic.Int32{}
	newPeer := newPrivateManagedAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"version": map[string]any{"version": "test"},
				"agent": map[string]any{
					"protocol_version":    grpcwire.AgentProtocolVersion,
					"id":                  "reload-http",
					"capabilities":        []string{"http"},
					"protocols":           3,
					"max_concurrent_jobs": 4,
				},
			})
		case "/invoke":
			newInvokeCalls.Add(1)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer newPeer.Close()
	oldPeer := newPrivateManagedAgentTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"version": map[string]any{"version": "test"},
				"agent": map[string]any{
					"protocol_version":    grpcwire.AgentProtocolVersion,
					"id":                  "reload-http",
					"capabilities":        []string{"http"},
					"protocols":           3,
					"max_concurrent_jobs": 4,
				},
			})
		case "/invoke":
			oldInvokeCalls.Add(1)
			http.Error(w, "stale endpoint", http.StatusGone)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oldPeer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewTestMemory()
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "reload-http", Enabled: true, URL: oldPeer.URL}); err != nil {
		t.Fatal(err)
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, 10*time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	hub.ReloadOutboundAgents(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		hub.mu.Lock()
		oldGeneration := hub.outboundAgentGenerations["reload-http"]
		hub.mu.Unlock()
		if oldGeneration != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old HTTP loop did not start")
		}
		time.Sleep(time.Millisecond)
	}

	hub.mu.Lock()
	oldGeneration := hub.outboundAgentGenerations["reload-http"]
	hub.mu.Unlock()
	if err := st.UpsertOutboundAgent(ctx, config.OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: "reload-http", Enabled: true, URL: newPeer.URL}); err != nil {
		t.Fatal(err)
	}
	hub.ReloadOutboundAgents(ctx)
	for {
		hub.mu.Lock()
		newGeneration := hub.outboundAgentGenerations["reload-http"]
		capacity := hub.meta["reload-http"].maxConcurrentJobs
		hub.mu.Unlock()
		if newGeneration != 0 && newGeneration != oldGeneration && capacity == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement HTTP loop did not publish current protocol metadata")
		}
		time.Sleep(time.Millisecond)
	}
	hub.mu.Lock()
	newGeneration := hub.outboundAgentGenerations["reload-http"]
	hub.mu.Unlock()
	if newGeneration == 0 || newGeneration == oldGeneration {
		t.Fatalf("HTTP loop generation did not change: old=%d new=%d", oldGeneration, newGeneration)
	}

	now := time.Now().UTC()
	job := model.Job{ID: "reload-job", AgentID: "reload-http", Tool: model.ToolHTTP, Target: "https://example.com", Args: map[string]string{"http_version": "1.1"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := st.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == model.JobSucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement job status=%s error=%q", got.Status, got.Error)
		}
		time.Sleep(time.Millisecond)
	}
	if got := oldInvokeCalls.Load(); got != 0 {
		t.Fatalf("stale HTTP endpoint received %d invokes", got)
	}
	if got := newInvokeCalls.Load(); got != 1 {
		t.Fatalf("replacement HTTP endpoint received %d invokes", got)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Status != model.AgentOnline {
		t.Fatalf("stale loop marked replacement offline: %#v", agents)
	}
}

func TestOutboundAgentLoopDispatchesCurrentProtocolPeers(t *testing.T) {
	type peer struct {
		id      string
		invokes int32
		server  *httptest.Server
	}
	makePeer := func(id string) *peer {
		p := &peer{id: id}
		p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status":  "ok",
					"version": map[string]any{"version": "test"},
					"agent": map[string]any{
						"protocol_version":    grpcwire.AgentProtocolVersion,
						"id":                  p.id,
						"capabilities":        []string{"http"},
						"protocols":           3,
						"max_concurrent_jobs": 4,
					},
				})
			case "/invoke":
				atomic.AddInt32(&p.invokes, 1)
				w.Header().Set("Content-Type", "application/x-ndjson")
				_ = json.NewEncoder(w).Encode(map[string]any{"type": "summary", "exit_code": 0})
			default:
				http.NotFound(w, r)
			}
		}))
		return p
	}
	oldPeer := makePeer("peer-a")
	defer oldPeer.server.Close()
	newPeer := makePeer("peer-b")
	defer newPeer.server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := store.NewTestMemory()
	now := time.Now().UTC()
	for _, job := range []model.Job{
		{ID: "old-job", AgentID: oldPeer.id, Tool: model.ToolHTTP, Target: "https://example.com", Args: map[string]string{"http_version": "auto"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now},
		{ID: "new-job", AgentID: newPeer.id, Tool: model.ToolHTTP, Target: "https://example.com", Args: map[string]string{"http_version": "auto"}, Status: model.JobQueued, CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, 10*time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go hub.outboundAgentLoop(ctx, OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: oldPeer.id, URL: oldPeer.server.URL, HTTPClient: oldPeer.server.Client()})
	go hub.outboundAgentLoop(ctx, OutboundAgent{Transport: config.OutboundTransportHTTP, Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, ID: newPeer.id, URL: newPeer.server.URL, HTTPClient: newPeer.server.Client()})

	deadline := time.After(2 * time.Second)
	for {
		oldJob, err := st.GetJob(ctx, "old-job")
		if err != nil {
			t.Fatal(err)
		}
		newJob, err := st.GetJob(ctx, "new-job")
		if err != nil {
			t.Fatal(err)
		}
		if oldJob.Status == model.JobSucceeded && newJob.Status == model.JobSucceeded {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("old=%s/%q new=%s/%q", oldJob.Status, oldJob.Error, newJob.Status, newJob.Error)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if calls := atomic.LoadInt32(&oldPeer.invokes); calls != 1 {
		t.Fatalf("first HTTP agent invokes = %d, want 1", calls)
	}
	if calls := atomic.LoadInt32(&newPeer.invokes); calls != 1 {
		t.Fatalf("second HTTP agent invokes = %d, want 1", calls)
	}
}

type outboundMTLSFiles struct {
	ca         string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

func writeOutboundAgentMTLSFiles(t *testing.T) outboundMTLSFiles {
	t.Helper()
	return writeOutboundAgentMTLSFilesWithServerNames(t, nil, []string{"shared-agent-certificate.invalid"})
}

func newPrivateManagedAgentTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := newUnstartedPrivateManagedAgentTestServer(t, handler)
	server.Start()
	return server
}

func newUnstartedPrivateManagedAgentTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: privateManagedAgentTestIP(t)})
	if err != nil {
		t.Fatalf("listen on private managed-Agent test address: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	if err := server.Listener.Close(); err != nil {
		listener.Close()
		t.Fatalf("close default test listener: %v", err)
	}
	server.Listener = listener
	return server
}

func privateManagedAgentTestIP(t *testing.T) net.IP {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list interface addresses: %v", err)
	}
	for _, address := range addresses {
		network, ok := address.(*net.IPNet)
		if !ok {
			continue
		}
		ip := network.IP.To4()
		if ip == nil {
			continue
		}
		parsed, ok := netip.AddrFromSlice(ip)
		if ok && parsed.IsPrivate() {
			return append(net.IP(nil), ip...)
		}
	}
	t.Skip("test host has no private IPv4 interface")
	return nil
}

func TestOutboundAgentConfigHashTracksTLSFileContent(t *testing.T) {
	files := writeOutboundAgentMTLSFiles(t)
	node := config.OutboundAgent{
		ID: "edge-tls", Transport: config.OutboundTransportHTTP, URL: "https://agent.example",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		TLS:  config.TLS{Enabled: true, CAFiles: []string{files.ca}, CertFile: files.clientCert, KeyFile: files.clientKey},
	}
	firstMaterial, err := tlsutil.MaterialFingerprint(node.TLS.CAFiles, node.TLS.CertFile, node.TLS.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := outboundAgentConfigHash(node, firstMaterial)
	file, err := os.OpenFile(files.ca, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	secondMaterial, err := tlsutil.MaterialFingerprint(node.TLS.CAFiles, node.TLS.CertFile, node.TLS.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if secondHash := outboundAgentConfigHash(node, secondMaterial); secondHash == firstHash {
		t.Fatal("outbound config hash did not change after TLS material rotation")
	}
}

func writeOutboundAgentMTLSFilesWithServerNames(t *testing.T, serverIPs []net.IP, serverDNSNames []string) outboundMTLSFiles {
	t.Helper()
	dir := t.TempDir()
	caKey := newTestKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mtr test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	writeTestPEM(t, caPath, "CERTIFICATE", caDER)

	serverCert, serverKey := newSignedTestCert(t, caTemplate, caKey, x509.ExtKeyUsageServerAuth, serverIPs, serverDNSNames)
	serverCertPath := filepath.Join(dir, "server.pem")
	serverKeyPath := filepath.Join(dir, "server-key.pem")
	writeTestPEM(t, serverCertPath, "CERTIFICATE", serverCert)
	writeTestPEM(t, serverKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(serverKey))

	clientCert, clientKey := newSignedTestCert(t, caTemplate, caKey, x509.ExtKeyUsageClientAuth, nil, nil)
	clientCertPath := filepath.Join(dir, "client.pem")
	clientKeyPath := filepath.Join(dir, "client-key.pem")
	writeTestPEM(t, clientCertPath, "CERTIFICATE", clientCert)
	writeTestPEM(t, clientKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(clientKey))

	return outboundMTLSFiles{
		ca:         caPath,
		serverCert: serverCertPath,
		serverKey:  serverKeyPath,
		clientCert: clientCertPath,
		clientKey:  clientKeyPath,
	}
}

func newSignedTestCert(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, usage x509.ExtKeyUsage, ips []net.IP, dnsNames []string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key := newTestKey(t)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mtr test leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
		DNSNames:     dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func newTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func writeTestPEM(t *testing.T, path string, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

func TestNextOutboundAgentHealthIntervalBacksOffToFiveMinutes(t *testing.T) {
	if got := nextOutboundAgentHealthInterval(time.Second, time.Second, 300*time.Second); got != 2*time.Second {
		t.Fatalf("interval = %s", got)
	}
	if got := nextOutboundAgentHealthInterval(400*time.Second, time.Second, 300*time.Second); got != 300*time.Second {
		t.Fatalf("capped interval = %s", got)
	}
	if got := nextOutboundAgentHealthInterval(0, 2*time.Second, 300*time.Second); got != 4*time.Second {
		t.Fatalf("base interval = %s", got)
	}
	if got := nextOutboundAgentHealthInterval(10*time.Second, time.Second, 15*time.Second); got != 15*time.Second {
		t.Fatalf("custom capped interval = %s", got)
	}
}

func TestHubRestoresCompactGRPCResultEnvelope(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, "edge-1", job.ID)

	hub.handleAgentResultForSession(ctx, "edge-1", session.generation, grpcwire.AgentResult{
		JobID: "job-1",
		Event: map[string]any{"type": "message", "message": "started"},
	})
	hub.handleAgentResultForSession(ctx, "edge-1", session.generation, grpcwire.AgentResult{
		JobID: "job-1",
		Event: map[string]any{"type": "summary", "exit_code": 0, "metric": map[string]any{"received": float64(1)}},
	})

	events, err := st.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].AgentID != "edge-1" || events[0].Stream != "message" || events[0].Event == nil || events[0].Event.Message != "started" {
		t.Fatalf("agent message event = %#v", events[0])
	}
	if events[1].AgentID != "edge-1" || events[1].Stream != "summary" || events[1].ExitCode == nil || events[1].Parsed == nil || events[1].Parsed.Type != "summary" || events[1].Parsed.Tool != model.ToolPing || events[1].Parsed.Summary["received"] != float64(1) {
		t.Fatalf("parsed event = %#v", events[1])
	}
	if events[2].AgentID != "" || events[2].Stream != "progress" || events[2].Event == nil || events[2].Event.Message != "completed" {
		t.Fatalf("job progress event = %#v", events[2])
	}
}

func TestHubRequeuesAgentBusyWithoutPublishingTerminalSummary(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	started := now.Add(-time.Second)
	completed := now
	job := model.Job{ID: "busy-job", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, Error: "old-error", StartedAt: &started, CompletedAt: &completed, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, "edge-1", job.ID)

	hub.handleAgentResultForSession(ctx, "edge-1", session.generation, grpcwire.AgentResult{
		JobID: job.ID,
		Event: map[string]any{"type": "summary", "exit_code": -1, "metric": map[string]any{"status": model.JobErrorAgentBusy}},
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobQueued || got.Error != "" || got.StartedAt != nil || got.CompletedAt != nil {
		t.Fatalf("busy job was not cleanly requeued: %#v", got)
	}
	events, err := st.ListJobEvents(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Parsed != nil || event.ExitCode != nil {
			t.Fatalf("busy response published terminal event: %#v", events)
		}
	}
}

func TestHubBusyRequeueDoesNotReviveCanceledJob(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()
	job := model.Job{ID: "busy-canceled", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJobStatus(ctx, job.ID, model.JobCanceled, "canceled"); err != nil {
		t.Fatal(err)
	}
	hub.requeueBusyJob(ctx, job.AgentID, job, 0, 0, 0)
	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobCanceled {
		t.Fatalf("busy response revived canceled job: %#v", got)
	}
}

func TestOutboundBusyRequeuePreservesImmediateSameGenerationRedispatch(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	barrier := &requeueBarrierStore{Store: base, requeued: make(chan struct{}), release: make(chan struct{})}
	hub := NewHub(barrier, policy.DefaultPolicies(), time.Second, time.Second, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "busy-immediate", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	_, generation := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
	hub.mu.Unlock()
	firstAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
	if !ok {
		t.Fatal("mark first outbound attempt inflight")
	}

	done := make(chan struct{})
	agent := OutboundAgent{
		ID: job.AgentID, Transport: config.OutboundTransportHTTP, URL: "https://agent.example",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone},
		HTTPClient: &http.Client{Transport: eventRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}, nil
		})},
	}
	go func() {
		hub.invokeOutboundAgent(ctx, agent, &grpcwire.JobSpec{ID: job.ID, Tool: job.Tool, TimeoutSeconds: 1}, make(chan outboundInvokeOutcome, 1), 1, generation, firstAttempt)
		close(done)
	}()
	<-barrier.requeued
	claimed, err := base.ClaimQueuedJob(ctx, job.ID, job.AgentID)
	if err != nil || !claimed {
		t.Fatalf("immediate redispatch claim = %t, err = %v", claimed, err)
	}
	secondAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
	if !ok || secondAttempt == firstAttempt {
		t.Fatalf("mark replacement attempt = %d, ok = %t", secondAttempt, ok)
	}
	secondCancel := func() {}
	if !hub.setInflightCancelForOutboundAttempt(job.ID, secondAttempt, secondCancel) {
		t.Fatal("set replacement cancellation")
	}
	close(barrier.release)
	<-done

	hub.mu.Lock()
	_, running := hub.running[job.AgentID][job.ID]
	gotGeneration := hub.outboundJobGenerations[job.ID]
	gotAttempt := hub.outboundJobAttempts[job.ID]
	gotCancel := hub.inflightCancels[job.ID]
	hub.mu.Unlock()
	if !running || gotGeneration != generation || gotAttempt != secondAttempt || gotCancel == nil {
		t.Fatalf("replacement ownership lost: running=%t generation=%d attempt=%d cancel=%v", running, gotGeneration, gotAttempt, gotCancel != nil)
	}
}

func TestStaleBusyGenerationCannotRequeueReplacement(t *testing.T) {
	t.Run("grpc", func(t *testing.T) {
		ctx := context.Background()
		st := store.NewTestMemory()
		hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		job := model.Job{ID: "grpc-stale-busy", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-grpc", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		oldSession, _ := hub.registerGRPCSession(job.AgentID, &grpcwire.AgentHello{ID: job.AgentID, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, MaxConcurrentJobs: 2}, time.Now(), make(chan string, 1))
		if !hub.markInflightForGRPCSession(job.AgentID, job.ID, oldSession.generation) {
			t.Fatal("mark old gRPC attempt")
		}
		newSession, _ := hub.registerGRPCSession(job.AgentID, &grpcwire.AgentHello{ID: job.AgentID, Capabilities: []model.Tool{model.ToolPing}, Protocols: model.ProtocolAll, MaxConcurrentJobs: 2}, time.Now(), make(chan string, 1))
		if !hub.markInflightForGRPCSession(job.AgentID, job.ID, newSession.generation) {
			t.Fatal("mark replacement gRPC attempt")
		}
		if !hub.requeueBusyJob(ctx, job.AgentID, job, oldSession.generation, 0, 0) {
			t.Fatal("stale gRPC busy result should be ignored")
		}
		assertGRPCJobOwnership(t, hub, job.AgentID, job.ID, newSession.generation)
		assertRunningJob(t, ctx, st, job.ID)
	})

	t.Run("outbound", func(t *testing.T) {
		ctx := context.Background()
		st := store.NewTestMemory()
		hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		job := model.Job{ID: "outbound-stale-busy", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-http", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		hub.mu.Lock()
		_, oldGeneration := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
		hub.mu.Unlock()
		oldAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, oldGeneration)
		if !ok {
			t.Fatal("mark old outbound attempt")
		}
		hub.mu.Lock()
		_, newGeneration := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
		hub.mu.Unlock()
		newAttempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, newGeneration)
		if !ok {
			t.Fatal("mark replacement outbound attempt")
		}
		if !hub.requeueBusyJob(ctx, job.AgentID, job, 0, oldGeneration, oldAttempt) {
			t.Fatal("stale outbound busy result should be ignored")
		}
		hub.mu.Lock()
		_, running := hub.running[job.AgentID][job.ID]
		gotGeneration := hub.outboundJobGenerations[job.ID]
		gotAttempt := hub.outboundJobAttempts[job.ID]
		hub.mu.Unlock()
		if !running || gotGeneration != newGeneration || gotAttempt != newAttempt {
			t.Fatalf("replacement outbound ownership changed: running=%t generation=%d attempt=%d", running, gotGeneration, gotAttempt)
		}
		assertRunningJob(t, ctx, st, job.ID)
	})
}

func assertGRPCJobOwnership(t *testing.T, hub *Hub, agentID string, jobID string, generation uint64) {
	t.Helper()
	hub.mu.Lock()
	_, running := hub.running[agentID][jobID]
	gotGeneration := hub.grpcJobGenerations[jobID]
	hub.mu.Unlock()
	if !running || gotGeneration != generation {
		t.Fatalf("replacement gRPC ownership changed: running=%t generation=%d", running, gotGeneration)
	}
}

func assertRunningJob(t *testing.T, ctx context.Context, st store.Store, jobID string) {
	t.Helper()
	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != model.JobRunning {
		t.Fatalf("job status = %s, want running", job.Status)
	}
}

type requeueBarrierStore struct {
	store.Store
	requeued chan struct{}
	release  chan struct{}
}

func (s *requeueBarrierStore) RequeueRunningJob(ctx context.Context, id string) (bool, error) {
	requeued, err := s.Store.RequeueRunningJob(ctx, id)
	if err != nil || !requeued {
		return requeued, err
	}
	close(s.requeued)
	select {
	case <-s.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestHubRejectsSummaryWithoutExitCode(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, "edge-1", job.ID)

	hub.handleAgentResultForSession(ctx, "edge-1", session.generation, grpcwire.AgentResult{
		JobID: "job-1",
		Event: map[string]any{"type": "summary", "metric": map[string]any{"packets_received": 1}},
	})

	events, err := st.ListJobEvents(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("malformed summary should be dropped: %#v", events)
	}
	loaded, err := st.GetJob(ctx, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != model.JobRunning {
		t.Fatalf("malformed summary changed job status: %#v", loaded)
	}
}

func TestHubPublishesFanoutChildEventsToParent(t *testing.T) {
	ctx := context.Background()
	st := store.NewTestMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	parent := model.Job{ID: "parent-1", Tool: model.ToolPing, Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	child := model.Job{ID: "child-1", ParentID: parent.ID, Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	if err := st.CreateJob(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, child); err != nil {
		t.Fatal(err)
	}
	session, _ := registerHubTestGRPCSession(t, hub, "edge-1", child.ID)

	hub.handleAgentResultForSession(ctx, "edge-1", session.generation, grpcwire.AgentResult{
		JobID: "child-1",
		Event: map[string]any{"type": "summary", "exit_code": 0, "metric": map[string]any{"packets_received": 1}},
	})

	events, err := st.ListJobEvents(ctx, "parent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("parent events = %#v", events)
	}
	if events[0].JobID != parent.ID || events[0].AgentID != "edge-1" || events[0].Parsed == nil {
		t.Fatalf("parent summary event = %#v", events[0])
	}
	if events[1].JobID != parent.ID || events[1].AgentID != "" || events[1].Event == nil || events[1].Event.Message != "completed" {
		t.Fatalf("parent progress event = %#v", events[1])
	}
	child, err = st.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err = st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != model.JobSucceeded || parent.Status != model.JobSucceeded {
		t.Fatalf("statuses parent=%s child=%s", parent.Status, child.Status)
	}
}

func TestToJobSpecUsesCalculatedTimeout(t *testing.T) {
	spec := toJobSpec(model.Job{
		ID:             "job-1",
		Tool:           model.ToolMTR,
		Target:         "example.com",
		ResolvedTarget: "1.1.1.1",
		Args:           map[string]string{"count": "20", "max_hops": "30"},
	}, policy.Policy{})
	if spec.TimeoutSeconds != int((5 * time.Minute).Seconds()) {
		t.Fatalf("timeout_seconds = %d", spec.TimeoutSeconds)
	}
	if spec.ResolvedTarget != "1.1.1.1" {
		t.Fatalf("resolved_target not propagated: %#v", spec)
	}
	if spec.Args["count"] != "10" || spec.Args["max_hops"] != "30" {
		t.Fatalf("server-owned args not normalized: %#v", spec.Args)
	}
	if spec.ResolveTimeoutSeconds != 3 {
		t.Fatalf("resolve_timeout_seconds = %d", spec.ResolveTimeoutSeconds)
	}
	if spec.ProbeTimeoutSeconds != 1 {
		t.Fatalf("probe_timeout_seconds = %d", spec.ProbeTimeoutSeconds)
	}
}

func TestToJobSpecUsesConfiguredRuntime(t *testing.T) {
	runtime := config.DefaultRuntime()
	runtime.Count = 2
	runtime.MaxHops = 4
	runtime.ProbeStepTimeoutSec = 5
	runtime.ResolveTimeoutSec = 7
	policies := policy.PoliciesWithRuntime(runtime)
	p, ok := policies.Get(model.ToolMTR)
	if !ok {
		t.Fatal("mtr policy missing")
	}

	spec := toJobSpecWithPolicies(model.Job{
		ID:   "job-1",
		Tool: model.ToolMTR,
		Args: map[string]string{"count": "20", "max_hops": "30"},
	}, p, policies)
	if spec.TimeoutSeconds != 40 {
		t.Fatalf("timeout_seconds = %d, want 40", spec.TimeoutSeconds)
	}
	if spec.Args["count"] != "2" || spec.Args["max_hops"] != "4" {
		t.Fatalf("server-owned args not configured: %#v", spec.Args)
	}
	if spec.ResolveTimeoutSeconds != 7 {
		t.Fatalf("resolve_timeout_seconds = %d, want 7", spec.ResolveTimeoutSeconds)
	}
	if spec.ProbeTimeoutSeconds != 5 {
		t.Fatalf("probe_timeout_seconds = %d, want 5", spec.ProbeTimeoutSeconds)
	}
}

func TestToJobSpecAlwaysIncludesHTTPVersion(t *testing.T) {
	policies := policy.DefaultPolicies()
	p, ok := policies.Get(model.ToolHTTP)
	if !ok {
		t.Fatal("http policy missing")
	}
	job := model.Job{
		ID:     "job-http",
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "1.1"},
	}

	spec := toJobSpecWithPolicies(job, p, policies)
	if spec.ProtocolVersion != grpcwire.AgentProtocolVersion || spec.Args["http_version"] != "1.1" || spec.Args["method"] != "HEAD" {
		t.Fatalf("HTTP spec must preserve explicit semantics: %#v", spec.Args)
	}
}
