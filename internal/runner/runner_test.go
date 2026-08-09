package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"

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

func TestRunBuiltinUDPTracerouteReportsProtocolAndPort(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		buf := make([]byte, 1500)
		n, peer, err := listener.ReadFrom(buf)
		if err == nil {
			_, _ = listener.WriteTo(buf[:n], peer)
		}
	}()

	port := listener.LocalAddr().(*net.UDPAddr).Port
	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "udp", "port": strconv.Itoa(port)},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Summary["protocol"] != "udp" || result.Summary["port"] != port {
		t.Fatalf("unexpected UDP traceroute result: %#v", result)
	}
	if len(result.Hops) != 1 || result.Hops[0].IP != "127.0.0.1" {
		t.Fatalf("unexpected UDP traceroute hops: %#v", result.Hops)
	}
}

func TestUDPTracerouteTreatsDestinationPortUnreachableAsReached(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.LocalAddr().(*net.UDPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	rtt, peer, reached, err := udpTraceProbeWithTimeout(context.Background(), net.ParseIP("127.0.0.1"), model.IPv4, port, 64, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if rtt <= 0 || !reached || peer == nil || !peer.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("unexpected UDP error queue result: rtt=%s peer=%v reached=%v", rtt, peer, reached)
	}
}

func TestRunBuiltinTracerouteAutoFallsBackToUDPAndLocksProtocol(t *testing.T) {
	originalListen := listenRouteICMP
	originalUDP := routeUDPProbe
	originalTCP := routeTCPProbe
	t.Cleanup(func() {
		listenRouteICMP = originalListen
		routeUDPProbe = originalUDP
		routeTCPProbe = originalTCP
	})
	listenRouteICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		return nil, errors.New("raw socket denied")
	}
	udpCalls := 0
	routeUDPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, port int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		udpCalls++
		if port != defaultUDPProbePort {
			t.Fatalf("UDP port = %d", port)
		}
		return time.Millisecond, target, true, nil
	}
	tcpCalls := 0
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		tcpCalls++
		return 0, target, true, nil
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "auto"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if udpCalls != 1 || tcpCalls != 0 || result.ExitCode != 0 {
		t.Fatalf("unexpected fallback calls: udp=%d tcp=%d result=%#v", udpCalls, tcpCalls, result)
	}
	if result.Summary["requested_protocol"] != "auto" || result.Summary["protocol"] != "udp" || result.Summary["port"] != defaultUDPProbePort {
		t.Fatalf("unexpected protocol summary: %#v", result.Summary)
	}
	if _, ok := result.Summary["protocol_fallbacks"]; ok {
		t.Fatalf("fallback internals must not be returned: %#v", result.Summary)
	}
}

func TestRunBuiltinMTRAutoSelectsProtocolOnlyOnce(t *testing.T) {
	originalListen := listenRouteICMP
	originalUDP := routeUDPProbe
	t.Cleanup(func() {
		listenRouteICMP = originalListen
		routeUDPProbe = originalUDP
	})
	listenCalls := 0
	listenRouteICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		listenCalls++
		return nil, errors.New("raw socket denied")
	}
	udpCalls := 0
	routeUDPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		udpCalls++
		return time.Millisecond, target, true, nil
	}

	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "2", "max_hops": "1", "protocol": "auto"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if listenCalls != 1 || udpCalls != 2 {
		t.Fatalf("selection was repeated: listen=%d udp=%d", listenCalls, udpCalls)
	}
	if result.Summary["requested_protocol"] != "auto" || result.Summary["protocol"] != "udp" {
		t.Fatalf("unexpected protocol summary: %#v", result.Summary)
	}
}

func TestRunBuiltinMTRKeepsConfiguredPortAcrossCycles(t *testing.T) {
	originalUDP := routeUDPProbe
	t.Cleanup(func() { routeUDPProbe = originalUDP })
	var ports []int
	routeUDPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, port int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		ports = append(ports, port)
		return time.Millisecond, target, true, nil
	}

	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "3", "max_hops": "1", "protocol": "udp", "port": "65535"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ports, []int{65535, 65535, 65535}) {
		t.Fatalf("ports changed across cycles: %#v", ports)
	}
	if result.Summary["port"] != 65535 || result.Summary["cycles"] != 3 {
		t.Fatalf("unexpected summary: %#v", result.Summary)
	}
}

func TestRunBuiltinMTRTreatsCompletedProbeTimeoutsAsSuccess(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	calls := 0
	routeTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		calls++
		return 0, nil, false, &net.DNSError{Err: "probe timeout", IsTimeout: true}
	}

	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "10", "max_hops": "1", "protocol": "tcp", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatalf("completed MTR returned an error: %v", err)
	}
	if calls != 10 || result.ExitCode != 0 || result.Summary["cycles"] != 10 {
		t.Fatalf("completed MTR was marked failed: calls=%d result=%#v", calls, result)
	}
	if len(result.Hops) != 1 || !result.Hops[0].Timeout || result.Hops[0].Sent == nil || *result.Hops[0].Sent != 10 || result.Hops[0].LossPct == nil || *result.Hops[0].LossPct != 100 {
		t.Fatalf("probe timeouts were not preserved as measurement data: %#v", result.Hops)
	}
	if _, ok := result.Summary["status"]; ok {
		t.Fatalf("completed MTR exposed an execution error: %#v", result.Summary)
	}
}

func TestRunBuiltinMTRPropagatesLaterCycleError(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	calls := 0
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		calls++
		if calls == 1 {
			return time.Millisecond, target, true, nil
		}
		return 0, nil, false, syscall.EPERM
	}

	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "3", "max_hops": "1", "protocol": "tcp", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("later cycle error was swallowed: %v", err)
	}
	if calls != 2 || result.Summary["cycles"] != 2 {
		t.Fatalf("actual cycles not reported: calls=%d summary=%#v", calls, result.Summary)
	}
	if len(result.Hops) != 1 || result.Hops[0].Sent == nil || *result.Hops[0].Sent != 2 || result.Hops[0].LossPct == nil || *result.Hops[0].LossPct != 50 {
		t.Fatalf("hop sent count should use recorded attempts: %#v", result.Hops)
	}
	if result.Summary["status"] != "error" || result.ExitCode != -1 {
		t.Fatalf("hard error was disguised: %#v", result)
	}
}

func TestRunBuiltinMTRPropagatesLaterCycleCancellation(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	calls := 0
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		calls++
		if calls == 1 {
			return time.Millisecond, target, true, nil
		}
		return 0, nil, false, context.Canceled
	}

	result, err := runBuiltinMTR(context.Background(), model.Job{
		Tool:      model.ToolMTR,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "3", "max_hops": "1", "protocol": "tcp", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if !errors.Is(err, context.Canceled) || result.Summary["cycles"] != 2 {
		t.Fatalf("later cancellation was swallowed: calls=%d err=%v summary=%#v", calls, err, result.Summary)
	}
	if result.ExitCode != -1 || result.Summary["status"] != "canceled" {
		t.Fatalf("cancellation was disguised: %#v", result)
	}
}

func TestRunBuiltinTracerouteReturnsHardProbeError(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	calls := 0
	routeTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		calls++
		return 0, nil, false, syscall.EPERM
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "5", "protocol": "tcp", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if !errors.Is(err, syscall.EPERM) || calls != 1 {
		t.Fatalf("hard error was retried or hidden: calls=%d err=%v", calls, err)
	}
	if result.ExitCode != -1 || result.Summary["status"] != "error" || len(result.Hops) != 0 {
		t.Fatalf("unexpected hard-error result: %#v", result)
	}
}

func TestRunBuiltinTracerouteAutoFallsBackAfterRemoteUDPUnreachable(t *testing.T) {
	originalListen := listenRouteICMP
	originalUDP := routeUDPProbe
	originalTCP := routeTCPProbe
	t.Cleanup(func() {
		listenRouteICMP = originalListen
		routeUDPProbe = originalUDP
		routeTCPProbe = originalTCP
	})
	listenRouteICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		return nil, syscall.EPERM
	}
	peer := net.ParseIP("192.0.2.254")
	routeUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		return time.Millisecond, peer, false, &udpTraceICMPError{
			Version: model.IPv4,
			Type:    icmpDestinationUnreachable,
			Code:    1,
			Peer:    peer,
			Errno:   syscall.EHOSTUNREACH,
		}
	}
	tcpCalls := 0
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		tcpCalls++
		return time.Millisecond, target, true, nil
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "auto", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil || tcpCalls != 1 || result.ExitCode != 0 || result.Summary["protocol"] != "tcp" {
		t.Fatalf("remote UDP unreachable should permit auto fallback: tcp=%d err=%v result=%#v", tcpCalls, err, result)
	}
}

func TestRunBuiltinTracerouteExplicitUDPReturnsStructuredUnreachable(t *testing.T) {
	originalUDP := routeUDPProbe
	t.Cleanup(func() { routeUDPProbe = originalUDP })
	peer := net.ParseIP("192.0.2.254")
	routeUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		return time.Millisecond, peer, false, &udpTraceICMPError{
			Version: model.IPv4,
			Type:    icmpDestinationUnreachable,
			Code:    1,
			Peer:    peer,
			Errno:   syscall.EHOSTUNREACH,
		}
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:           model.ToolTraceroute,
		Target:         "192.0.2.1",
		Args:           map[string]string{"max_hops": "3", "protocol": "udp"},
		ResolvedTarget: "192.0.2.1",
		IPVersion:      model.IPv4,
	}, nil)
	var icmpErr *udpTraceICMPError
	if !errors.As(err, &icmpErr) || result.ExitCode != -1 {
		t.Fatalf("explicit UDP unreachable should fail: err=%v result=%#v", err, result)
	}
	if result.Summary["icmp_type"] != int(icmpDestinationUnreachable) || result.Summary["icmp_code"] != 1 || result.Summary["peer"] != peer.String() || result.Summary["errno"] != syscall.EHOSTUNREACH.Error() {
		t.Fatalf("structured ICMP failure missing: %#v", result.Summary)
	}
}

func TestRunBuiltinTracerouteAutoStopsOnLocalUDPHardError(t *testing.T) {
	originalListen := listenRouteICMP
	originalUDP := routeUDPProbe
	originalTCP := routeTCPProbe
	t.Cleanup(func() {
		listenRouteICMP = originalListen
		routeUDPProbe = originalUDP
		routeTCPProbe = originalTCP
	})
	listenRouteICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		return nil, syscall.EPERM
	}
	routeUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		return 0, nil, false, syscall.EPERM
	}
	tcpCalls := 0
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		tcpCalls++
		return 0, target, true, nil
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "1", "protocol": "auto"},
		IPVersion: model.IPv4,
	}, nil)
	if !errors.Is(err, syscall.EPERM) || tcpCalls != 0 || result.ExitCode != -1 {
		t.Fatalf("local hard error should stop auto fallback: tcp=%d err=%v result=%#v", tcpCalls, err, result)
	}
}

func TestRunBuiltinTracerouteKeepsProbeTimeoutAsHopLoss(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	routeTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
		return 0, nil, false, &net.DNSError{Err: "probe timeout", IsTimeout: true}
	}

	result, err := runBuiltinTraceroute(context.Background(), model.Job{
		Tool:      model.ToolTraceroute,
		Target:    "127.0.0.1",
		Args:      map[string]string{"max_hops": "2", "protocol": "tcp", "port": "80"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatalf("per-probe timeout should remain hop loss: %v", err)
	}
	if result.ExitCode != 1 || len(result.Hops) != 2 || !result.Hops[0].Timeout || !result.Hops[1].Timeout {
		t.Fatalf("unexpected timeout result: %#v", result)
	}
}

func TestTraceOnceUsesTCPProbeReachability(t *testing.T) {
	originalTCP := routeTCPProbe
	t.Cleanup(func() { routeTCPProbe = originalTCP })
	routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
		return 2 * time.Millisecond, target, true, nil
	}

	rtt, peer, reached, err := traceOnce(context.Background(), nil, net.ParseIP("127.0.0.1"), model.IPv4, "tcp", 81, 1, 1, 1, time.Second)
	if err != nil || !reached || rtt != 2*time.Millisecond || !peer.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("TCP probe reachability was lost: rtt=%s peer=%v reached=%v err=%v", rtt, peer, reached, err)
	}
}

func TestClassifyUDPTraceErrorRejectsOtherUnreachableCodes(t *testing.T) {
	target := net.ParseIP("192.0.2.1")
	peer := net.ParseIP("192.0.2.254")
	gotPeer, reached, handled, err := classifyUDPTraceError(model.IPv4, target, unix.SO_EE_ORIGIN_ICMP, icmpDestinationUnreachable, 1, syscall.EHOSTUNREACH, peer)
	var icmpErr *udpTraceICMPError
	if !errors.As(err, &icmpErr) {
		t.Fatalf("expected typed ICMP error, got %T %v", err, err)
	}
	if !handled || reached || !gotPeer.Equal(peer) {
		t.Fatalf("unreachable was treated as destination: peer=%v reached=%v handled=%v", gotPeer, reached, handled)
	}
	if icmpErr.Type != icmpDestinationUnreachable || icmpErr.Code != 1 || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("typed error lost ICMP details: %#v", icmpErr)
	}
}

func TestClassifyUDPTracePortUnreachableFromIntermediateIsNotReached(t *testing.T) {
	target := net.ParseIP("192.0.2.1")
	peer := net.ParseIP("192.0.2.254")
	gotPeer, reached, handled, err := classifyUDPTraceError(model.IPv4, target, unix.SO_EE_ORIGIN_ICMP, icmpDestinationUnreachable, icmpPortUnreachable, syscall.ECONNREFUSED, peer)
	if err != nil || !handled || reached || !gotPeer.Equal(peer) {
		t.Fatalf("intermediate port unreachable was treated as destination: peer=%v reached=%v handled=%v err=%v", gotPeer, reached, handled, err)
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

func TestRunBuiltinTCPPingTreatsConnectionRefusedAsReply(t *testing.T) {
	originalTCP := pingTCPProbe
	t.Cleanup(func() { pingTCPProbe = originalTCP })
	pingTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		return 2500 * time.Microsecond, &net.OpError{Op: "dial", Net: "tcp4", Err: syscall.ECONNREFUSED}
	}

	var events []model.StreamEvent
	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "tcp", "port": "9"},
		IPVersion: model.IPv4,
	}, func(event model.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Summary["packets_received"] != 1 || result.Summary["packet_loss_pct"] != float64(0) {
		t.Fatalf("connection refused must count as a reply: %#v", result)
	}
	if result.Summary["rtt_avg_ms"] != 2.5 {
		t.Fatalf("connection refused RTT was lost: %#v", result.Summary)
	}
	if len(events) != 2 || events[1].Type != "metric" || events[1].Metric["latency_ms"] != 2.5 {
		t.Fatalf("expected a successful latency metric, events = %#v", events)
	}
	if _, ok := events[1].Metric["timeout"]; ok {
		t.Fatalf("connection refused was emitted as timeout: %#v", events[1])
	}
	if _, ok := events[1].Metric["error"]; ok {
		t.Fatalf("connection refused was emitted as error: %#v", events[1])
	}
}

func TestRunBuiltinUDPPingReportsProtocolAndPort(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		buf := make([]byte, 1500)
		n, peer, err := listener.ReadFrom(buf)
		if err == nil {
			_, _ = listener.WriteTo(buf[:n], peer)
		}
	}()

	port := listener.LocalAddr().(*net.UDPAddr).Port
	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "udp", "port": strconv.Itoa(port)},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Summary["protocol"] != "udp" || result.Summary["port"] != port {
		t.Fatalf("unexpected udp ping result: %#v", result)
	}
	if _, ok := result.Summary["requested_protocol"]; ok {
		t.Fatalf("explicit udp must not be reported as auto: %#v", result.Summary)
	}
}

func TestRunBuiltinPingExplicitUDPDoesNotFallback(t *testing.T) {
	originalUDP := pingUDPProbe
	originalTCP := pingTCPProbe
	t.Cleanup(func() {
		pingUDPProbe = originalUDP
		pingTCPProbe = originalTCP
	})
	udpCalls := 0
	tcpCalls := 0
	pingUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		udpCalls++
		return 0, errors.New("udp unavailable")
	}
	pingTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		tcpCalls++
		return time.Millisecond, nil
	}

	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "udp"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if udpCalls != 1 || tcpCalls != 0 || result.ExitCode != 1 || result.Summary["protocol"] != "udp" {
		t.Fatalf("explicit udp unexpectedly fell back: udp=%d tcp=%d result=%#v", udpCalls, tcpCalls, result)
	}
}

func TestRunBuiltinPingAutoFallsBackAndReportsActualProtocol(t *testing.T) {
	originalListen := listenPingICMP
	originalUDP := pingUDPProbe
	originalTCP := pingTCPProbe
	t.Cleanup(func() {
		listenPingICMP = originalListen
		pingUDPProbe = originalUDP
		pingTCPProbe = originalTCP
	})
	listenPingICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		return nil, errors.New("raw socket denied")
	}
	pingUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		return 0, errors.New("udp filtered")
	}
	tcpCalls := 0
	pingTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		tcpCalls++
		return 2 * time.Millisecond, nil
	}

	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "auto"},
		IPVersion: model.IPv4,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Summary["requested_protocol"] != "auto" || result.Summary["protocol"] != "tcp" || result.Summary["port"] != 80 || tcpCalls != 1 {
		t.Fatalf("unexpected auto fallback result: tcp=%d result=%#v", tcpCalls, result)
	}
	if _, ok := result.Summary["protocol_fallbacks"]; ok {
		t.Fatalf("auto fallback internals must not be returned: %#v", result.Summary)
	}
}

func TestRunBuiltinPingExplicitICMPDoesNotFallbackWhenSocketDenied(t *testing.T) {
	originalListen := listenPingICMP
	originalUDP := pingUDPProbe
	t.Cleanup(func() {
		listenPingICMP = originalListen
		pingUDPProbe = originalUDP
	})
	listenPingICMP = func(model.IPVersion) (*icmp.PacketConn, error) {
		return nil, errors.New("raw socket denied")
	}
	udpCalls := 0
	pingUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
		udpCalls++
		return time.Millisecond, nil
	}

	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool:      model.ToolPing,
		Target:    "127.0.0.1",
		Args:      map[string]string{"count": "1", "protocol": "icmp"},
		IPVersion: model.IPv4,
	}, nil)
	if err == nil || udpCalls != 0 || result.Summary["protocol"] != "icmp" {
		t.Fatalf("explicit icmp unexpectedly fell back: udp=%d err=%v result=%#v", udpCalls, err, result)
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

func TestHTTPProbeDisablesEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	result, err := runBuiltinHTTP(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: srv.URL,
		Args:   map[string]string{"method": "HEAD"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary["http_code"] != http.StatusNoContent {
		t.Fatalf("http_code = %#v, summary = %#v", result.Summary["http_code"], result.Summary)
	}
	if result.Summary["http_version"] != "1.1" {
		t.Fatalf("http_version = %#v, summary = %#v", result.Summary["http_version"], result.Summary)
	}
}

func TestHTTPProbeTransportUsesStrictRequestedVersion(t *testing.T) {
	h1, closeH1 := newHTTPProbeTransport(model.Job{}, "1.1", "example.com")
	defer closeH1()
	h1Transport, ok := h1.(*http.Transport)
	if !ok || !h1Transport.Protocols.HTTP1() || h1Transport.Protocols.HTTP2() {
		t.Fatalf("unexpected HTTP/1.1 transport: %#v", h1)
	}
	if h1Transport.MaxResponseHeaderBytes != maxHTTPResponseHeaderBytes {
		t.Fatalf("HTTP/1.1 response header limit = %d", h1Transport.MaxResponseHeaderBytes)
	}

	h2, closeH2 := newHTTPProbeTransport(model.Job{}, "2", "example.com")
	defer closeH2()
	h2Transport, ok := h2.(*http.Transport)
	if !ok || h2Transport.Protocols.HTTP1() || !h2Transport.Protocols.HTTP2() {
		t.Fatalf("unexpected HTTP/2 transport: %#v", h2)
	}

	h3, closeH3 := newHTTPProbeTransport(model.Job{}, "3", "example.com")
	defer closeH3()
	if _, ok := h3.(*http3.Transport); !ok {
		t.Fatalf("unexpected HTTP/3 transport: %#v", h3)
	}
	if h3.(*http3.Transport).MaxResponseHeaderBytes != maxHTTPResponseHeaderBytes {
		t.Fatalf("HTTP/3 response header limit = %d", h3.(*http3.Transport).MaxResponseHeaderBytes)
	}
}

func TestRunBuiltinHTTPAutoFallsBackInOrder(t *testing.T) {
	originalAttempt := executeHTTPProbeAttempt
	t.Cleanup(func() { executeHTTPProbeAttempt = originalAttempt })
	var attempts []string
	executeHTTPProbeAttempt = func(_ context.Context, job model.Job, _ string, version string, _ time.Duration) (*model.ToolResult, bool, error) {
		attempts = append(attempts, version)
		result := newBuiltinResult(job, 1)
		if version != "1.1" {
			return result, false, errors.New("protocol unavailable")
		}
		result.ExitCode = 0
		result.Summary["http_protocol"] = "1.1"
		return result, true, nil
	}

	result, err := runBuiltinHTTP(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "auto"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(attempts, []string{"3", "2", "1.1"}) {
		t.Fatalf("attempts = %#v", attempts)
	}
	if result.Summary["requested_http_version"] != "auto" || result.Summary["http_version"] != "1.1" {
		t.Fatalf("unexpected version summary: %#v", result.Summary)
	}
	if _, ok := result.Summary["protocol_fallbacks"]; ok {
		t.Fatalf("fallback internals must not be returned: %#v", result.Summary)
	}
}

func TestRunBuiltinHTTPAutoTotalIncludesFailedAttempts(t *testing.T) {
	originalAttempt := executeHTTPProbeAttempt
	t.Cleanup(func() { executeHTTPProbeAttempt = originalAttempt })
	executeHTTPProbeAttempt = func(_ context.Context, job model.Job, _ string, version string, _ time.Duration) (*model.ToolResult, bool, error) {
		time.Sleep(12 * time.Millisecond)
		result := newBuiltinResult(job, 1)
		if version != "1.1" {
			return result, false, errors.New("protocol unavailable")
		}
		result.ExitCode = 0
		result.Summary["http_protocol"] = "1.1"
		return result, true, nil
	}

	result, err := runBuiltinHTTPWithTimeout(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "auto"},
	}, nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	total, ok := result.Summary["time_total_ms"].(float64)
	if !ok || total < 30 {
		t.Fatalf("total time omitted fallback attempts: %#v", result.Summary["time_total_ms"])
	}
}

func TestRunHTTPProbeAttemptFirstByteBudgetDoesNotLimitBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a"))
		w.(http.Flusher).Flush()
		time.Sleep(45 * time.Millisecond)
		_, _ = w.Write([]byte("bcdef"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, responseStarted, err := runHTTPProbeAttempt(ctx, model.Job{
		Tool:   model.ToolHTTP,
		Target: srv.URL,
	}, http.MethodGet, "1.1", 10*time.Millisecond)
	if err != nil {
		t.Fatalf("body inherited first-byte deadline: %v, result=%#v", err, result)
	}
	if !responseStarted || result.Summary["bytes_downloaded"] != int64(6) {
		t.Fatalf("unexpected response: started=%v summary=%#v", responseStarted, result.Summary)
	}
	total, ok := result.Summary["time_total_ms"].(float64)
	if !ok || total < 35 {
		t.Fatalf("body did not use the overall context: %#v", result.Summary)
	}
}

func TestRunHTTPProbeAttemptTimesOutBeforeFirstByte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			w.WriteHeader(http.StatusNoContent)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	result, responseStarted, err := runHTTPProbeAttempt(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: srv.URL,
	}, http.MethodHead, "1.1", 10*time.Millisecond)
	if err == nil || responseStarted {
		t.Fatalf("expected pre-response timeout: started=%v err=%v", responseStarted, err)
	}
	if result.Summary["status"] != "timeout" {
		t.Fatalf("timeout status missing: %#v", result.Summary)
	}
}

func TestCopyHTTPResponseBodyEnforcesLimit(t *testing.T) {
	read, truncated, err := copyHTTPResponseBody(strings.NewReader("12345"), 4)
	if err != nil || !truncated || read != 4 {
		t.Fatalf("body limit not enforced: read=%d truncated=%v err=%v", read, truncated, err)
	}
	result := newBuiltinResult(model.Job{Tool: model.ToolHTTP}, 0)
	fillHTTPSummary(result, httpProbeSummary{bodyTruncated: true})
	if result.Summary["body_truncated"] != true || result.Summary["body_limit_bytes"] != int64(maxHTTPResponseBodyBytes) {
		t.Fatalf("truncation metrics missing: %#v", result.Summary)
	}
}

func TestRunBuiltinHTTPExplicitVersionDoesNotFallback(t *testing.T) {
	originalAttempt := executeHTTPProbeAttempt
	t.Cleanup(func() { executeHTTPProbeAttempt = originalAttempt })
	var attempts []string
	executeHTTPProbeAttempt = func(_ context.Context, job model.Job, _ string, version string, _ time.Duration) (*model.ToolResult, bool, error) {
		attempts = append(attempts, version)
		return newBuiltinResult(job, 1), false, errors.New("protocol unavailable")
	}

	result, err := runBuiltinHTTP(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "2"},
	}, nil)
	if err == nil || !reflect.DeepEqual(attempts, []string{"2"}) {
		t.Fatalf("explicit HTTP/2 unexpectedly fell back: attempts=%#v err=%v", attempts, err)
	}
	if result.Summary["http_version"] != "2" {
		t.Fatalf("unexpected version summary: %#v", result.Summary)
	}
}

func TestRunBuiltinHTTPAutoDoesNotRetryAfterResponseStarts(t *testing.T) {
	originalAttempt := executeHTTPProbeAttempt
	t.Cleanup(func() { executeHTTPProbeAttempt = originalAttempt })
	var attempts []string
	executeHTTPProbeAttempt = func(_ context.Context, job model.Job, _ string, version string, _ time.Duration) (*model.ToolResult, bool, error) {
		attempts = append(attempts, version)
		return newBuiltinResult(job, 1), true, errors.New("response body failed")
	}

	_, err := runBuiltinHTTP(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "GET", "http_version": "auto"},
	}, nil)
	if err == nil || !reflect.DeepEqual(attempts, []string{"3"}) {
		t.Fatalf("request retried after response start: attempts=%#v err=%v", attempts, err)
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

func TestRunBuiltinHTTPDoesNotFollowRedirects(t *testing.T) {
	redirectHit := false
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer redirectSrv.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, redirectSrv.URL, http.StatusFound)
	}))
	defer srv.Close()

	result, err := runBuiltinHTTP(context.Background(), model.Job{
		Tool:   model.ToolHTTP,
		Target: srv.URL,
		Args:   map[string]string{"method": "HEAD"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if redirectHit {
		t.Fatal("redirect target should not be requested")
	}
	if result.Summary["http_code"] != http.StatusFound {
		t.Fatalf("http_code = %#v, summary = %#v", result.Summary["http_code"], result.Summary)
	}
	if result.ExitCode != 0 {
		t.Fatalf("redirect status should be returned directly, exit_code = %d", result.ExitCode)
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

func TestBuiltinProtocolSelectorsRequireCanonicalValues(t *testing.T) {
	for _, value := range []string{"auto", "icmp", "udp", "tcp"} {
		if got, err := probeMode(value); err != nil || got != value {
			t.Fatalf("probeMode(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "ICMP", " udp", "h2"} {
		if got, err := probeMode(value); err == nil {
			t.Fatalf("probeMode(%q) = %q, want error", value, got)
		}
	}

	for _, value := range []string{"auto", "3", "2", "1.1"} {
		if got, err := httpVersionMode(value); err != nil || got != value {
			t.Fatalf("httpVersionMode(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "h3", "http/2", "2.0", "HTTP/1.1"} {
		if got, err := httpVersionMode(value); err == nil {
			t.Fatalf("httpVersionMode(%q) = %q, want error", value, got)
		}
	}

	for _, value := range []string{"GET", "HEAD"} {
		if got, err := httpMethod(value); err != nil || got != value {
			t.Fatalf("httpMethod(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "get", "POST"} {
		if got, err := httpMethod(value); err == nil {
			t.Fatalf("httpMethod(%q) = %q, want error", value, got)
		}
	}

	if got := parsePort(nil, 443); got != 443 {
		t.Fatalf("missing port = %d, want default 443", got)
	}
	for _, value := range []string{"+80", "080", " 80", "80 ", "65536"} {
		if got := parsePort(map[string]string{"port": value}, 443); got != 0 {
			t.Fatalf("parsePort(%q) = %d, want strict rejection", value, got)
		}
	}
}

func TestRunBuiltinPingRejectsNonCanonicalPortBeforeResolution(t *testing.T) {
	result, err := runBuiltinPing(context.Background(), model.Job{
		Tool: model.ToolPing, Target: "does-not-resolve.invalid", Args: map[string]string{"protocol": "tcp", "port": "080"},
	}, nil)
	if err == nil || result == nil || result.ExitCode != -1 {
		t.Fatalf("result = %#v, err = %v, want strict port error", result, err)
	}
}

func TestRunBuiltinDNSRejectsNonCanonicalRecordType(t *testing.T) {
	result, err := runBuiltinDNS(context.Background(), model.Job{
		Tool: model.ToolDNS, Target: "example.com", Args: map[string]string{"type": "aaaa"},
	}, nil)
	if err == nil || result == nil || result.ExitCode != 1 {
		t.Fatalf("result = %#v, err = %v, want strict type error", result, err)
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
