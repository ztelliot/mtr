package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/grpcwire"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

func TestEventAgentHealthUsesExactEndpointHeadersAndAuth(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/functions/agent" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("X-CFF-Request-Version") != "v1" || request.Header.Get("X-Auth-Token") != "secret" {
			t.Fatalf("headers = %#v", request.Header)
		}
		var event struct {
			Action          string `json:"action"`
			ProtocolVersion int    `json:"protocol_version"`
		}
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Action != "healthz" || event.ProtocolVersion != grpcwire.AgentProtocolVersion {
			t.Fatalf("event = %#v", event)
		}
		_ = json.NewEncoder(w).Encode(testOutboundHealth("event-edge"))
	}))
	defer server.Close()

	agent := testEventOutboundAgent(server, config.EventResponseDirect)
	health, err := probeOutboundAgentHealth(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	if health.AgentID != "event-edge" || health.ProtocolVersion != grpcwire.AgentProtocolVersion || calls.Load() != 1 {
		t.Fatalf("health = %#v calls=%d", health, calls.Load())
	}
}

func TestEventAgentDirectResultIsValidatedBeforeDelivery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var event map[string]any
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["action"] != "invoke" || event["id"] != "job-1" || event["protocol_version"] != float64(grpcwire.AgentProtocolVersion) {
			t.Fatalf("event = %#v", event)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_id": "job-1",
			"events": []any{map[string]any{"type": "message", "message": "started"}},
			"result": map[string]any{"type": "summary", "exit_code": 0, "metric": map[string]any{"received": 1}},
		})
	}))
	defer server.Close()

	var events []grpcwire.ResultEvent
	err := callOutboundAgent(context.Background(), testEventOutboundAgent(server, config.EventResponseDirect), &grpcwire.JobSpec{
		ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1", Tool: model.ToolPing,
	}, func(event grpcwire.ResultEvent) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].JobID != "job-1" || events[0].AgentID != "event-edge" || eventType(events[0].Event) != "message" || eventType(events[1].Event) != "summary" {
		t.Fatalf("events = %#v", events)
	}
}

func TestEventAgentResultFieldAcceptsObjectAndJSONString(t *testing.T) {
	inner, err := json.Marshal(map[string]any{
		"job_id": "job-1", "events": []any{},
		"result": map[string]any{"type": "summary", "exit_code": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		result any
	}{
		{name: "object", result: json.RawMessage(inner)},
		{name: "json string", result: string(inner)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "error": nil, "result": test.result})
			}))
			defer server.Close()
			var events []grpcwire.ResultEvent
			err := callOutboundAgent(context.Background(), testEventOutboundAgent(server, config.EventResponseResultField), &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}, func(event grpcwire.ResultEvent) {
				events = append(events, event)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || eventType(events[0].Event) != "summary" {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestEventAgentResultFieldAllowsBoundedJSONStringEnvelope(t *testing.T) {
	inner, err := json.Marshal(map[string]any{
		"job_id": "job-1",
		"events": []any{map[string]any{"type": "message", "message": strings.Repeat("\\", 1<<20)}},
		"result": map[string]any{"type": "summary", "exit_code": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) >= outboundAgentResultBytesLimit {
		t.Fatalf("decoded fixture = %d bytes, must remain below decoded limit", len(inner))
	}
	wrapper, err := json.Marshal(map[string]any{"status": 200, "error": nil, "result": string(inner)})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapper) <= outboundAgentResultBytesLimit || len(wrapper) > outboundEventResultHTTPBodyLimit {
		t.Fatalf("wrapped fixture = %d bytes, decoded limit = %d, HTTP limit = %d", len(wrapper), outboundAgentResultBytesLimit, outboundEventResultHTTPBodyLimit)
	}
	agent := testEventOutboundAgentWithBody(wrapper)
	agent.EventResponse = config.EventResponseResultField
	var delivered int
	if err := callEventAgent(context.Background(), agent, &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}, func(grpcwire.ResultEvent) {
		delivered++
	}); err != nil {
		t.Fatal(err)
	}
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2", delivered)
	}
}

func TestEventAgentHealthRejectsOversizedDecodedResultField(t *testing.T) {
	inner, err := json.Marshal(map[string]any{
		"status": strings.Repeat("x", outboundAgentHealthBodyLimit),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) <= outboundAgentHealthBodyLimit {
		t.Fatalf("decoded fixture = %d bytes, want more than %d", len(inner), outboundAgentHealthBodyLimit)
	}
	wrapper, err := json.Marshal(map[string]any{
		"status": 200,
		"error":  nil,
		"result": string(inner),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapper) > outboundEventHealthHTTPBodyLimit {
		t.Fatalf("wrapped fixture = %d bytes, HTTP limit = %d", len(wrapper), outboundEventHealthHTTPBodyLimit)
	}
	agent := testEventOutboundAgentWithBody(wrapper)
	agent.EventResponse = config.EventResponseResultField
	if _, err := probeEventAgentHealth(context.Background(), agent); err == nil || !strings.Contains(err.Error(), "exceeds byte limit") {
		t.Fatalf("error = %v, want decoded health byte limit", err)
	}
}

func TestEventAgentRejectsMalformedDocumentBeforeDeliveringEvents(t *testing.T) {
	for name, result := range map[string]string{
		"wrong job":         `{"job_id":"other","events":[{"type":"message"}],"result":{"type":"summary","exit_code":0}}`,
		"summary in events": `{"job_id":"job-1","events":[{"type":"summary","exit_code":0}],"result":{"type":"summary","exit_code":0}}`,
		"missing exit code": `{"job_id":"job-1","events":[{"type":"message"}],"result":{"type":"summary"}}`,
		"padded event type": `{"job_id":"job-1","events":[{"type":" message "}],"result":{"type":"summary","exit_code":0}}`,
		"unknown field":     `{"job_id":"job-1","events":[],"result":{"type":"summary","exit_code":0},"extra":true}`,
		"trailing json":     `{"job_id":"job-1","events":[],"result":{"type":"summary","exit_code":0}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := decodeEventAgentResult([]byte(result), "job-1", "event-edge")
			if err == nil {
				t.Fatal("expected malformed result to fail")
			}
		})
	}
}

func TestEventAgentResultFieldRejectsOuterFailureAndTrailingJSON(t *testing.T) {
	for name, body := range map[string]string{
		"status":   `{"status":500,"result":{}}`,
		"error":    `{"status":200,"error":"failed","result":{}}`,
		"trailing": `{"status":200,"result":{}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := unwrapEventAgentResponse([]byte(body), config.EventResponseResultField); err == nil {
				t.Fatal("expected wrapper to fail")
			}
		})
	}
}

func TestEventAgentDoesNotRetryConnectionErrors(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: eventRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("dial failed")
	})}
	agent := OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example/invoke",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect, HTTPClient: client,
	}
	err := callOutboundAgentWithAttempts(context.Background(), agent, &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}, 5, func(grpcwire.ResultEvent) {})
	if err == nil || !isOutboundAgentConnectionError(err) {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestEventAgentDisablesTransportLevelPOSTReplay(t *testing.T) {
	client := &http.Client{Transport: eventRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.GetBody != nil {
			t.Fatal("event request exposed GetBody and may be replayed")
		}
		if request.Header.Get("Idempotency-Key") != "static-value" {
			t.Fatalf("idempotency header = %q", request.Header.Get("Idempotency-Key"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader([]byte(`{}`))),
		}, nil
	})}
	agent := OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example/invoke",
		Auth:          config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "Idempotency-Key", Secret: "static-value"},
		EventResponse: config.EventResponseDirect, HTTPClient: client,
	}
	if _, err := callEventEndpoint(context.Background(), agent, []byte(`{}`), 1024); err != nil {
		t.Fatal(err)
	}
}

func TestEventAgentStopsLargeReplayAfterContextDeadline(t *testing.T) {
	events := make([]any, outboundAgentResultEventLimit)
	for index := range events {
		events[index] = map[string]any{"type": "message", "message": "progress"}
	}
	body, err := json.Marshal(map[string]any{
		"job_id": "job-1",
		"events": events,
		"result": map[string]any{"type": "summary", "exit_code": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := testEventOutboundAgentWithBody(body)
	ctx := newTestDeadlineContext()
	var delivered atomic.Int32
	err = callEventAgent(ctx, agent, &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}, func(grpcwire.ResultEvent) {
		if delivered.Add(1) == 1 {
			ctx.expire()
		}
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if got := delivered.Load(); got != 1 {
		t.Fatalf("delivered %d events after deadline, want 1", got)
	}
}

func TestInvokeEventAgentSlowStoreDeadlineFailsRunningJob(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"job_id": "slow-store-job",
		"events": []any{map[string]any{"type": "message", "message": "progress"}},
		"result": map[string]any{"type": "summary", "exit_code": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	base := store.NewTestMemory()
	ctx := context.Background()
	now := time.Now().UTC()
	job := model.Job{ID: "slow-store-job", AgentID: "event-edge", Tool: model.ToolPing, Target: "1.1.1.1", Status: model.JobRunning, CreatedAt: now, UpdatedAt: now}
	if err := base.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	blocking := &blockFirstJobReadStore{Store: base}
	hub := NewHub(blocking, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	unlockAgent := hub.lockAgentLifecycles(job.AgentID)
	hub.mu.Lock()
	_, generation := hub.registerOutboundAgentLoopLocked(ctx, job.AgentID)
	hub.mu.Unlock()
	unlockAgent()
	attempt, ok := hub.markInflightForOutboundAgent(job.AgentID, job.ID, generation)
	if !ok {
		t.Fatal("mark outbound job inflight")
	}

	oldGrace := outboundAgentInvokeGrace
	outboundAgentInvokeGrace = func(string) time.Duration { return 20 * time.Millisecond }
	defer func() { outboundAgentInvokeGrace = oldGrace }()
	started := time.Now()
	hub.invokeOutboundAgent(ctx, testEventOutboundAgentWithBody(body), &grpcwire.JobSpec{
		ProtocolVersion: grpcwire.AgentProtocolVersion,
		ID:              job.ID,
		Tool:            job.Tool,
	}, make(chan outboundInvokeOutcome, 1), 1, generation, attempt)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("invoke remained blocked after deadline for %s", elapsed)
	}
	got, err := base.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobFailed || got.Error != model.JobErrorTimeout {
		t.Fatalf("slow-store job was left active: %#v", got)
	}
	if inflight := hub.inflightCount(job.AgentID); inflight != 0 {
		t.Fatalf("inflight jobs = %d, want 0", inflight)
	}
	if blocking.blocked.Load() != 1 {
		t.Fatalf("blocked reads = %d, want 1", blocking.blocked.Load())
	}
}

func TestDecodeOutboundHealthRejectsTrailingJSON(t *testing.T) {
	body, err := json.Marshal(testOutboundHealth("event-edge"))
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(` {}`)...)
	if _, err := decodeOutboundAgentHealth(body); err == nil {
		t.Fatal("expected trailing health JSON to fail")
	}
}

func testEventOutboundAgent(server *httptest.Server, responseMode string) OutboundAgent {
	return OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, URL: server.URL + "/functions/agent",
		Headers:       map[string]string{"X-CFF-Request-Version": "v1"},
		Auth:          config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "X-Auth-Token", Secret: "secret"},
		EventResponse: responseMode, HTTPClient: server.Client(),
	}
}

func testOutboundHealth(id string) map[string]any {
	return map[string]any{
		"status": "ok", "version": map[string]any{"version": "test"},
		"agent": map[string]any{
			"protocol_version": grpcwire.AgentProtocolVersion, "id": id,
			"capabilities": []string{"ping"}, "protocols": 3, "max_concurrent_jobs": 4,
		},
	}
}

type eventRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn eventRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func testEventOutboundAgentWithBody(body []byte) OutboundAgent {
	return OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example/invoke",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect,
		HTTPClient: &http.Client{Transport: eventRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(body)),
			}, nil
		})},
	}
}

type blockFirstJobReadStore struct {
	store.Store
	once    sync.Once
	blocked atomic.Int32
}

type testDeadlineContext struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newTestDeadlineContext() *testDeadlineContext {
	return &testDeadlineContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *testDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *testDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *testDeadlineContext) expire() {
	c.once.Do(func() { close(c.done) })
}

func (s *blockFirstJobReadStore) GetJob(ctx context.Context, id string) (model.Job, error) {
	block := false
	s.once.Do(func() { block = true })
	if block {
		s.blocked.Add(1)
		<-ctx.Done()
		return model.Job{}, ctx.Err()
	}
	return s.Store.GetJob(ctx, id)
}
