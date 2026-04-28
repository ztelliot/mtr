package runner

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/ztelliot/mtr/internal/model"
)

func TestEmitHopFlattensSingleProbeAndSummaryOmitsProbeDetails(t *testing.T) {
	sent := 3
	rtt := 0.238
	stdev := 1.5
	hop := model.HopResult{
		Index:   1,
		IP:      "fc80::1",
		Probes:  []model.ProbeResult{{IP: "fc80::1", RTTMS: &rtt}},
		TimesMS: []float64{rtt},
		Sent:    &sent,
		StdevMS: &stdev,
	}
	var events []model.StreamEvent
	emitHop(func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	}, hop)
	emitHopSummary(func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	}, hop)

	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Type != "hop" || len(events[0].Hop.Probes) != 0 || events[0].Hop.RTTMS == nil || *events[0].Hop.RTTMS != rtt {
		t.Fatalf("progress hop should flatten single probe: %#v", events[0])
	}
	if events[1].Type != "hop_summary" {
		t.Fatalf("summary event type = %q", events[1].Type)
	}
	if len(events[1].Hop.Probes) != 0 || len(events[1].Hop.TimesMS) != 0 {
		t.Fatalf("summary should omit probe details: %#v", events[1].Hop)
	}
	if events[1].Hop.Sent == nil {
		t.Fatalf("summary fields removed: %#v", events[1].Hop)
	}
	if events[1].Hop.StdevMS == nil || *events[1].Hop.StdevMS != stdev {
		t.Fatalf("summary stdev removed: %#v", events[1].Hop)
	}
}

func TestSummarizeHopIncludesStandardDeviation(t *testing.T) {
	one, two, three := 1.0, 2.0, 3.0
	hop := summarizeHop(1, []model.ProbeResult{
		{RTTMS: &one},
		{RTTMS: &two},
		{RTTMS: &three},
	}, 3)
	if hop.StdevMS == nil || math.Abs(*hop.StdevMS-math.Sqrt(2.0/3.0)) > 1e-12 {
		t.Fatalf("stdev_ms = %#v, hop = %#v", hop.StdevMS, hop)
	}
}

func TestTraceProbeSeqIsUniquePerHop(t *testing.T) {
	if traceProbeSeq(1, 0, 1) != 1 {
		t.Fatalf("first probe seq = %d, want 1", traceProbeSeq(1, 0, 1))
	}
	if traceProbeSeq(2, 0, 1) == traceProbeSeq(1, 0, 1) {
		t.Fatalf("different TTLs reused seq %d", traceProbeSeq(1, 0, 1))
	}
	if traceProbeSeq(3, 1, 3) != 8 {
		t.Fatalf("multi-probe seq = %d, want 8", traceProbeSeq(3, 1, 3))
	}
}

func TestTimeExceededMatchesOriginalProbe(t *testing.T) {
	const id = 0x1234
	const seq = 7

	ipv4Body := &icmp.TimeExceeded{Data: originalIPv4EchoDatagram(id, seq)}
	if !timeExceededMatches(ipv4Body, model.IPv4, id, seq) {
		t.Fatal("IPv4 time exceeded did not match original echo")
	}
	if timeExceededMatches(ipv4Body, model.IPv4, id, seq+1) {
		t.Fatal("IPv4 time exceeded matched the wrong sequence")
	}

	ipv6Body := &icmp.TimeExceeded{Data: originalIPv6EchoDatagram(id, seq)}
	if !timeExceededMatches(ipv6Body, model.IPv6, id, seq) {
		t.Fatal("IPv6 time exceeded did not match original echo")
	}
	if timeExceededMatches(ipv6Body, model.IPv6, id+1, seq) {
		t.Fatal("IPv6 time exceeded matched the wrong id")
	}
	if timeExceededMatches(&icmp.TimeExceeded{Data: []byte{0x45}}, model.IPv4, id, seq) {
		t.Fatal("truncated time exceeded data should not match")
	}
}

func originalIPv4EchoDatagram(id int, seq int) []byte {
	data := make([]byte, 28)
	data[0] = 0x45
	data[9] = icmpProtocolIPv4
	data[20] = byte(ipv4.ICMPTypeEcho)
	binary.BigEndian.PutUint16(data[24:26], uint16(id))
	binary.BigEndian.PutUint16(data[26:28], uint16(seq))
	return data
}

func originalIPv6EchoDatagram(id int, seq int) []byte {
	data := make([]byte, 48)
	data[0] = 0x60
	data[6] = icmpProtocolIPv6
	data[40] = byte(ipv6.ICMPTypeEchoRequest)
	binary.BigEndian.PutUint16(data[44:46], uint16(id))
	binary.BigEndian.PutUint16(data[46:48], uint16(seq))
	return data
}

func TestRunBuiltinPortTCP(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runBuiltinPort(context.Background(), model.Job{
		Tool:      model.ToolPort,
		Target:    "127.0.0.1",
		Args:      map[string]string{"port": port},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Summary["status"] != "open" || result.Summary["protocol"] != "tcp" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestPreferredIPUsesIPv6ForUnspecifiedVersion(t *testing.T) {
	ips := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}
	if got := preferredIP(ips, model.IPAny); got.String() != "2606:4700:4700::1111" {
		t.Fatalf("preferred ip = %s", got)
	}
	if got := preferredIP(ips, model.IPv4); got.String() != "1.1.1.1" {
		t.Fatalf("explicit IPv4 should keep first resolved address, got %s", got)
	}
}

func TestRunBuiltinMTREmitsResolvedTargetBeforeFirstHop(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var events []model.StreamEvent
	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "max_hops": "1", "protocol": "tcp", "port": port},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(events) < 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != "target_resolved" || events[0].Metric["target_ip"] != "127.0.0.1" || events[0].Metric["ip_version"] != model.IPv4 {
		t.Fatalf("first event should be resolved target: %#v", events[0])
	}
	if firstEventIndex(events, "target_resolved") >= firstEventIndex(events, "hop") {
		t.Fatalf("resolved target should precede first hop: %#v", events)
	}
}

func TestRunBuiltinTracerouteEmitsResolvedTargetBeforeFirstHop(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var events []model.StreamEvent
	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "tcp", "port": port},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(events) < 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Type != "target_resolved" || events[0].Metric["target_ip"] != "127.0.0.1" || events[0].Metric["ip_version"] != model.IPv4 {
		t.Fatalf("first event should be resolved target: %#v", events[0])
	}
	if firstEventIndex(events, "target_resolved") >= firstEventIndex(events, "hop") {
		t.Fatalf("resolved target should precede first hop: %#v", events)
	}
}

func TestRunBuiltinPingSummaryIncludesTargetIP(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	var events []model.StreamEvent
	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "tcp", "port": port},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary["target_ip"] != "127.0.0.1" {
		t.Fatalf("summary target_ip = %#v, summary = %#v", result.Summary["target_ip"], result.Summary)
	}
	if len(events) == 0 || events[0].Type != "target_resolved" || events[0].Metric["target_ip"] != "127.0.0.1" {
		t.Fatalf("expected target_resolved event, events = %#v", events)
	}
}

func TestRunBuiltinPingEmitsMetricWhenTimedOut(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinPing(ctx, model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "tcp"},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if result == nil || result.ExitCode != -1 || result.Summary["status"] != "timeout" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) < 2 || events[0].Type != "target_resolved" || events[0].Metric["target_ip"] != "127.0.0.1" {
		t.Fatalf("expected target_resolved before timeout metric, events = %#v", events)
	}
	if !hasTimeoutMetric(events) {
		t.Fatalf("expected timeout metric, events = %#v", events)
	}
	want := map[string]any{"seq": 1, "timeout": true}
	if !reflect.DeepEqual(events[len(events)-1].Metric, want) {
		t.Fatalf("timeout metric = %#v, want %#v", events[len(events)-1].Metric, want)
	}
}

func TestRunBuiltinTracerouteDoesNotEmitMetricWhenTimedOut(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinTraceroute(ctx, model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "tcp"},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if result == nil || result.ExitCode != -1 || result.Summary["status"] != "timeout" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if hasEventType(events, "metric") {
		t.Fatalf("traceroute should not emit metric events: %#v", events)
	}
}

func TestPingTimeoutProbeMetricIsMinimal(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	emitPingProbeMetric(func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	}, ctx, 7, 0, context.DeadlineExceeded)

	if len(events) != 1 || events[0].Type != "metric" {
		t.Fatalf("events = %#v", events)
	}
	want := map[string]any{"seq": 7, "timeout": true}
	if !reflect.DeepEqual(events[0].Metric, want) {
		t.Fatalf("metric = %#v, want %#v", events[0].Metric, want)
	}
}

func TestPingSuccessfulProbeMetricIsMinimal(t *testing.T) {
	var events []model.StreamEvent
	emitPingProbeMetric(func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	}, context.Background(), 7, 1500*time.Microsecond, nil)

	if len(events) != 1 || events[0].Type != "metric" {
		t.Fatalf("events = %#v", events)
	}
	want := map[string]any{"seq": 7, "latency_ms": 1.5}
	if !reflect.DeepEqual(events[0].Metric, want) {
		t.Fatalf("metric = %#v, want %#v", events[0].Metric, want)
	}
}

func TestRunBuiltinMTRDoesNotEmitMetricWhenTimedOut(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinMTR(ctx, model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "max_hops": "1", "protocol": "tcp"},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if result == nil || result.ExitCode != -1 || result.Summary["status"] != "timeout" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if hasEventType(events, "metric") {
		t.Fatalf("mtr should not emit metric events: %#v", events)
	}
}

func TestHopFlatteningForcesFirstProbe(t *testing.T) {
	hop := model.HopResult{
		Index: 1,
		Probes: []model.ProbeResult{
			{IP: "1.1.1.1"},
			{IP: "1.0.0.1"},
		},
	}
	flattened := hop.FlattenSingleProbe()
	if len(flattened.Probes) != 0 || flattened.IP != "1.1.1.1" {
		t.Fatalf("expected first probe only: %#v", flattened)
	}
}

func TestHTTPProbeKeepsOriginalHostAndSNI(t *testing.T) {
	req, sni, err := newHTTPProbeRequest(context.Background(), "GET", "https://example.com:8443/path")
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != "example.com:8443" {
		t.Fatalf("host = %q", req.Host)
	}
	if sni != "example.com" {
		t.Fatalf("sni = %q", sni)
	}
}

func TestHTTPProbeDialUsesResolvedTargetOnlyForConnection(t *testing.T) {
	address, err := httpDialAddress(model.Job{ResolvedTarget: "1.1.1.1"}, "example.com:8443")
	if err != nil {
		t.Fatal(err)
	}
	if address != "1.1.1.1:8443" {
		t.Fatalf("address = %q", address)
	}
	req, sni, err := newHTTPProbeRequest(context.Background(), "GET", "https://example.com:8443/path")
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != "example.com:8443" || sni != "example.com" {
		t.Fatalf("resolved dial leaked into request identity: host=%q sni=%q", req.Host, sni)
	}
}

func TestHTTPProbeOmitsSNIForIPLiteral(t *testing.T) {
	req, sni, err := newHTTPProbeRequest(context.Background(), "GET", "https://1.1.1.1/status")
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != "1.1.1.1" {
		t.Fatalf("host = %q", req.Host)
	}
	if sni != "" {
		t.Fatalf("sni = %q", sni)
	}
}

func TestRunBuiltinHTTPReturnsPartialSummaryOnTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinHTTP(ctx, model.Job{
		Tool:   model.ToolHTTP,
		Target: srv.URL,
		Args:   map[string]string{"method": "GET"},
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if result == nil {
		t.Fatal("expected partial result")
	}
	if result.ExitCode != 1 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if result.Summary["status"] != "timeout" {
		t.Fatalf("status = %#v, summary = %#v", result.Summary["status"], result.Summary)
	}
	if result.Summary["http_code"] != http.StatusOK {
		t.Fatalf("http_code = %#v, summary = %#v", result.Summary["http_code"], result.Summary)
	}
	if got, ok := result.Summary["bytes_downloaded"].(int64); !ok || got == 0 {
		t.Fatalf("bytes_downloaded = %#v, summary = %#v", result.Summary["bytes_downloaded"], result.Summary)
	}
	if _, ok := result.Summary["time_total_ms"].(float64); !ok {
		t.Fatalf("missing time_total_ms: %#v", result.Summary)
	}
	if len(events) != 0 {
		t.Fatalf("expected no stream events for single-result http probe, events = %#v", events)
	}
}

func TestRunBuiltinDNSReturnsTimeoutWithoutMetric(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinDNS(ctx, model.Job{
		Tool:   model.ToolDNS,
		Target: "example.com",
		Args:   map[string]string{"type": "A"},
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
	if result == nil || result.ExitCode != 1 || result.Summary["status"] != "timeout" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) != 0 {
		t.Fatalf("expected no stream events for single-result dns probe, events = %#v", events)
	}
}

func TestRunBuiltinPortReturnsTimeoutWithoutMetric(t *testing.T) {
	ctx, cancel := expiredContext()
	defer cancel()
	var events []model.StreamEvent
	result, err := runBuiltinPort(ctx, model.Job{
		Tool:      model.ToolPort,
		Target:    "127.0.0.1",
		Args:      map[string]string{"port": "80"},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.ExitCode != 1 || result.Summary["status"] != "timeout" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(events) != 0 {
		t.Fatalf("expected no stream events for single-result port probe, events = %#v", events)
	}
}

func expiredContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	time.Sleep(time.Millisecond)
	return ctx, cancel
}

func hasTimeoutMetric(events []model.StreamEvent) bool {
	for _, event := range events {
		if event.Type == "metric" && event.Metric["timeout"] == true {
			return true
		}
	}
	return false
}

func hasEventType(events []model.StreamEvent, typ string) bool {
	for _, event := range events {
		if event.Type == typ {
			return true
		}
	}
	return false
}

func firstEventIndex(events []model.StreamEvent, typ string) int {
	for i, event := range events {
		if event.Type == typ {
			return i
		}
	}
	return len(events)
}
