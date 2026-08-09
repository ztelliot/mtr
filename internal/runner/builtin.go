package runner

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/ztelliot/mtr/internal/model"
)

const (
	icmpProtocolIPv4           = 1
	icmpProtocolIPv6           = 58
	defaultPingProbeTimeout    = time.Second
	defaultPingInterval        = time.Second
	defaultRouteProbeTimeout   = time.Second
	defaultHTTPAttemptTimeout  = time.Second
	defaultUDPProbePort        = 33434
	maxHTTPResponseBodyBytes   = 8 << 20
	maxHTTPResponseHeaderBytes = 1 << 20
	tcpProbeTimeout            = 2 * time.Second
)

var (
	listenPingICMP          = listenICMP
	pingUDPProbe            = udpProbeWithTimeout
	pingTCPProbe            = tcpProbeWithTimeout
	listenRouteICMP         = listenICMP
	routeUDPProbe           = udpTraceProbeWithTimeout
	routeTCPProbe           = tcpTraceProbeWithTimeout
	executeHTTPProbeAttempt = runHTTPProbeAttempt
)

func runBuiltin(ctx context.Context, job model.Job, probeTimeout time.Duration, sink StreamSink) (*model.ToolResult, error) {
	switch job.Tool {
	case model.ToolPing:
		return runBuiltinPingWithTimeout(ctx, job, sink, probeTimeout)
	case model.ToolTraceroute:
		return runBuiltinTracerouteWithTimeout(ctx, job, sink, probeTimeout)
	case model.ToolMTR:
		return runBuiltinMTRWithTimeout(ctx, job, sink, probeTimeout)
	case model.ToolHTTP:
		return runBuiltinHTTPWithTimeout(ctx, job, sink, probeTimeout)
	case model.ToolDNS:
		return runBuiltinDNS(ctx, job, sink)
	case model.ToolPort:
		return runBuiltinPort(ctx, job, sink)
	default:
		return nil, fmt.Errorf("unsupported builtin tool %s", job.Tool)
	}
}

func runBuiltinPing(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	return runBuiltinPingWithTimeout(ctx, job, sink, defaultPingProbeTimeout)
}

func runBuiltinPingWithTimeout(ctx context.Context, job model.Job, sink StreamSink, probeTimeout time.Duration) (*model.ToolResult, error) {
	probeTimeout = normalizedPingProbeTimeout(probeTimeout)
	count := parsePositiveInt(argOr(job.Args, "count", "10"))
	requestedMode, err := probeMode(argOr(job.Args, "protocol", "icmp"))
	if err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		emitPingTimeoutMetric(sink, ctx, err, 1)
		return result, err
	}
	if err := validateOptionalProbePort(job.Args); err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		emitPingTimeoutMetric(sink, ctx, err, 1)
		return result, err
	}
	mode := requestedMode
	targetIP, version, err := resolveOne(ctx, executionTarget(job), job.IPVersion)
	if err != nil {
		result := newBuiltinResult(job, -1)
		setPingProtocolSummary(result, requestedMode, mode, job.Args)
		fillErrorSummary(ctx, result, err)
		emitPingTimeoutMetric(sink, ctx, err, 1)
		return result, err
	}
	emitResolvedTarget(sink, targetIP, version)
	var conn *icmp.PacketConn
	if mode == "auto" || mode == "icmp" {
		mode = "icmp"
		conn, err = listenPingICMP(version)
		if err != nil {
			if requestedMode == "auto" {
				mode = "udp"
			} else {
				result := newBuiltinResult(job, -1)
				result.IPVersion = version
				setPingProtocolSummary(result, requestedMode, mode, job.Args)
				fillErrorSummary(ctx, result, err)
				emitPingTimeoutMetric(sink, ctx, err, 1)
				return result, err
			}
		}
	}
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	result := newBuiltinResult(job, 0)
	result.IPVersion = version
	result.Summary["target_ip"] = targetIP.String()
	setPingProtocolSummary(result, requestedMode, mode, job.Args)
	id := rand.New(rand.NewSource(time.Now().UnixNano())).Intn(0xffff)
	attempted := 0
	var samples []float64
	received := 0
	autoSelecting := requestedMode == "auto"
	for seq := 0; seq < count; seq++ {
		select {
		case <-ctx.Done():
			result.ExitCode = -1
			setPingProtocolSummary(result, requestedMode, mode, job.Args)
			fillPingSummary(result, attempted, received, samples)
			fillErrorSummary(ctx, result, ctx.Err())
			emitPingTimeoutMetric(sink, ctx, ctx.Err(), attempted+1)
			return result, ctx.Err()
		default:
		}
		attempted = seq + 1
		var rtt time.Duration
		for {
			port := parsePort(job.Args, defaultProbePort(mode))
			rtt, err = probeOnce(ctx, conn, targetIP, version, mode, port, 64, id, seq+1, probeTimeout)
			if !autoSelecting || err == nil || mode == "tcp" {
				autoSelecting = false
				break
			}
			switch mode {
			case "icmp":
				if conn != nil {
					_ = conn.Close()
					conn = nil
				}
				mode = "udp"
			case "udp":
				mode = "tcp"
			}
			setPingProtocolSummary(result, requestedMode, mode, job.Args)
		}
		setPingProtocolSummary(result, requestedMode, mode, job.Args)
		if err == nil {
			received++
			ms := float64(rtt.Microseconds()) / 1000
			samples = append(samples, ms)
		}
		emitPingProbeMetric(sink, ctx, seq+1, rtt, err)
		if seq+1 < count {
			timer := time.NewTimer(defaultPingInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				result.ExitCode = -1
				setPingProtocolSummary(result, requestedMode, mode, job.Args)
				fillPingSummary(result, attempted, received, samples)
				fillErrorSummary(ctx, result, ctx.Err())
				return result, ctx.Err()
			case <-timer.C:
			}
		}
	}
	setPingProtocolSummary(result, requestedMode, mode, job.Args)
	fillPingSummary(result, count, received, samples)
	if received == 0 {
		result.ExitCode = 1
		result.Summary["status"] = "timeout"
	}
	return result, nil
}

func setPingProtocolSummary(result *model.ToolResult, requested string, actual string, args map[string]string) {
	result.Summary["protocol"] = actual
	if requested == "auto" {
		result.Summary["requested_protocol"] = "auto"
	}
	port := parsePort(args, defaultProbePort(actual))
	if port > 0 {
		result.Summary["port"] = port
	} else {
		delete(result.Summary, "port")
	}
}

func probeOnce(ctx context.Context, conn *icmp.PacketConn, target net.IP, version model.IPVersion, mode string, port int, ttl int, id int, seq int, timeout time.Duration) (time.Duration, error) {
	switch mode {
	case "tcp":
		rtt, err := pingTCPProbe(ctx, target, version, port, ttl, normalizedPingProbeTimeout(timeout))
		if errors.Is(err, syscall.ECONNREFUSED) {
			// A TCP RST from the destination proves that it is reachable even
			// though the probed port is closed.
			return rtt, nil
		}
		return rtt, err
	case "udp":
		return pingUDPProbe(ctx, target, version, port, ttl, normalizedPingProbeTimeout(timeout))
	default:
		return icmpProbe(ctx, conn, target, version, ttl, id, seq, timeout)
	}
}

func udpProbeWithTimeout(ctx context.Context, target net.IP, version model.IPVersion, port int, ttl int, timeout time.Duration) (time.Duration, error) {
	network := "udp4"
	if version == model.IPv6 {
		network = "udp6"
	}
	dialer := net.Dialer{
		Timeout: timeout,
		Control: func(_ string, _ string, c syscall.RawConn) error {
			var controlErr error
			err := c.Control(func(fd uintptr) {
				if version == model.IPv6 {
					controlErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
					return
				}
				controlErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
			})
			if err != nil {
				return err
			}
			return controlErr
		},
	}
	conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(target.String(), strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	payload := []byte("mtr-udp-ping")
	start := time.Now()
	if _, err := conn.Write(payload); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return time.Since(start), nil
		}
		return 0, err
	}
	buf := make([]byte, 1500)
	if _, err := conn.Read(buf); err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return time.Since(start), nil
		}
		return 0, err
	}
	return time.Since(start), nil
}

func icmpProbe(ctx context.Context, conn *icmp.PacketConn, target net.IP, version model.IPVersion, ttl int, id int, seq int, timeout time.Duration) (time.Duration, error) {
	if err := setTTL(conn, version, ttl); err != nil {
		return 0, err
	}
	var msgType icmp.Type = ipv4.ICMPTypeEcho
	var replyType icmp.Type = ipv4.ICMPTypeEchoReply
	protocol := icmpProtocolIPv4
	if version == model.IPv6 {
		msgType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply
		protocol = icmpProtocolIPv6
	}
	body := []byte("mtr-ping")
	msg := icmp.Message{Type: msgType, Code: 0, Body: &icmp.Echo{ID: id, Seq: seq, Data: body}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	deadline := start.Add(normalizedPingProbeTimeout(timeout))
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	if _, err := conn.WriteTo(b, ipAddr(target)); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		rm, err := icmp.ParseMessage(protocol, buf[:n])
		if err != nil {
			continue
		}
		if rm.Type == replyType {
			if body, ok := rm.Body.(*icmp.Echo); ok && body.ID == id && body.Seq == seq {
				return time.Since(start), nil
			}
		}
	}
}

func normalizedPingProbeTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultPingProbeTimeout
	}
	return timeout
}

func runBuiltinTraceroute(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	return runBuiltinTracerouteWithTimeout(ctx, job, sink, defaultRouteProbeTimeout)
}

func runBuiltinTracerouteWithTimeout(ctx context.Context, job model.Job, sink StreamSink, probeTimeout time.Duration) (*model.ToolResult, error) {
	maxHops := parsePositiveInt(argOr(job.Args, "max_hops", "30"))
	mode, err := probeMode(argOr(job.Args, "protocol", "icmp"))
	if err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	if err := validateOptionalProbePort(job.Args); err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	result, err := runTrace(ctx, job, sink, maxHops, 1, mode, true, probeTimeout)
	flattenResultHops(result)
	if err != nil {
		return result, err
	}
	return result, nil
}

func runTrace(ctx context.Context, job model.Job, sink StreamSink, maxHops int, probesPerHop int, requestedMode string, emitResolved bool, probeTimeout time.Duration) (*model.ToolResult, error) {
	probeTimeout = normalizedRouteProbeTimeout(probeTimeout)
	targetIP, version, err := resolveOne(ctx, executionTarget(job), job.IPVersion)
	if err != nil {
		result := newBuiltinResult(job, -1)
		setRouteProtocolSummary(result, requestedMode, requestedMode, job.Args)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	if emitResolved {
		emitResolvedTarget(sink, targetIP, version)
	}
	mode := requestedMode
	if mode == "auto" {
		mode = "icmp"
	}
	var conn *icmp.PacketConn
	if mode == "icmp" {
		conn, err = listenRouteICMP(version)
		if err != nil {
			if requestedMode == "auto" {
				mode = "udp"
			} else {
				result := newBuiltinResult(job, -1)
				result.IPVersion = version
				setRouteProtocolSummary(result, requestedMode, mode, job.Args)
				fillErrorSummary(ctx, result, err)
				return result, err
			}
		}
	}
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()

	result := newBuiltinResult(job, 1)
	result.IPVersion = version
	setRouteProtocolSummary(result, requestedMode, mode, job.Args)
	id := rand.New(rand.NewSource(time.Now().UnixNano())).Intn(0xffff)
	autoSelecting := requestedMode == "auto"
	for ttl := 1; ttl <= maxHops; ttl++ {
		select {
		case <-ctx.Done():
			result.ExitCode = -1
			setRouteProtocolSummary(result, requestedMode, mode, job.Args)
			fillErrorSummary(ctx, result, ctx.Err())
			return result, ctx.Err()
		default:
		}
		hop := model.HopResult{Index: ttl}
		for probe := 0; probe < probesPerHop; probe++ {
			var rtt time.Duration
			var peer net.IP
			var reached bool
			for {
				port := routeProbePort(job.Args, mode)
				rtt, peer, reached, err = traceOnce(ctx, conn, targetIP, version, mode, port, id, ttl, traceProbeSeq(ttl, probe, probesPerHop), probeTimeout)
				if err == nil {
					autoSelecting = false
					break
				}
				if contextErr := routeContextError(ctx, err); contextErr != nil {
					result.ExitCode = -1
					setRouteProtocolSummary(result, requestedMode, mode, job.Args)
					fillErrorSummary(ctx, result, contextErr)
					return result, contextErr
				}
				if !autoSelecting || mode == "tcp" || !routeErrorAllowsFallback(err) {
					autoSelecting = false
					break
				}
				switch mode {
				case "icmp":
					if conn != nil {
						_ = conn.Close()
						conn = nil
					}
					mode = "udp"
				case "udp":
					mode = "tcp"
				}
				setRouteProtocolSummary(result, requestedMode, mode, job.Args)
			}
			if err != nil {
				if routeProbeTimedOut(err) {
					hop.Probes = append(hop.Probes, model.ProbeResult{Timeout: true})
					continue
				}
				result.ExitCode = -1
				setRouteProtocolSummary(result, requestedMode, mode, job.Args)
				fillRouteProbeErrorSummary(ctx, result, err)
				return result, err
			}
			ms := float64(rtt.Microseconds()) / 1000
			probeResult := model.ProbeResult{RTTMS: &ms}
			if peer != nil {
				probeResult.IP = peer.String()
			}
			hop.Probes = append(hop.Probes, probeResult)
			if reached {
				result.ExitCode = 0
			}
		}
		result.Hops = append(result.Hops, hop)
		emitHop(sink, hop)
		if result.ExitCode == 0 {
			break
		}
	}
	result.Summary["hop_count"] = len(result.Hops)
	setRouteProtocolSummary(result, requestedMode, mode, job.Args)
	return result, nil
}

func traceOnce(ctx context.Context, conn *icmp.PacketConn, target net.IP, version model.IPVersion, mode string, port int, id int, ttl int, seq int, probeTimeout time.Duration) (time.Duration, net.IP, bool, error) {
	switch mode {
	case "tcp":
		return routeTCPProbe(ctx, target, version, port, ttl, normalizedRouteProbeTimeout(probeTimeout))
	case "udp":
		return routeUDPProbe(ctx, target, version, port, ttl, normalizedRouteProbeTimeout(probeTimeout))
	}
	if err := setTTL(conn, version, ttl); err != nil {
		return 0, nil, false, err
	}
	var msgType icmp.Type = ipv4.ICMPTypeEcho
	protocol := icmpProtocolIPv4
	echoReply := ipv4.ICMPTypeEchoReply
	timeExceeded := ipv4.ICMPTypeTimeExceeded
	if version == model.IPv6 {
		msgType = ipv6.ICMPTypeEchoRequest
		protocol = icmpProtocolIPv6
	}
	msg := icmp.Message{Type: msgType, Code: 0, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("mtr-trace")}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, nil, false, err
	}
	start := time.Now()
	deadline := start.Add(normalizedRouteProbeTimeout(probeTimeout))
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, nil, false, err
	}
	if _, err := conn.WriteTo(b, ipAddr(target)); err != nil {
		return 0, nil, false, err
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, nil, false, err
		}
		rm, err := icmp.ParseMessage(protocol, buf[:n])
		if err != nil {
			continue
		}
		switch rm.Type {
		case echoReply:
			if echo, ok := rm.Body.(*icmp.Echo); ok && echo.ID == id && echo.Seq == seq {
				return time.Since(start), addrIP(peer), true, nil
			}
		case timeExceeded:
			if timeExceededMatches(rm.Body, version, id, seq) {
				return time.Since(start), addrIP(peer), false, nil
			}
		case ipv6.ICMPTypeEchoReply:
			if version == model.IPv6 {
				if echo, ok := rm.Body.(*icmp.Echo); ok && echo.ID == id && echo.Seq == seq {
					return time.Since(start), addrIP(peer), true, nil
				}
			}
		case ipv6.ICMPTypeTimeExceeded:
			if version == model.IPv6 && timeExceededMatches(rm.Body, version, id, seq) {
				return time.Since(start), addrIP(peer), false, nil
			}
		}
	}
}

func traceProbeSeq(ttl int, probe int, probesPerHop int) int {
	if probesPerHop <= 0 {
		probesPerHop = 1
	}
	return (ttl-1)*probesPerHop + probe + 1
}

func timeExceededMatches(body icmp.MessageBody, version model.IPVersion, id int, seq int) bool {
	timeExceeded, ok := body.(*icmp.TimeExceeded)
	if !ok {
		return false
	}
	inner := originalEchoMessage(timeExceeded.Data, version)
	if len(inner) < 8 {
		return false
	}
	switch version {
	case model.IPv4:
		if inner[0] != byte(ipv4.ICMPTypeEcho) {
			return false
		}
	case model.IPv6:
		if inner[0] != byte(ipv6.ICMPTypeEchoRequest) {
			return false
		}
	default:
		return false
	}
	return int(binary.BigEndian.Uint16(inner[4:6])) == id && int(binary.BigEndian.Uint16(inner[6:8])) == seq
}

func originalEchoMessage(data []byte, version model.IPVersion) []byte {
	switch version {
	case model.IPv4:
		if len(data) < 28 || data[0]>>4 != 4 {
			return nil
		}
		headerLen := int(data[0]&0x0f) * 4
		if headerLen < 20 || len(data) < headerLen+8 || data[9] != icmpProtocolIPv4 {
			return nil
		}
		return data[headerLen:]
	case model.IPv6:
		if len(data) < 48 || data[0]>>4 != 6 || data[6] != icmpProtocolIPv6 {
			return nil
		}
		return data[40:]
	default:
		return nil
	}
}

func runBuiltinMTR(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	return runBuiltinMTRWithTimeout(ctx, job, sink, defaultRouteProbeTimeout)
}

func runBuiltinMTRWithTimeout(ctx context.Context, job model.Job, sink StreamSink, probeTimeout time.Duration) (*model.ToolResult, error) {
	count := parsePositiveInt(argOr(job.Args, "count", "10"))
	maxHops := parsePositiveInt(argOr(job.Args, "max_hops", "30"))
	requestedMode, err := probeMode(argOr(job.Args, "protocol", "icmp"))
	if err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	if err := validateOptionalProbePort(job.Args); err != nil {
		result := newBuiltinResult(job, -1)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	mode := requestedMode
	acc := map[int][]model.ProbeResult{}
	var final *model.ToolResult
	var terminalErr error
	cyclesAttempted := 0
	observedMaxHop := 0
	for i := 0; i < count; i++ {
		cyclesAttempted++
		trace, err := runTrace(ctx, job, sink, maxHops, 1, mode, i == 0, probeTimeout)
		if trace != nil {
			if requestedMode == "auto" && i == 0 {
				mode = summaryString(trace.Summary, "protocol", "tcp")
			}
			for _, hop := range trace.Hops {
				acc[hop.Index] = append(acc[hop.Index], hop.Probes...)
				if hop.Index > observedMaxHop {
					observedMaxHop = hop.Index
				}
			}
		}
		if err != nil {
			final = trace
			terminalErr = err
			break
		}
		final = trace
		if trace.ExitCode == 0 {
			maxHops = len(trace.Hops)
		}
	}
	result := newBuiltinResult(job, 0)
	if final != nil {
		result.IPVersion = final.IPVersion
		result.ExitCode = final.ExitCode
		copyErrorSummary(result, final)
	}
	for i := 1; i <= observedMaxHop; i++ {
		probes := acc[i]
		if len(probes) == 0 {
			continue
		}
		hop := summarizeHop(i, probes, cyclesAttempted).FlattenSingleProbe()
		result.Hops = append(result.Hops, hop)
		emitHopSummary(sink, hop)
	}
	result.Summary["hop_count"] = len(result.Hops)
	setRouteProtocolSummary(result, requestedMode, mode, job.Args)
	result.Summary["cycles"] = cyclesAttempted
	return result, terminalErr
}

func setRouteProtocolSummary(result *model.ToolResult, requested string, actual string, args map[string]string) {
	result.Summary["protocol"] = actual
	if requested == "auto" {
		result.Summary["requested_protocol"] = "auto"
	}
	port := routeProbePort(args, actual)
	if port > 0 {
		result.Summary["port"] = port
	} else {
		delete(result.Summary, "port")
	}
}

func routeProbePort(args map[string]string, mode string) int {
	if mode == "icmp" || mode == "auto" {
		return 0
	}
	return parsePort(args, defaultProbePort(mode))
}

func routeContextError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func routeProbeTimedOut(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func routeErrorAllowsFallback(err error) bool {
	var unreachable *udpTraceICMPError
	return routeProbeTimedOut(err) || errors.As(err, &unreachable)
}

func fillRouteProbeErrorSummary(ctx context.Context, result *model.ToolResult, err error) {
	fillErrorSummary(ctx, result, err)
	var unreachable *udpTraceICMPError
	if errors.As(err, &unreachable) {
		setRouteICMPErrorSummary(result, unreachable.Type, unreachable.Code, unreachable.Peer, unreachable.Errno)
		return
	}
	var tcpUnreachable *tcpTraceICMPError
	if errors.As(err, &tcpUnreachable) {
		setRouteICMPErrorSummary(result, tcpUnreachable.Type, tcpUnreachable.Code, tcpUnreachable.Peer, tcpUnreachable.Errno)
	}
}

func setRouteICMPErrorSummary(result *model.ToolResult, icmpType byte, icmpCode byte, peer net.IP, errno syscall.Errno) {
	result.Summary["icmp_type"] = int(icmpType)
	result.Summary["icmp_code"] = int(icmpCode)
	if peer != nil {
		result.Summary["peer"] = peer.String()
	}
	if errno != 0 {
		result.Summary["errno"] = errno.Error()
	}
}

func summaryString(summary map[string]any, key string, fallback string) string {
	if value, ok := summary[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func normalizedRouteProbeTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultRouteProbeTimeout
	}
	return timeout
}

func runBuiltinHTTP(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	return runBuiltinHTTPWithTimeout(ctx, job, sink, defaultHTTPAttemptTimeout)
}

func runBuiltinHTTPWithTimeout(ctx context.Context, job model.Job, _ StreamSink, attemptTimeout time.Duration) (*model.ToolResult, error) {
	method, err := httpMethod(argOr(job.Args, "method", "GET"))
	if err != nil {
		result := newBuiltinResult(job, 1)
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	requestedVersion, err := httpVersionMode(argOr(job.Args, "http_version", "1.1"))
	if err != nil {
		result := newBuiltinResult(job, 1)
		result.Summary["method"] = method
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	versions, err := httpVersionCandidates(job.Target, requestedVersion)
	if err != nil {
		result := newBuiltinResult(job, 1)
		result.Summary["method"] = method
		setHTTPVersionSummary(result, requestedVersion, requestedVersion)
		fillErrorSummary(ctx, result, err)
		return result, err
	}

	var lastResult *model.ToolResult
	var lastErr error
	overallStart := time.Now()
	for index, version := range versions {
		firstResponseTimeout := time.Duration(0)
		if requestedVersion == "auto" && index < len(versions)-1 {
			firstResponseTimeout = normalizedHTTPAttemptTimeout(attemptTimeout)
		}
		result, responseStarted, err := executeHTTPProbeAttempt(ctx, job, method, version, firstResponseTimeout)
		setHTTPVersionSummary(result, requestedVersion, version)
		if result != nil {
			result.Summary["time_total_ms"] = elapsedMS(overallStart)
		}
		lastResult, lastErr = result, err
		if err == nil || requestedVersion != "auto" || responseStarted || ctx.Err() != nil {
			return result, err
		}
	}
	return lastResult, lastErr
}

type httpProbeTraceCollector struct {
	mu              sync.Mutex
	firstByteOnce   sync.Once
	firstByteSignal chan struct{}
	dnsStart        time.Time
	connectStart    time.Time
	tlsStart        time.Time
	firstByteAt     time.Time
	dnsMS           float64
	connectMS       float64
	tlsMS           float64
	remoteAddr      string
}

type httpProbeTraceSnapshot struct {
	firstByteAt time.Time
	dnsMS       float64
	connectMS   float64
	tlsMS       float64
	remoteAddr  string
}

func newHTTPProbeTraceCollector() *httpProbeTraceCollector {
	return &httpProbeTraceCollector{firstByteSignal: make(chan struct{})}
}

func (c *httpProbeTraceCollector) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			c.mu.Lock()
			c.dnsStart = time.Now()
			c.mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			c.mu.Lock()
			if !c.dnsStart.IsZero() {
				c.dnsMS = elapsedMS(c.dnsStart)
			}
			c.mu.Unlock()
		},
		ConnectStart: func(_, addr string) {
			c.mu.Lock()
			c.connectStart = time.Now()
			c.remoteAddr = addr
			c.mu.Unlock()
		},
		ConnectDone: func(_, addr string, _ error) {
			c.mu.Lock()
			c.remoteAddr = addr
			if !c.connectStart.IsZero() {
				c.connectMS = elapsedMS(c.connectStart)
			}
			c.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			c.mu.Lock()
			c.tlsStart = time.Now()
			c.mu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			c.mu.Lock()
			if !c.tlsStart.IsZero() {
				c.tlsMS = elapsedMS(c.tlsStart)
			}
			c.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			c.firstByteOnce.Do(func() {
				c.mu.Lock()
				c.firstByteAt = time.Now()
				c.mu.Unlock()
				close(c.firstByteSignal)
			})
		},
	}
}

func (c *httpProbeTraceCollector) snapshot() httpProbeTraceSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return httpProbeTraceSnapshot{
		firstByteAt: c.firstByteAt,
		dnsMS:       c.dnsMS,
		connectMS:   c.connectMS,
		tlsMS:       c.tlsMS,
		remoteAddr:  c.remoteAddr,
	}
}

func runHTTPProbeAttempt(ctx context.Context, job model.Job, method string, version string, firstResponseTimeout time.Duration) (*model.ToolResult, bool, error) {
	result := newBuiltinResult(job, 0)
	result.Summary["method"] = method
	var requestStart time.Time
	probeTrace := newHTTPProbeTraceCollector()
	trace := probeTrace.clientTrace()
	requestCtx, cancelRequest := context.WithCancel(httptrace.WithClientTrace(ctx, trace))
	defer cancelRequest()
	req, tlsServerName, err := newHTTPProbeRequest(requestCtx, method, job.Target)
	if err != nil {
		result.ExitCode = 1
		fillErrorSummary(ctx, result, err)
		return result, false, err
	}
	if version != "1.1" && req.URL.Scheme != "https" {
		err := fmt.Errorf("HTTP/%s requires an https target", version)
		result.ExitCode = 1
		fillErrorSummary(ctx, result, err)
		return result, false, err
	}

	transport, closeTransport := newHTTPProbeTransport(job, version, tlsServerName)
	defer closeTransport()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	requestStart = time.Now()
	resp, err := doHTTPProbeRequest(client, req, cancelRequest, probeTrace.firstByteSignal, firstResponseTimeout)
	timings := probeTrace.snapshot()
	if err != nil {
		result.ExitCode = 1
		fillHTTPSummary(result, httpProbeSummary{
			method:       method,
			requestStart: requestStart,
			dnsMS:        timings.dnsMS,
			connectMS:    timings.connectMS,
			tlsMS:        timings.tlsMS,
			firstByteAt:  timings.firstByteAt,
			remoteAddr:   timings.remoteAddr,
			err:          err,
			ctx:          ctx,
		})
		return result, !timings.firstByteAt.IsZero(), err
	}
	defer resp.Body.Close()
	var n int64
	var bodyTruncated bool
	downloadStart := time.Now()
	if method == http.MethodGet {
		n, bodyTruncated, err = copyHTTPResponseBody(resp.Body, maxHTTPResponseBodyBytes)
	} else {
		n, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
	}
	fillHTTPSummary(result, httpProbeSummary{
		method:        method,
		requestStart:  requestStart,
		downloadStart: downloadStart,
		dnsMS:         timings.dnsMS,
		connectMS:     timings.connectMS,
		tlsMS:         timings.tlsMS,
		firstByteAt:   timings.firstByteAt,
		remoteAddr:    timings.remoteAddr,
		resp:          resp,
		bytesRead:     n,
		bodyTruncated: bodyTruncated,
		err:           err,
		ctx:           ctx,
	})
	if err != nil {
		result.ExitCode = 1
		return result, true, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		result.ExitCode = 1
	}
	return result, true, nil
}

type httpProbeResponse struct {
	resp *http.Response
	err  error
}

func doHTTPProbeRequest(client *http.Client, req *http.Request, cancelRequest context.CancelFunc, firstByte <-chan struct{}, firstResponseTimeout time.Duration) (*http.Response, error) {
	if firstResponseTimeout <= 0 {
		return client.Do(req)
	}
	response := make(chan httpProbeResponse, 1)
	go func() {
		resp, err := client.Do(req)
		response <- httpProbeResponse{resp: resp, err: err}
	}()
	timer := time.NewTimer(firstResponseTimeout)
	defer timer.Stop()
	select {
	case outcome := <-response:
		return outcome.resp, outcome.err
	case <-firstByte:
		outcome := <-response
		return outcome.resp, outcome.err
	case <-timer.C:
		// Prefer a first byte or completed response that raced with the timer.
		// Once either exists, the overall job context owns the remaining budget.
		select {
		case outcome := <-response:
			return outcome.resp, outcome.err
		case <-firstByte:
			outcome := <-response
			return outcome.resp, outcome.err
		default:
		}
		cancelRequest()
		outcome := <-response
		if outcome.resp != nil {
			_ = outcome.resp.Body.Close()
		}
		return nil, fmt.Errorf("HTTP response did not start within %s: %w", firstResponseTimeout, context.DeadlineExceeded)
	}
}

func copyHTTPResponseBody(body io.Reader, limit int64) (int64, bool, error) {
	read, err := io.Copy(io.Discard, io.LimitReader(body, limit+1))
	if err != nil {
		return read, false, err
	}
	if read > limit {
		return limit, true, nil
	}
	return read, false, nil
}

func newHTTPProbeTransport(job model.Job, version string, tlsServerName string) (http.RoundTripper, func()) {
	tlsConfig := &tls.Config{ServerName: tlsServerName}
	if version == "3" {
		transport := &http3.Transport{
			TLSClientConfig:        tlsConfig,
			QUICConfig:             &quic.Config{},
			MaxResponseHeaderBytes: maxHTTPResponseHeaderBytes,
			Dial: func(ctx context.Context, address string, tlsConfig *tls.Config, quicConfig *quic.Config) (*quic.Conn, error) {
				address, err := httpQUICDialAddress(ctx, job, address)
				if err != nil {
					return nil, err
				}
				trace := httptrace.ContextClientTrace(ctx)
				if trace != nil && trace.ConnectStart != nil {
					trace.ConnectStart("udp", address)
				}
				if trace != nil && trace.TLSHandshakeStart != nil {
					trace.TLSHandshakeStart()
				}
				conn, err := quic.DialAddr(ctx, address, tlsConfig, quicConfig)
				var state tls.ConnectionState
				if conn != nil {
					state = conn.ConnectionState().TLS
				}
				if trace != nil && trace.TLSHandshakeDone != nil {
					trace.TLSHandshakeDone(state, err)
				}
				if trace != nil && trace.ConnectDone != nil {
					trace.ConnectDone("udp", address, err)
				}
				return conn, err
			},
		}
		return transport, func() { _ = transport.Close() }
	}

	protocols := new(http.Protocols)
	if version == "2" {
		protocols.SetHTTP2(true)
	} else {
		protocols.SetHTTP1(true)
	}
	transport := &http.Transport{
		Proxy:                  nil,
		Protocols:              protocols,
		TLSClientConfig:        tlsConfig,
		MaxResponseHeaderBytes: maxHTTPResponseHeaderBytes,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			address, err := httpDialAddress(job, address)
			if err != nil {
				return nil, err
			}
			switch job.IPVersion {
			case model.IPv4:
				network = strings.Replace(network, "tcp", "tcp4", 1)
			case model.IPv6:
				network = strings.Replace(network, "tcp", "tcp6", 1)
			}
			dialer := net.Dialer{Timeout: 10 * time.Second}
			return dialer.DialContext(ctx, network, address)
		},
	}
	return transport, transport.CloseIdleConnections
}

func httpQUICDialAddress(ctx context.Context, job model.Job, address string) (string, error) {
	address, err := httpDialAddress(job, address)
	if err != nil || job.ResolvedTarget != "" {
		return address, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return address, err
	}
	trace := httptrace.ContextClientTrace(ctx)
	if trace != nil && trace.DNSStart != nil {
		trace.DNSStart(httptrace.DNSStartInfo{Host: host})
	}
	ip, _, resolveErr := resolveOne(ctx, host, job.IPVersion)
	if trace != nil && trace.DNSDone != nil {
		info := httptrace.DNSDoneInfo{Err: resolveErr}
		if ip != nil {
			info.Addrs = []net.IPAddr{{IP: ip}}
		}
		trace.DNSDone(info)
	}
	if resolveErr != nil {
		return "", resolveErr
	}
	return net.JoinHostPort(ip.String(), port), nil
}

func httpVersionMode(raw string) (string, error) {
	switch raw {
	case "auto":
		return "auto", nil
	case "3":
		return "3", nil
	case "2":
		return "2", nil
	case "1.1":
		return "1.1", nil
	}
	return "", fmt.Errorf("unsupported HTTP version %q", raw)
}

func httpMethod(raw string) (string, error) {
	switch raw {
	case http.MethodGet, http.MethodHead:
		return raw, nil
	default:
		return "", fmt.Errorf("unsupported HTTP method %q", raw)
	}
}

func httpVersionCandidates(target string, requested string) ([]string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	if requested == "auto" {
		if u.Scheme == "https" {
			return []string{"3", "2", "1.1"}, nil
		}
		return []string{"1.1"}, nil
	}
	if requested != "1.1" && u.Scheme != "https" {
		return nil, fmt.Errorf("HTTP/%s requires an https target", requested)
	}
	return []string{requested}, nil
}

func normalizedHTTPAttemptTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultHTTPAttemptTimeout
	}
	return timeout
}

func setHTTPVersionSummary(result *model.ToolResult, requested string, attempted string) {
	if result == nil {
		return
	}
	actual := attempted
	if protocol, ok := result.Summary["http_protocol"].(string); ok {
		actual = protocol
	}
	result.Summary["http_version"] = actual
	delete(result.Summary, "http_protocol")
	if requested == "auto" {
		result.Summary["requested_http_version"] = "auto"
	}
}

type httpProbeSummary struct {
	method        string
	requestStart  time.Time
	downloadStart time.Time
	dnsMS         float64
	connectMS     float64
	tlsMS         float64
	firstByteAt   time.Time
	remoteAddr    string
	resp          *http.Response
	bytesRead     int64
	bodyTruncated bool
	err           error
	ctx           context.Context
}

func fillHTTPSummary(result *model.ToolResult, summary httpProbeSummary) {
	result.Summary["method"] = summary.method
	if !summary.requestStart.IsZero() {
		result.Summary["time_total_ms"] = elapsedMS(summary.requestStart)
	}
	result.Summary["time_connect_ms"] = summary.connectMS
	result.Summary["time_dns_ms"] = summary.dnsMS
	result.Summary["time_tls_ms"] = summary.tlsMS
	if !summary.firstByteAt.IsZero() {
		result.Summary["time_first_byte_ms"] = float64(summary.firstByteAt.Sub(summary.requestStart).Microseconds()) / 1000
	} else if !summary.downloadStart.IsZero() {
		result.Summary["time_first_byte_ms"] = float64(summary.downloadStart.Sub(summary.requestStart).Microseconds()) / 1000
	}
	if !summary.downloadStart.IsZero() {
		downloadMS := elapsedMS(summary.downloadStart)
		result.Summary["time_download_ms"] = downloadMS
		result.Summary["bytes_downloaded"] = summary.bytesRead
		result.Summary["download_bytes_per_sec"] = bytesPerSecond(summary.bytesRead, downloadMS)
	}
	if summary.resp != nil {
		result.Summary["http_code"] = summary.resp.StatusCode
		result.Summary["content_length"] = summary.resp.ContentLength
		result.Summary["http_protocol"] = responseHTTPVersion(summary.resp)
	}
	if summary.bodyTruncated {
		result.Summary["body_truncated"] = true
		result.Summary["body_limit_bytes"] = int64(maxHTTPResponseBodyBytes)
	}
	if summary.remoteAddr != "" {
		result.Summary["remote_addr"] = summary.remoteAddr
	}
	if summary.err != nil {
		result.Summary["status"] = httpErrorStatus(summary.ctx, summary.err)
		result.Summary["error"] = summary.err.Error()
	}
}

func responseHTTPVersion(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	switch resp.ProtoMajor {
	case 3:
		return "3"
	case 2:
		return "2"
	default:
		return "1.1"
	}
}

func httpErrorStatus(ctx context.Context, err error) string {
	return genericErrorStatus(ctx, err)
}

func fillErrorSummary(ctx context.Context, result *model.ToolResult, err error) {
	if result == nil || err == nil {
		return
	}
	result.Summary["status"] = genericErrorStatus(ctx, err)
	result.Summary["error"] = err.Error()
}

func copyErrorSummary(dst *model.ToolResult, src *model.ToolResult) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range []string{"status", "error", "icmp_type", "icmp_code", "peer", "errno"} {
		if value, ok := src.Summary[key]; ok {
			dst.Summary[key] = value
		}
	}
}

func statusOrFallback(ctx context.Context, err error, fallback string) string {
	status := genericErrorStatus(ctx, err)
	if status == "error" {
		return fallback
	}
	return status
}

func genericErrorStatus(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "error"
}

func newHTTPProbeRequest(ctx context.Context, method string, target string) (*http.Request, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, "", err
	}
	req.Host = req.URL.Host
	return req, httpTLSServerName(req.URL), nil
}

func httpTLSServerName(u *url.URL) string {
	if u == nil || u.Scheme != "https" {
		return ""
	}
	host := u.Hostname()
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	return host
}

func httpDialAddress(job model.Job, address string) (string, error) {
	if job.ResolvedTarget == "" {
		return address, nil
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(job.ResolvedTarget, port), nil
}

func tcpProbe(ctx context.Context, target net.IP, version model.IPVersion, port int, ttl int) (time.Duration, error) {
	return tcpProbeWithTimeout(ctx, target, version, port, ttl, tcpProbeTimeout)
}

func tcpProbeWithTimeout(ctx context.Context, target net.IP, version model.IPVersion, port int, ttl int, timeout time.Duration) (time.Duration, error) {
	network := "tcp4"
	if version == model.IPv6 {
		network = "tcp6"
	}
	dialer := net.Dialer{
		Timeout: timeout,
		Control: func(network string, address string, c syscall.RawConn) error {
			var controlErr error
			err := c.Control(func(fd uintptr) {
				if version == model.IPv6 {
					controlErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_UNICAST_HOPS, ttl)
					return
				}
				controlErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL, ttl)
			})
			if err != nil {
				return err
			}
			return controlErr
		},
	}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(target.String(), strconv.Itoa(port)))
	if err != nil {
		return time.Since(start), err
	}
	_ = conn.Close()
	return time.Since(start), nil
}

func runBuiltinDNS(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	result := newBuiltinResult(job, 0)
	records, err := lookupRecords(ctx, job.Target, argOr(job.Args, "type", "A"))
	if err != nil {
		result.ExitCode = 1
		fillErrorSummary(ctx, result, err)
		return result, err
	}
	result.Records = records
	result.Summary["record_count"] = len(records)
	return result, nil
}

func runBuiltinPort(ctx context.Context, job model.Job, sink StreamSink) (*model.ToolResult, error) {
	result := newBuiltinResult(job, 0)
	if err := validateOptionalProbePort(job.Args); err != nil {
		result.ExitCode = 1
		result.Summary["status"] = "invalid_port"
		return result, err
	}
	port := parsePort(job.Args, 0)
	if port <= 0 {
		result.ExitCode = 1
		result.Summary["status"] = "invalid_port"
		return result, errors.New("port is required")
	}
	targetIP, version, err := resolveOne(ctx, executionTarget(job), job.IPVersion)
	if err != nil {
		result.ExitCode = 1
		result.Summary["status"] = statusOrFallback(ctx, err, "resolve_failed")
		result.Summary["error"] = err.Error()
		return result, err
	}
	result.IPVersion = version
	start := time.Now()
	_, err = tcpProbe(ctx, targetIP, version, port, 64)
	result.Summary["port"] = port
	result.Summary["protocol"] = "tcp"
	result.Summary["peer"] = targetIP.String()
	result.Summary["connect_ms"] = elapsedMS(start)
	if err != nil {
		result.ExitCode = 1
		result.Summary["status"] = statusOrFallback(ctx, err, "closed")
		result.Summary["error"] = err.Error()
		return result, nil
	}
	result.Summary["status"] = "open"
	return result, nil
}

func lookupRecords(ctx context.Context, target string, typ string) ([]model.DNSRecord, error) {
	resolver := net.DefaultResolver
	var records []model.DNSRecord
	switch typ {
	case "A", "AAAA":
		network := "ip"
		if typ == "A" {
			network = "ip4"
		}
		if typ == "AAAA" {
			network = "ip6"
		}
		ips, err := resolver.LookupIP(ctx, network, target)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			records = append(records, model.DNSRecord{Type: typ, Value: ip.String()})
		}
	case "CNAME":
		cname, err := resolver.LookupCNAME(ctx, target)
		if err != nil {
			return nil, err
		}
		records = append(records, model.DNSRecord{Type: typ, Value: strings.TrimSuffix(cname, ".")})
	case "MX":
		mx, err := resolver.LookupMX(ctx, target)
		if err != nil {
			return nil, err
		}
		for _, item := range mx {
			records = append(records, model.DNSRecord{Type: typ, Value: fmt.Sprintf("%d %s", item.Pref, strings.TrimSuffix(item.Host, "."))})
		}
	case "NS":
		ns, err := resolver.LookupNS(ctx, target)
		if err != nil {
			return nil, err
		}
		for _, item := range ns {
			records = append(records, model.DNSRecord{Type: typ, Value: strings.TrimSuffix(item.Host, ".")})
		}
	case "TXT":
		txt, err := resolver.LookupTXT(ctx, target)
		if err != nil {
			return nil, err
		}
		for _, item := range txt {
			records = append(records, model.DNSRecord{Type: typ, Value: item})
		}
	default:
		return nil, fmt.Errorf("unsupported dns type %s", typ)
	}
	return records, nil
}

func resolveOne(ctx context.Context, target string, version model.IPVersion) (net.IP, model.IPVersion, error) {
	if ip := net.ParseIP(target); ip != nil {
		return ip, ipVersion(ip), nil
	}
	network := "ip"
	switch version {
	case model.IPv4:
		network = "ip4"
	case model.IPv6:
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, network, target)
	if err != nil {
		return nil, model.IPAny, err
	}
	if len(ips) == 0 {
		return nil, model.IPAny, errors.New("target resolved to no addresses")
	}
	ip := preferredIP(ips, version)
	if ip == nil {
		return nil, model.IPAny, errors.New("target resolved to no addresses")
	}
	return ip, ipVersion(ip), nil
}

func preferredIP(ips []net.IP, version model.IPVersion) net.IP {
	if version == model.IPAny {
		for _, ip := range ips {
			if ip != nil && ip.To4() == nil {
				return ip
			}
		}
	}
	for _, ip := range ips {
		if ip != nil {
			return ip
		}
	}
	return nil
}

func executionTarget(job model.Job) string {
	if job.ResolvedTarget != "" {
		return job.ResolvedTarget
	}
	return job.Target
}

func ipVersion(ip net.IP) model.IPVersion {
	if ip.To4() != nil {
		return model.IPv4
	}
	return model.IPv6
}

func listenICMP(version model.IPVersion) (*icmp.PacketConn, error) {
	if version == model.IPv6 {
		return icmp.ListenPacket("ip6:ipv6-icmp", "::")
	}
	return icmp.ListenPacket("ip4:icmp", "0.0.0.0")
}

func setTTL(conn *icmp.PacketConn, version model.IPVersion, ttl int) error {
	if version == model.IPv6 {
		return conn.IPv6PacketConn().SetHopLimit(ttl)
	}
	return conn.IPv4PacketConn().SetTTL(ttl)
}

func ipAddr(ip net.IP) net.Addr {
	return &net.IPAddr{IP: ip}
}

func addrIP(addr net.Addr) net.IP {
	switch a := addr.(type) {
	case *net.IPAddr:
		return a.IP
	case *net.UDPAddr:
		return a.IP
	default:
		host, _, err := net.SplitHostPort(addr.String())
		if err == nil {
			if ip := net.ParseIP(host); ip != nil {
				return ip
			}
		}
		return net.ParseIP(addr.String())
	}
}

func newBuiltinResult(job model.Job, exitCode int) *model.ToolResult {
	return &model.ToolResult{
		Tool:      job.Tool,
		Target:    job.Target,
		IPVersion: job.IPVersion,
		ExitCode:  exitCode,
		Summary:   map[string]any{},
	}
}

func fillPingSummary(result *model.ToolResult, transmitted int, received int, samples []float64) {
	loss := 0.0
	if transmitted > 0 {
		loss = float64(transmitted-received) / float64(transmitted) * 100
	}
	result.Summary["packets_transmitted"] = transmitted
	result.Summary["packets_received"] = received
	result.Summary["packet_loss_pct"] = loss
	if len(samples) == 0 {
		return
	}
	minV, maxV, sum := samples[0], samples[0], 0.0
	for _, sample := range samples {
		minV = math.Min(minV, sample)
		maxV = math.Max(maxV, sample)
		sum += sample
	}
	avg := sum / float64(len(samples))
	variance := 0.0
	for _, sample := range samples {
		delta := sample - avg
		variance += delta * delta
	}
	result.Summary["rtt_min_ms"] = minV
	result.Summary["rtt_avg_ms"] = avg
	result.Summary["rtt_max_ms"] = maxV
	result.Summary["rtt_mdev_ms"] = math.Sqrt(variance / float64(len(samples)))
}

func summarizeHop(index int, probes []model.ProbeResult, sent int) model.HopResult {
	hop := model.HopResult{Index: index, Probes: probes}
	received := 0
	var samples []float64
	for _, probe := range probes {
		if probe.IP != "" && hop.IP == "" {
			hop.IP = probe.IP
			hop.Host = probe.Host
		}
		if probe.RTTMS != nil {
			received++
			samples = append(samples, *probe.RTTMS)
		}
	}
	loss := 0.0
	if sent > 0 {
		loss = float64(sent-received) / float64(sent) * 100
	}
	hop.LossPct = &loss
	hop.Sent = &sent
	if len(samples) == 0 {
		return hop
	}
	minV, maxV, sum := samples[0], samples[0], 0.0
	for _, sample := range samples {
		minV = math.Min(minV, sample)
		maxV = math.Max(maxV, sample)
		sum += sample
	}
	avg := sum / float64(len(samples))
	last := samples[len(samples)-1]
	variance := 0.0
	for _, sample := range samples {
		delta := sample - avg
		variance += delta * delta
	}
	stdev := math.Sqrt(variance / float64(len(samples)))
	hop.AvgMS = &avg
	hop.BestMS = &minV
	hop.WorstMS = &maxV
	hop.LastMS = &last
	hop.StdevMS = &stdev
	return hop
}

func flattenResultHops(result *model.ToolResult) {
	if result == nil {
		return
	}
	for i := range result.Hops {
		result.Hops[i] = result.Hops[i].FlattenSingleProbe()
	}
}

func probeMode(raw string) (string, error) {
	switch raw {
	case "auto":
		return "auto", nil
	case "icmp":
		return "icmp", nil
	case "udp":
		return "udp", nil
	case "tcp":
		return "tcp", nil
	}
	return "", fmt.Errorf("unsupported probe protocol %q", raw)
}

func defaultProbePort(mode string) int {
	switch mode {
	case "udp":
		return defaultUDPProbePort
	case "tcp":
		return 80
	default:
		return 0
	}
}

func parsePort(args map[string]string, fallback int) int {
	if args == nil || args["port"] == "" {
		return fallback
	}
	port, err := canonicalProbePort(args["port"])
	if err != nil {
		return 0
	}
	return port
}

func validateOptionalProbePort(args map[string]string) error {
	if args == nil || args["port"] == "" {
		return nil
	}
	_, err := canonicalProbePort(args["port"])
	return err
}

func canonicalProbePort(value string) (int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, fmt.Errorf("invalid port %q", value)
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid port %q", value)
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", value)
	}
	return port, nil
}

func elapsedMS(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

func bytesPerSecond(bytes int64, ms float64) float64 {
	if ms <= 0 {
		return 0
	}
	return float64(bytes) / (ms / 1000)
}

func emitMetric(sink StreamSink, event model.StreamEvent) {
	if sink != nil {
		_ = sink(event)
	}
}

func emitPingTimeoutMetric(sink StreamSink, ctx context.Context, err error, seq int) {
	if genericErrorStatus(ctx, err) != "timeout" {
		return
	}
	if seq < 1 {
		seq = 1
	}
	emitMetric(sink, model.StreamEvent{Type: "metric", Metric: map[string]any{"seq": seq, "timeout": true}})
}

func emitPingProbeMetric(sink StreamSink, ctx context.Context, seq int, rtt time.Duration, err error) {
	metric := map[string]any{
		"seq": seq,
	}
	if err == nil {
		metric["latency_ms"] = float64(rtt.Microseconds()) / 1000
	} else {
		status := genericErrorStatus(ctx, err)
		if status == "timeout" {
			metric["timeout"] = true
		} else {
			metric["error"] = err.Error()
		}
	}
	emitMetric(sink, model.StreamEvent{Type: "metric", Metric: metric})
}

func emitResolvedTarget(sink StreamSink, target net.IP, version model.IPVersion) {
	emitMetric(sink, model.StreamEvent{
		Type: "target_resolved",
		Metric: map[string]any{
			"target_ip":  target.String(),
			"ip_version": version,
		},
	})
}

func emitHop(sink StreamSink, hop model.HopResult) {
	if sink != nil {
		hop = hop.FlattenSingleProbe()
		_ = sink(model.StreamEvent{Type: "hop", Hop: &hop})
	}
}

func emitHopSummary(sink StreamSink, hop model.HopResult) {
	if sink != nil {
		hop.Probes = nil
		hop.TimesMS = nil
		_ = sink(model.StreamEvent{Type: "hop_summary", Hop: &hop})
	}
}
