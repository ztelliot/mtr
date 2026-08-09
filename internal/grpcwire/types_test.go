package grpcwire

import (
	"encoding/json"
	"testing"

	"github.com/ztelliot/mtr/internal/model"
)

func TestAgentHelloProtocolVersionRoundTrip(t *testing.T) {
	oldPayload, err := json.Marshal(AgentHello{ID: "invalid-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldPayload) == 0 {
		t.Fatal("empty hello payload")
	}

	var invalidHello AgentHello
	if err := json.Unmarshal([]byte(`{"id":"invalid-agent"}`), &invalidHello); err != nil {
		t.Fatal(err)
	}
	if invalidHello.ProtocolVersion != 0 {
		t.Fatalf("missing protocol version decoded unexpectedly: %#v", invalidHello)
	}

	payload, err := json.Marshal(AgentHello{ProtocolVersion: AgentProtocolVersion, ID: "new-agent", MaxConcurrentJobs: 4})
	if err != nil {
		t.Fatal(err)
	}
	var got AgentHello
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != AgentProtocolVersion || got.MaxConcurrentJobs != 4 {
		t.Fatalf("hello = %#v", got)
	}
}

func TestJSONCodecRejectsUnknownFieldsAndTrailingDocuments(t *testing.T) {
	codec := JSONCodec{}
	for name, payload := range map[string][]byte{
		"unknown field":      []byte(`{"type":"cancel","cancel":{"job_id":"job-1"},"legacy":true}`),
		"trailing document":  []byte(`{"type":"cancel","cancel":{"job_id":"job-1"}} {}`),
		"leading whitespace": []byte(` {"type":"cancel","cancel":{"job_id":"job-1"}}`),
		"duplicate field":    []byte(`{"type":"cancel","type":"cancel","cancel":{"job_id":"job-1"}}`),
		"duplicate nested":   []byte(`{"type":"cancel","cancel":{"job_id":"job-1","job_id":"job-2"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var message ServerMessage
			if err := codec.Unmarshal(payload, &message); err == nil {
				t.Fatalf("codec accepted %s: %#v", payload, message)
			}
		})
	}

	var message ServerMessage
	if err := codec.Unmarshal([]byte(`{"type":"cancel","cancel":{"job_id":"job-1"}}`), &message); err != nil {
		t.Fatalf("codec rejected canonical message: %v", err)
	}
}

func TestJSONCodecRoundTripsStructuredEventPayload(t *testing.T) {
	codec := JSONCodec{}
	message := AgentMessage{
		Type: "result",
		Result: &AgentResult{
			JobID: "job-1",
			Event: model.StreamEvent{
				Type: "hop",
				Hop:  &model.HopResult{Index: 1, Host: "router.example", IP: "192.0.2.1"},
			}.WirePayload(),
		},
	}
	payload, err := codec.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AgentMessage
	if err := codec.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("codec rejected its own structured event: %v\npayload: %s", err, payload)
	}
	if decoded.Result == nil || decoded.Result.JobID != "job-1" {
		t.Fatalf("decoded message = %#v", decoded)
	}
}
