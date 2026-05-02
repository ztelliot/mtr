package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestCallHTTPAgentConsumesStreamingEvents(t *testing.T) {
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
	err := callHTTPAgent(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL}, &grpcwire.JobSpec{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1"}, func(event grpcwire.ResultEvent) {
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

func TestCallHTTPAgentRetriesConnectionErrors(t *testing.T) {
	oldDelay := httpAgentInvokeRetryDelay
	httpAgentInvokeRetryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	defer func() { httpAgentInvokeRetryDelay = oldDelay }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	err := callHTTPAgent(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL}, &grpcwire.JobSpec{ID: "job-1"}, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || len(got) != 1 {
		t.Fatalf("attempts=%d events=%d", attempts, len(got))
	}
}

func TestCallHTTPAgentHonorsConfiguredAttemptLimit(t *testing.T) {
	oldDelay := httpAgentInvokeRetryDelay
	httpAgentInvokeRetryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { httpAgentInvokeRetryDelay = oldDelay }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
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

	err := callHTTPAgentWithAttempts(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL}, &grpcwire.JobSpec{ID: "job-1"}, 2, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("expected connection error")
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestCallHTTPAgentRejectsUntypedEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": "job-1"})
	}))
	defer srv.Close()

	err := callHTTPAgent(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("expected untyped event to fail")
	}
}

func TestCallHTTPAgentHTTPErrorIsNotConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := callHTTPAgent(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL}, &grpcwire.JobSpec{ID: "job-1"}, func(grpcwire.ResultEvent) {})
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if isHTTPAgentConnectionError(err) {
		t.Fatalf("HTTP status error should not mark agent offline: %v", err)
	}
}

func TestHTTPAgentHealthURLDefaultsToHealthz(t *testing.T) {
	got, err := httpAgentHealthURL(HTTPAgent{BaseURL: "https://agent.example.com/path?token=nope"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://agent.example.com/path/healthz" {
		t.Fatalf("health url = %q", got)
	}
}

func TestCheckHTTPAgentHealth(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization") == "Bearer secret"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"version": map[string]any{
				"version": "v1.2.3",
				"commit":  "abc123",
			},
			"agent": map[string]any{
				"country":      "CN",
				"region":       "edge",
				"provider":     "kubernetes",
				"isp":          "bgp",
				"capabilities": []string{"ping", "dns"},
				"protocols":    3,
			},
		})
	}))
	defer srv.Close()

	health, err := probeHTTPAgentHealth(context.Background(), HTTPAgent{BaseURL: srv.URL, Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAuth {
		t.Fatal("health check did not send bearer token")
	}
	if health.Version != "v1.2.3 abc123" {
		t.Fatalf("version = %q", health.Version)
	}
	if health.Country != "CN" || health.Region != "edge" || health.Provider != "kubernetes" || health.ISP != "bgp" || health.Protocols != model.ProtocolAll || len(health.Capabilities) != 2 {
		t.Fatalf("agent profile not decoded: %#v", health)
	}
}

func TestHTTPAgentHTTPSMutualTLS(t *testing.T) {
	files := writeHTTPAgentMTLSFiles(t)
	assertHTTPAgentHTTPSMutualTLS(t, files)
}

func TestHTTPAgentHTTPSMutualTLSVerifiesCAWithoutServerName(t *testing.T) {
	files := writeHTTPAgentMTLSFilesWithServerNames(t, nil, []string{"agent.mtr.svc"})
	assertHTTPAgentHTTPSMutualTLS(t, files)
}

func assertHTTPAgentHTTPSMutualTLS(t *testing.T, files httpAgentMTLSFiles) {
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
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) == 0 {
			t.Fatal("expected client certificate")
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

	client, err := NewHTTPAgentHTTPClient(HTTPAgentTLS{
		Enabled:  true,
		CAFiles:  []string{files.ca},
		CertFile: files.clientCert,
		KeyFile:  files.clientKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []grpcwire.ResultEvent
	err = callHTTPAgent(context.Background(), HTTPAgent{ID: "edge-http", BaseURL: srv.URL, HTTPClient: client}, &grpcwire.JobSpec{ID: "job-1"}, func(event grpcwire.ResultEvent) {
		got = append(got, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Event["type"] != "summary" {
		t.Fatalf("events = %#v", got)
	}
}

func TestCheckHTTPAgentHealthRejectsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := checkHTTPAgentHealth(context.Background(), HTTPAgent{BaseURL: srv.URL})
	if err == nil {
		t.Fatal("expected non-2xx health check to fail")
	}
}

func TestHTTPAgentLoopChecksHealthAndKeepsStartupVersion(t *testing.T) {
	var healthCalls int32
	var invokeCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			atomic.AddInt32(&healthCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "ok",
				"version": "v9.8.7",
				"agent": map[string]any{
					"country":      "CN",
					"region":       "edge",
					"provider":     "kubernetes",
					"isp":          "bgp",
					"capabilities": []string{"ping"},
					"protocols":    3,
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
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, 10*time.Millisecond, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-http", Status: model.JobQueued, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	go hub.httpAgentLoop(ctx, HTTPAgent{
		ID:      "edge-http",
		BaseURL: srv.URL,
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
		t.Fatalf("startup health checks = %d", got)
	}
	agents, err := st.ListAgents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Version != "v9.8.7" || agents[0].Country != "CN" || agents[0].Region != "edge" || agents[0].Provider != "kubernetes" || agents[0].ISP != "bgp" || agents[0].Protocols != model.ProtocolAll || len(agents[0].Capabilities) != 1 || agents[0].Capabilities[0] != model.ToolPing {
		t.Fatalf("agent profile not stored: %#v", agents)
	}
}

type httpAgentMTLSFiles struct {
	ca         string
	serverCert string
	serverKey  string
	clientCert string
	clientKey  string
}

func writeHTTPAgentMTLSFiles(t *testing.T) httpAgentMTLSFiles {
	t.Helper()
	return writeHTTPAgentMTLSFilesWithServerNames(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
}

func writeHTTPAgentMTLSFilesWithServerNames(t *testing.T, serverIPs []net.IP, serverDNSNames []string) httpAgentMTLSFiles {
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

	return httpAgentMTLSFiles{
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

func TestNextHTTPAgentHealthIntervalBacksOffToFiveMinutes(t *testing.T) {
	if got := nextHTTPAgentHealthInterval(time.Second, time.Second, 300*time.Second); got != 2*time.Second {
		t.Fatalf("interval = %s", got)
	}
	if got := nextHTTPAgentHealthInterval(400*time.Second, time.Second, 300*time.Second); got != 300*time.Second {
		t.Fatalf("capped interval = %s", got)
	}
	if got := nextHTTPAgentHealthInterval(0, 2*time.Second, 300*time.Second); got != 4*time.Second {
		t.Fatalf("base interval = %s", got)
	}
	if got := nextHTTPAgentHealthInterval(10*time.Second, time.Second, 15*time.Second); got != 15*time.Second {
		t.Fatalf("custom capped interval = %s", got)
	}
}

func TestHubRestoresCompactGRPCResultEnvelope(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub.markInflight("edge-1", job.ID)

	hub.handleAgentResult(ctx, "edge-1", grpcwire.AgentResult{
		JobID: "job-1",
		Event: map[string]any{"type": "message", "message": "started"},
	})
	hub.handleAgentResult(ctx, "edge-1", grpcwire.AgentResult{
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

func TestHubRejectsSummaryWithoutExitCode(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	hub := NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job := model.Job{ID: "job-1", Tool: model.ToolPing, Target: "1.1.1.1", AgentID: "edge-1", Status: model.JobRunning, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	hub.markInflight("edge-1", job.ID)

	hub.handleAgentResult(ctx, "edge-1", grpcwire.AgentResult{
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
	st := store.NewMemory()
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
	hub.markInflight("edge-1", child.ID)

	hub.handleAgentResult(ctx, "edge-1", grpcwire.AgentResult{
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
