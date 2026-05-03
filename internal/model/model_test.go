package model

import (
	"encoding/json"
	"testing"
)

func TestStreamEventPayloadKeepsHopShape(t *testing.T) {
	rtt := 1.25
	ignored := 9.9
	payload := StreamEvent{
		Type: "hop",
		Hop: &HopResult{
			Index: 2,
			Probes: []ProbeResult{
				{IP: "1.1.1.1", RTTMS: &rtt},
				{IP: "1.0.0.1", RTTMS: &ignored},
			},
		},
	}.EventPayload()

	hop, ok := payload["hop"].(*HopResult)
	if !ok {
		t.Fatalf("hop payload = %#v", payload["hop"])
	}
	if len(hop.Probes) != 2 || hop.IP != "" || hop.RTTMS != nil {
		t.Fatalf("hop shape changed in payload: %#v", hop)
	}
}

func TestToolResultWirePayloadKeepsHopsShape(t *testing.T) {
	payload := ToolResult{
		Type:     "summary",
		ExitCode: 0,
		Hops: []HopResult{{
			Index:  1,
			Probes: []ProbeResult{{Timeout: true}},
		}},
	}.WirePayload()

	hops, ok := payload["hops"].([]HopResult)
	if !ok || len(hops) != 1 {
		t.Fatalf("hops payload = %#v", payload["hops"])
	}
	if len(hops[0].Probes) != 1 || !hops[0].Probes[0].Timeout || hops[0].Timeout {
		t.Fatalf("hop shape changed in payload: %#v", hops[0])
	}
}

func TestWirePayloadKeepsMetricErrorForInternalStorage(t *testing.T) {
	streamPayload := StreamEvent{
		Type:   "metric",
		Metric: map[string]any{"seq": 1, "error": "lookup failed"},
	}.WirePayload()
	streamMetric, ok := streamPayload["metric"].(map[string]any)
	if !ok || streamMetric["error"] != "lookup failed" {
		t.Fatalf("stream wire metric lost error: %#v", streamPayload)
	}

	resultPayload := ToolResult{
		Type:     "summary",
		ExitCode: -1,
		Summary:  map[string]any{"error": "lookup failed"},
	}.WirePayload()
	resultMetric, ok := resultPayload["metric"].(map[string]any)
	if !ok || resultMetric["error"] != "lookup failed" {
		t.Fatalf("summary wire metric lost error: %#v", resultPayload)
	}
}

func TestToolResultEventPayloadIsCompact(t *testing.T) {
	payload := ToolResult{
		Type:      "summary",
		Tool:      ToolDNS,
		Target:    "example.com",
		IPVersion: IPv4,
		ExitCode:  0,
		Summary: map[string]any{
			"record_count": 1,
			"error":        "lookup failed",
		},
	}.EventPayload()

	if _, ok := payload["tool"]; ok {
		t.Fatalf("tool exposed in summary payload: %#v", payload)
	}
	if _, ok := payload["target"]; ok {
		t.Fatalf("target exposed in summary payload: %#v", payload)
	}
	if _, ok := payload["ip_version"]; ok {
		t.Fatalf("ip_version exposed in summary payload: %#v", payload)
	}
	metric, ok := payload["metric"].(map[string]any)
	if !ok || metric["record_count"] != 1 {
		t.Fatalf("summary metric missing: %#v", payload)
	}
	if _, ok := payload["record_count"]; ok {
		t.Fatalf("summary metric should not be flattened: %#v", payload)
	}
	if _, ok := metric["error"]; ok {
		t.Fatalf("public summary metric should not expose error: %#v", metric)
	}
}

func TestStreamEventPayloadRemovesMetricError(t *testing.T) {
	payload := StreamEvent{
		Type:   "metric",
		Metric: map[string]any{"seq": 1, "error": "lookup failed"},
	}.EventPayload()

	metric, ok := payload["metric"].(map[string]any)
	if !ok || metric["seq"] != 1 {
		t.Fatalf("metric missing: %#v", payload)
	}
	if _, ok := metric["error"]; ok {
		t.Fatalf("public stream metric should not expose error: %#v", metric)
	}
}

func TestEventPayloadDropsErrorOnlyStatusMetric(t *testing.T) {
	payload := ToolResult{
		Type:     "summary",
		ExitCode: -1,
		Summary: map[string]any{
			"status": "error",
			"error":  "lookup failed",
		},
	}.EventPayload()

	if _, ok := payload["metric"]; ok {
		t.Fatalf("public error-only summary should keep only exit_code: %#v", payload)
	}
	if payload["exit_code"] != -1 {
		t.Fatalf("exit_code missing: %#v", payload)
	}
}

func TestProgressEventJSONOmitsAgentID(t *testing.T) {
	b, err := json.Marshal(JobEvent{
		ID:     "event-1",
		JobID:  "job-1",
		Stream: "progress",
		Event:  &StreamEvent{Type: "progress", Message: "started"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["agent_id"]; ok {
		t.Fatalf("progress event should omit agent_id: %s", b)
	}
}

func TestJobJSONExposesOnlyFailureType(t *testing.T) {
	b, err := json.Marshal(Job{
		ID:     "job-1",
		Tool:   ToolPing,
		Target: "example.com",
		Status: JobFailed,
		Error:  JobErrorUnsupportedProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["error"]; ok {
		t.Fatalf("job json should not expose internal error: %s", b)
	}
	if got["error_type"] != JobErrorUnsupportedProtocol {
		t.Fatalf("job json should expose sanitized error_type: %s", b)
	}
}

func TestPublicJobErrorTypeMapsRawErrorsToGenericType(t *testing.T) {
	if got := PublicJobErrorType("lookup failed"); got != JobErrorFailed {
		t.Fatalf("raw error type = %q, want %q", got, JobErrorFailed)
	}
	if got := PublicJobErrorType("agent disconnected"); got != JobErrorAgentDisconnected {
		t.Fatalf("agent disconnect type = %q, want %q", got, JobErrorAgentDisconnected)
	}
	if got := PublicJobErrorType("job timed out"); got != JobErrorTimeout {
		t.Fatalf("job timeout type = %q, want %q", got, JobErrorTimeout)
	}
}

func TestNormalizeAgentLabelsAddsTransportLabelsAndDropsReservedInput(t *testing.T) {
	got := NormalizeAgentLabels("edge-1", AgentTransportGRPC, []string{"blue", "agent:http", "id:other", "blue"})
	want := []string{"agent", "agent:grpc", "id:edge-1", "blue"}
	if len(got) != len(want) {
		t.Fatalf("grpc labels = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("grpc labels = %#v, want %#v", got, want)
		}
	}

	got = NormalizeAgentLabels("edge-2", AgentTransportHTTP, []string{"agent:grpc", "green"})
	want = []string{"agent", "agent:http", "id:edge-2", "green"}
	if len(got) != len(want) {
		t.Fatalf("http labels = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("http labels = %#v, want %#v", got, want)
		}
	}
}

func TestDNSRecordJSONIncludesOnlyTypeAndValue(t *testing.T) {
	b, err := json.Marshal(DNSRecord{Type: "A", Value: "93.184.216.34"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"type":"A","value":"93.184.216.34"}` {
		t.Fatalf("dns record json = %s", b)
	}
}
