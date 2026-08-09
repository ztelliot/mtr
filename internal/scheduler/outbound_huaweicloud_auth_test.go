package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
)

func TestEventEndpointHuaweiCloudAKSKSignsFinalRequest(t *testing.T) {
	const (
		accessKey = "AKIDEXAMPLE123456789"
		projectID = "0123456789abcdef0123456789abcdef"
		secretKey = "secret-example-key"
	)
	var mutex sync.Mutex
	var authorizations []string
	var receivedBodies [][]byte
	client := &http.Client{Transport: eventRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.GetBody != nil {
			t.Error("signed event request became replayable")
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		authorization := request.Header.Get("Authorization")
		if !strings.HasPrefix(authorization, "SDK-HMAC-SHA256 Access="+accessKey+", SignedHeaders=") ||
			!strings.Contains(authorization, "host") ||
			!strings.Contains(authorization, "x-cff-request-version") ||
			!strings.Contains(authorization, "x-project-id") ||
			!strings.Contains(authorization, "x-sdk-date") ||
			strings.Contains(authorization, secretKey) {
			t.Errorf("unexpected Authorization header %q", authorization)
		}
		if request.Header.Get("X-Project-Id") != projectID {
			t.Errorf("X-Project-Id = %q", request.Header.Get("X-Project-Id"))
		}
		if _, err := time.Parse("20060102T150405Z", request.Header.Get("X-Sdk-Date")); err != nil {
			t.Errorf("X-Sdk-Date = %q: %v", request.Header.Get("X-Sdk-Date"), err)
		}
		mutex.Lock()
		authorizations = append(authorizations, authorization)
		receivedBodies = append(receivedBodies, body)
		mutex.Unlock()
		var event struct {
			Action string `json:"action"`
			ID     string `json:"id"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		var responseBody []byte
		switch event.Action {
		case "healthz":
			responseBody, err = json.Marshal(testOutboundHealth("event-edge"))
		case "invoke":
			responseBody, err = json.Marshal(map[string]any{
				"job_id": event.ID,
				"events": []any{},
				"result": map[string]any{"type": "summary", "exit_code": 0},
			})
		default:
			t.Fatalf("event action = %q", event.Action)
		}
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
		}, nil
	})}
	agent := OutboundAgent{
		ID:        "event-edge",
		Transport: config.OutboundTransportEvent,
		URL: "https://functiongraph.cn-east-3.myhuaweicloud.com/v2/" + projectID +
			"/fgs/functions/urn:fss:cn-east-3:" + projectID +
			":function:default:mtr-agent:latest/invocations",
		Headers: map[string]string{"X-CFF-Request-Version": "v1"},
		Auth: config.OutboundAgentAuth{
			Type:      config.OutboundAuthHuaweiCloudAKSK,
			AccessKey: accessKey,
			ProjectID: projectID,
			Secret:    secretKey,
		},
		EventResponse: config.EventResponseDirect,
		HTTPClient:    client,
	}
	if _, err := probeOutboundAgentHealth(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	var results []grpcwire.ResultEvent
	if err := callOutboundAgent(context.Background(), agent, &grpcwire.JobSpec{
		ProtocolVersion: grpcwire.AgentProtocolVersion,
		ID:              "job-1",
		Tool:            model.ToolPing,
		Target:          "example.com",
	}, func(event grpcwire.ResultEvent) { results = append(results, event) }); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || eventType(results[0].Event) != "summary" {
		t.Fatalf("results = %#v", results)
	}
	if len(authorizations) != 2 || len(receivedBodies) != 2 {
		t.Fatalf("signed requests = %d, bodies = %d", len(authorizations), len(receivedBodies))
	}
	if !bytes.Contains(receivedBodies[0], []byte(`"action":"healthz"`)) || !bytes.Contains(receivedBodies[1], []byte(`"action":"invoke"`)) {
		t.Fatalf("event bodies = %q, %q", receivedBodies[0], receivedBodies[1])
	}
	if authorizations[0] == authorizations[1] {
		t.Fatal("different final request bodies produced the same signature")
	}
}

func TestOutboundAuthorizerRejectsMixedHuaweiCloudCredentials(t *testing.T) {
	_, err := newOutboundRequestAuthorizer(config.OutboundAgentAuth{
		Type:      config.OutboundAuthHuaweiCloudAKSK,
		Header:    "X-Custom-Auth",
		AccessKey: "access-key",
		ProjectID: "project-id",
		Secret:    "secret-key",
	})
	if err == nil {
		t.Fatal("Huawei Cloud auth with a custom auth header was accepted")
	}

	for name, auth := range map[string]config.OutboundAgentAuth{
		"none":   {Type: config.OutboundAuthNone, AccessKey: "unexpected"},
		"bearer": {Type: config.OutboundAuthBearer, AccessKey: "unexpected", Secret: "secret"},
		"header": {Type: config.OutboundAuthHeader, Header: "X-Agent-Token", ProjectID: "unexpected", Secret: "secret"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newOutboundRequestAuthorizer(auth); err == nil {
				t.Fatal("unrelated Huawei Cloud credential fields were accepted")
			}
		})
	}
}

func TestHTTPPreConnectRetryRegeneratesHuaweiCloudSignature(t *testing.T) {
	oldDelay := httpTransportInvokeRetryDelay
	httpTransportInvokeRetryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { httpTransportInvokeRetryDelay = oldDelay }()

	const projectID = "0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		signedAt, err := time.Parse("20060102T150405Z", request.Header.Get("X-Sdk-Date"))
		if err != nil {
			t.Fatalf("X-Sdk-Date = %q: %v", request.Header.Get("X-Sdk-Date"), err)
		}
		// The first connection attempt is held for 2.5 seconds. A signature
		// reused from that attempt is therefore observably stale here, while a
		// newly generated signature is at most one-second old due to its format.
		if age := time.Since(signedAt); age < 0 || age >= 2*time.Second {
			t.Fatalf("retried request signature age = %v", age)
		}
		if request.Header.Get("X-Project-Id") != projectID ||
			!strings.HasPrefix(request.Header.Get("Authorization"), "SDK-HMAC-SHA256 Access=access-key, ") {
			t.Fatalf("signed headers = %#v", request.Header)
		}
		response.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(response).Encode(map[string]any{"type": "summary", "exit_code": 0})
	}))
	defer server.Close()

	var dialAttempts atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dialAttempts.Add(1) == 1 {
			timer := time.NewTimer(2500 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
				return nil, errors.New("dial failed before request write")
			}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client := &http.Client{Transport: transport}
	var results []grpcwire.ResultEvent
	err := callOutboundAgentWithAttempts(context.Background(), OutboundAgent{
		ID:        "http-edge",
		Transport: config.OutboundTransportHTTP,
		URL:       server.URL,
		Auth: config.OutboundAgentAuth{
			Type:      config.OutboundAuthHuaweiCloudAKSK,
			AccessKey: "access-key",
			ProjectID: projectID,
			Secret:    "secret-key",
		},
		HTTPClient: client,
	}, &grpcwire.JobSpec{ProtocolVersion: grpcwire.AgentProtocolVersion, ID: "job-1"}, 2, func(event grpcwire.ResultEvent) {
		results = append(results, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if dialAttempts.Load() != 2 || len(results) != 1 || eventType(results[0].Event) != "summary" {
		t.Fatalf("dial attempts = %d, results = %#v", dialAttempts.Load(), results)
	}
}
