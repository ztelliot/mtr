//go:build linux

package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ztelliot/mtr/internal/model"
)

func TestParseTCPTraceICMPIPv4MatchesQuotedFourTuple(t *testing.T) {
	key := tcpTraceProbeKey{
		source:     net.ParseIP("192.0.2.10"),
		target:     net.ParseIP("198.51.100.20"),
		sourcePort: 40123,
		targetPort: 443,
	}
	peerIP := net.ParseIP("203.0.113.7")
	packet := tcpTraceIPv4ICMPPacket(t, icmpTimeExceeded, 0, peerIP, key)

	peer, reached, matched, err := parseTCPTraceICMP(packet, nil, model.IPv4, key)
	if err != nil || !matched || reached || !peer.Equal(peerIP) {
		t.Fatalf("IPv4 Time Exceeded mismatch: peer=%v reached=%v matched=%v err=%v", peer, reached, matched, err)
	}

	wrongPort := key
	wrongPort.targetPort++
	if _, _, matched, err := parseTCPTraceICMP(packet, nil, model.IPv4, wrongPort); err != nil || matched {
		t.Fatalf("IPv4 ICMP for another TCP probe was accepted: matched=%v err=%v", matched, err)
	}
}

func TestParseTCPTraceICMPIPv6MatchesQuotedFourTuple(t *testing.T) {
	key := tcpTraceProbeKey{
		source:     net.ParseIP("2001:db8:1::10"),
		target:     net.ParseIP("2001:db8:2::20"),
		sourcePort: 40234,
		targetPort: 8443,
	}
	peerIP := net.ParseIP("2001:db8:ffff::7")
	packet := tcpTraceIPv6ICMPPacket(t, icmpv6TimeExceeded, 0, key)
	from := &unix.SockaddrInet6{}
	copy(from.Addr[:], peerIP.To16())

	peer, reached, matched, err := parseTCPTraceICMP(packet, from, model.IPv6, key)
	if err != nil || !matched || reached || !peer.Equal(peerIP) {
		t.Fatalf("IPv6 Time Exceeded mismatch: peer=%v reached=%v matched=%v err=%v", peer, reached, matched, err)
	}

	wrongSource := key
	wrongSource.source = net.ParseIP("2001:db8:1::11")
	if _, _, matched, err := parseTCPTraceICMP(packet, from, model.IPv6, wrongSource); err != nil || matched {
		t.Fatalf("IPv6 ICMP for another TCP probe was accepted: matched=%v err=%v", matched, err)
	}
}

func TestParseTCPTraceICMPDestinationUnreachableIsStructured(t *testing.T) {
	key := tcpTraceProbeKey{
		source:     net.ParseIP("192.0.2.10"),
		target:     net.ParseIP("198.51.100.20"),
		sourcePort: 40345,
		targetPort: 80,
	}
	peerIP := net.ParseIP("203.0.113.8")
	packet := tcpTraceIPv4ICMPPacket(t, icmpDestinationUnreachable, 1, peerIP, key)

	peer, reached, matched, err := parseTCPTraceICMP(packet, nil, model.IPv4, key)
	var unreachable *tcpTraceICMPError
	if !matched || reached || !peer.Equal(peerIP) || !errors.As(err, &unreachable) {
		t.Fatalf("destination unreachable was not preserved: peer=%v reached=%v matched=%v err=%v", peer, reached, matched, err)
	}
	if unreachable.Type != icmpDestinationUnreachable || unreachable.Code != 1 || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("destination unreachable details lost: %#v", unreachable)
	}
}

func TestClassifyTCPTraceConnectOutcomeTreatsConnectAndRSTAsReached(t *testing.T) {
	target := net.ParseIP("192.0.2.1")
	for _, connectErr := range []error{nil, syscall.ECONNREFUSED} {
		peer, reached, complete, err := classifyTCPTraceConnectOutcome(target, connectErr)
		if err != nil || !complete || !reached || !peer.Equal(target) {
			t.Fatalf("connect outcome %v: peer=%v reached=%v complete=%v err=%v", connectErr, peer, reached, complete, err)
		}
	}
}

func TestDrainTCPTraceICMPContinuousUnmatchedTrafficHonorsDeadline(t *testing.T) {
	key := tcpTraceProbeKey{
		source:     net.ParseIP("192.0.2.10"),
		target:     net.ParseIP("198.51.100.20"),
		sourcePort: 40456,
		targetPort: 443,
	}
	otherProbe := key
	otherProbe.sourcePort++
	unmatched := tcpTraceIPv4ICMPPacket(t, icmpTimeExceeded, 0, net.ParseIP("203.0.113.9"), otherProbe)
	receiverCalls := 0
	recv := func(buffer []byte) (int, unix.Sockaddr, error) {
		receiverCalls++
		return copy(buffer, unmatched), nil, nil
	}
	deadline := time.Now().Add(10 * time.Millisecond)
	packet := make([]byte, 4096)
	for {
		_, _, matched, err := drainTCPTraceICMP(context.Background(), deadline, packet, recv, model.IPv4, key)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
		if err != nil || matched {
			t.Fatalf("unmatched ICMP drain: matched=%v err=%v", matched, err)
		}
	}
	if receiverCalls < maxTCPTraceICMPDrain {
		t.Fatalf("continuous traffic test did not exercise a full bounded drain: calls=%d", receiverCalls)
	}
}

func tcpTraceIPv4ICMPPacket(t *testing.T, icmpType byte, icmpCode byte, peer net.IP, key tcpTraceProbeKey) []byte {
	t.Helper()
	packet := make([]byte, 20+8+20+8)
	packet[0] = 0x45
	packet[9] = unix.IPPROTO_ICMP
	copy(packet[12:16], peer.To4())
	copy(packet[16:20], key.source.To4())
	packet[20] = icmpType
	packet[21] = icmpCode
	inner := packet[28:]
	inner[0] = 0x45
	inner[9] = ipProtocolTCP
	copy(inner[12:16], key.source.To4())
	copy(inner[16:20], key.target.To4())
	binary.BigEndian.PutUint16(inner[20:22], key.sourcePort)
	binary.BigEndian.PutUint16(inner[22:24], key.targetPort)
	return packet
}

func tcpTraceIPv6ICMPPacket(t *testing.T, icmpType byte, icmpCode byte, key tcpTraceProbeKey) []byte {
	t.Helper()
	packet := make([]byte, 8+ipv6HeaderLength+8)
	packet[0] = icmpType
	packet[1] = icmpCode
	inner := packet[8:]
	inner[0] = 0x60
	inner[6] = ipProtocolTCP
	copy(inner[8:24], key.source.To16())
	copy(inner[24:40], key.target.To16())
	binary.BigEndian.PutUint16(inner[40:42], key.sourcePort)
	binary.BigEndian.PutUint16(inner[42:44], key.targetPort)
	return packet
}
