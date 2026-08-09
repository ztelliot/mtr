//go:build linux

package runner

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ztelliot/mtr/internal/model"
)

const (
	ipProtocolTCP        = 6
	ipv6HeaderLength     = 40
	maxTCPTraceICMPDrain = 64
)

// tcpTraceICMPError preserves an ICMP rejection for a TCP traceroute probe.
// Time Exceeded is returned as a successful intermediate-hop observation;
// other matched ICMP errors terminate the explicit TCP trace with details.
type tcpTraceICMPError struct {
	Version model.IPVersion
	Type    byte
	Code    byte
	Peer    net.IP
	Errno   syscall.Errno
}

func (e *tcpTraceICMPError) Error() string {
	peer := "unknown peer"
	if e.Peer != nil {
		peer = e.Peer.String()
	}
	if e.Errno != 0 {
		return fmt.Sprintf("TCP traceroute received ICMP type %d code %d from %s: %v", e.Type, e.Code, peer, e.Errno)
	}
	return fmt.Sprintf("TCP traceroute received ICMP type %d code %d from %s", e.Type, e.Code, peer)
}

func (e *tcpTraceICMPError) Unwrap() error {
	if e.Errno == 0 {
		return nil
	}
	return e.Errno
}

type tcpTraceProbeKey struct {
	source     net.IP
	target     net.IP
	sourcePort uint16
	targetPort uint16
}

// tcpTraceProbeWithTimeout runs one nonblocking connect and listens on a raw
// ICMP socket opened before the SYN is sent. Linux does not expose an error
// queue for SOCK_STREAM, so matching the quoted inner IP/TCP four-tuple is what
// associates a Time Exceeded response with this exact probe.
func tcpTraceProbeWithTimeout(ctx context.Context, target net.IP, version model.IPVersion, port int, ttl int, timeout time.Duration) (time.Duration, net.IP, bool, error) {
	family := unix.AF_INET
	icmpProtocol := unix.IPPROTO_ICMP
	protocolLevel := unix.IPPROTO_IP
	ttlOption := unix.IP_TTL
	if version == model.IPv6 {
		family = unix.AF_INET6
		icmpProtocol = unix.IPPROTO_ICMPV6
		protocolLevel = unix.IPPROTO_IPV6
		ttlOption = unix.IPV6_UNICAST_HOPS
	}

	icmpFD, err := unix.Socket(family, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, icmpProtocol)
	if err != nil {
		return 0, nil, false, err
	}
	defer unix.Close(icmpFD)

	tcpFD, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
	if err != nil {
		return 0, nil, false, err
	}
	defer unix.Close(tcpFD)
	if err := unix.SetsockoptInt(tcpFD, protocolLevel, ttlOption, ttl); err != nil {
		return 0, nil, false, err
	}

	start := time.Now()
	connectErr := unix.Connect(tcpFD, tcpTraceSockaddr(target, version, port))
	if peer, reached, complete, outcomeErr := classifyTCPTraceConnectOutcome(target, connectErr); complete {
		return time.Since(start), peer, reached, outcomeErr
	}

	key, err := tcpTraceKeyFromSocket(tcpFD, target, version, port)
	if err != nil {
		return 0, nil, false, err
	}
	deadline := start.Add(normalizedRouteProbeTimeout(timeout))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}

	pollFDs := []unix.PollFd{
		{Fd: int32(icmpFD), Events: unix.POLLIN},
		{Fd: int32(tcpFD), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP},
	}
	packet := make([]byte, 4096)
	var pendingSocketErr error
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, false, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if pendingSocketErr != nil {
				return time.Since(start), nil, false, pendingSocketErr
			}
			return 0, nil, false, os.ErrDeadlineExceeded
		}
		wait := remaining
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		waitMS := int((wait + time.Millisecond - 1) / time.Millisecond)
		for index := range pollFDs {
			pollFDs[index].Revents = 0
		}
		ready, err := unix.Poll(pollFDs, waitMS)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return 0, nil, false, err
		}
		if ready == 0 {
			continue
		}

		if pollFDs[0].Revents&unix.POLLIN != 0 {
			peer, reached, matched, drainErr := drainTCPTraceICMP(ctx, deadline, packet, func(buffer []byte) (int, unix.Sockaddr, error) {
				return unix.Recvfrom(icmpFD, buffer, unix.MSG_DONTWAIT)
			}, version, key)
			if drainErr != nil {
				return 0, nil, false, drainErr
			}
			if matched {
				return time.Since(start), peer, reached, nil
			}
		}

		if pollFDs[1].Revents&(unix.POLLOUT|unix.POLLERR|unix.POLLHUP) == 0 {
			continue
		}
		if pendingSocketErr != nil {
			continue
		}
		socketErr, err := unix.GetsockoptInt(tcpFD, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return 0, nil, false, err
		}
		var outcomeErr error
		if socketErr != 0 {
			outcomeErr = syscall.Errno(socketErr)
		}
		if peer, reached, complete, outcomeErr := classifyTCPTraceConnectOutcome(target, outcomeErr); complete {
			if outcomeErr == nil {
				return time.Since(start), peer, reached, nil
			}
			// An asynchronous routing error can become visible on the TCP socket
			// just before its raw ICMP copy is readable. Preserve it, but leave
			// the raw socket until the probe deadline to recover the hop address.
			pendingSocketErr = outcomeErr
			// A pollfd with Events=0 can still wake continuously for POLLERR or
			// POLLHUP. Disable the descriptor entirely while the raw socket gets
			// a chance to deliver the corresponding ICMP packet.
			pollFDs[1].Fd = -1
			pollFDs[1].Events = 0
		}
	}
}

type tcpTraceICMPReceiver func([]byte) (int, unix.Sockaddr, error)

// drainTCPTraceICMP bounds one raw-socket drain so unrelated ICMP traffic
// cannot starve the outer poll loop. The absolute deadline is checked before
// every packet as well, so a continuously readable socket cannot extend a
// probe beyond its configured timeout.
func drainTCPTraceICMP(ctx context.Context, deadline time.Time, packet []byte, recv tcpTraceICMPReceiver, version model.IPVersion, key tcpTraceProbeKey) (net.IP, bool, bool, error) {
	for drained := 0; drained < maxTCPTraceICMPDrain; drained++ {
		if err := ctx.Err(); err != nil {
			return nil, false, false, err
		}
		if !time.Now().Before(deadline) {
			return nil, false, false, os.ErrDeadlineExceeded
		}
		n, from, err := recv(packet)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, false, nil
		}
		if err != nil {
			return nil, false, false, err
		}
		peer, reached, matched, parseErr := parseTCPTraceICMP(packet[:n], from, version, key)
		if matched || parseErr != nil {
			return peer, reached, matched, parseErr
		}
	}
	return nil, false, false, nil
}

func classifyTCPTraceConnectOutcome(target net.IP, err error) (net.IP, bool, bool, error) {
	if err == nil || errors.Is(err, unix.EISCONN) || errors.Is(err, unix.ECONNREFUSED) {
		return target, true, true, nil
	}
	if errors.Is(err, unix.EINPROGRESS) || errors.Is(err, unix.EALREADY) || errors.Is(err, unix.EINTR) {
		return nil, false, false, nil
	}
	return nil, false, true, err
}

func tcpTraceSockaddr(target net.IP, version model.IPVersion, port int) unix.Sockaddr {
	if version == model.IPv6 {
		address := [16]byte{}
		copy(address[:], target.To16())
		return &unix.SockaddrInet6{Port: port, Addr: address}
	}
	address := [4]byte{}
	copy(address[:], target.To4())
	return &unix.SockaddrInet4{Port: port, Addr: address}
}

func tcpTraceKeyFromSocket(fd int, target net.IP, version model.IPVersion, targetPort int) (tcpTraceProbeKey, error) {
	local, err := unix.Getsockname(fd)
	if err != nil {
		return tcpTraceProbeKey{}, err
	}
	key := tcpTraceProbeKey{target: append(net.IP(nil), target...), targetPort: uint16(targetPort)}
	switch address := local.(type) {
	case *unix.SockaddrInet4:
		if version != model.IPv4 || address.Port < 1 || address.Port > 65535 {
			return tcpTraceProbeKey{}, errors.New("invalid IPv4 TCP traceroute source")
		}
		key.source = net.IPv4(address.Addr[0], address.Addr[1], address.Addr[2], address.Addr[3])
		key.sourcePort = uint16(address.Port)
	case *unix.SockaddrInet6:
		if version != model.IPv6 || address.Port < 1 || address.Port > 65535 {
			return tcpTraceProbeKey{}, errors.New("invalid IPv6 TCP traceroute source")
		}
		key.source = net.IP(append([]byte(nil), address.Addr[:]...))
		key.sourcePort = uint16(address.Port)
	default:
		return tcpTraceProbeKey{}, errors.New("unsupported TCP traceroute source address")
	}
	return key, nil
}

func parseTCPTraceICMP(packet []byte, from unix.Sockaddr, version model.IPVersion, key tcpTraceProbeKey) (net.IP, bool, bool, error) {
	peer := tcpTraceSockaddrIP(from)
	icmpOffset := 0
	if version == model.IPv4 && len(packet) >= 20 && packet[0]>>4 == 4 {
		headerLength := int(packet[0]&0x0f) * 4
		if headerLength < 20 || len(packet) < headerLength+8 || packet[9] != unix.IPPROTO_ICMP {
			return nil, false, false, nil
		}
		peer = net.IPv4(packet[12], packet[13], packet[14], packet[15])
		icmpOffset = headerLength
	} else if version == model.IPv6 && len(packet) >= ipv6HeaderLength && packet[0]>>4 == 6 {
		var ok bool
		icmpOffset, ok = ipv6TransportOffset(packet, unix.IPPROTO_ICMPV6)
		if !ok || len(packet) < icmpOffset+8 {
			return nil, false, false, nil
		}
		peer = net.IP(append([]byte(nil), packet[8:24]...))
	}
	if len(packet) < icmpOffset+8 {
		return nil, false, false, nil
	}
	icmpType := packet[icmpOffset]
	icmpCode := packet[icmpOffset+1]
	if !tcpTraceQuotedPacketMatches(packet[icmpOffset+8:], version, key) {
		return nil, false, false, nil
	}
	if isTCPTimeExceeded(version, icmpType) {
		return peer, false, true, nil
	}
	if isTCPDestinationUnreachable(version, icmpType) {
		return peer, false, true, &tcpTraceICMPError{
			Version: version,
			Type:    icmpType,
			Code:    icmpCode,
			Peer:    peer,
			Errno:   syscall.EHOSTUNREACH,
		}
	}
	return nil, false, false, nil
}

func tcpTraceQuotedPacketMatches(packet []byte, version model.IPVersion, key tcpTraceProbeKey) bool {
	var transportOffset int
	var source net.IP
	var target net.IP
	switch version {
	case model.IPv4:
		if len(packet) < 20 || packet[0]>>4 != 4 || packet[9] != ipProtocolTCP {
			return false
		}
		headerLength := int(packet[0]&0x0f) * 4
		if headerLength < 20 || len(packet) < headerLength+4 {
			return false
		}
		if binary.BigEndian.Uint16(packet[6:8])&0x1fff != 0 {
			return false
		}
		transportOffset = headerLength
		source = net.IPv4(packet[12], packet[13], packet[14], packet[15])
		target = net.IPv4(packet[16], packet[17], packet[18], packet[19])
	case model.IPv6:
		if len(packet) < ipv6HeaderLength || packet[0]>>4 != 6 {
			return false
		}
		var ok bool
		transportOffset, ok = ipv6TransportOffset(packet, ipProtocolTCP)
		if !ok || len(packet) < transportOffset+4 {
			return false
		}
		source = net.IP(append([]byte(nil), packet[8:24]...))
		target = net.IP(append([]byte(nil), packet[24:40]...))
	default:
		return false
	}
	return source.Equal(key.source) && target.Equal(key.target) &&
		binary.BigEndian.Uint16(packet[transportOffset:transportOffset+2]) == key.sourcePort &&
		binary.BigEndian.Uint16(packet[transportOffset+2:transportOffset+4]) == key.targetPort
}

func ipv6TransportOffset(packet []byte, wanted byte) (int, bool) {
	if len(packet) < ipv6HeaderLength || packet[0]>>4 != 6 {
		return 0, false
	}
	next := packet[6]
	offset := ipv6HeaderLength
	for extensions := 0; extensions < 8; extensions++ {
		if next == wanted {
			return offset, true
		}
		if offset+2 > len(packet) {
			return 0, false
		}
		var length int
		switch next {
		case unix.IPPROTO_HOPOPTS, unix.IPPROTO_ROUTING, unix.IPPROTO_DSTOPTS:
			length = (int(packet[offset+1]) + 1) * 8
		case unix.IPPROTO_FRAGMENT:
			length = 8
		case unix.IPPROTO_AH:
			length = (int(packet[offset+1]) + 2) * 4
		default:
			return 0, false
		}
		if length <= 0 || offset+length > len(packet) {
			return 0, false
		}
		next = packet[offset]
		offset += length
	}
	return 0, false
}

func tcpTraceSockaddrIP(address unix.Sockaddr) net.IP {
	switch value := address.(type) {
	case *unix.SockaddrInet4:
		return net.IPv4(value.Addr[0], value.Addr[1], value.Addr[2], value.Addr[3])
	case *unix.SockaddrInet6:
		return net.IP(append([]byte(nil), value.Addr[:]...))
	default:
		return nil
	}
}

func isTCPTimeExceeded(version model.IPVersion, icmpType byte) bool {
	if version == model.IPv6 {
		return icmpType == icmpv6TimeExceeded
	}
	return icmpType == icmpTimeExceeded
}

func isTCPDestinationUnreachable(version model.IPVersion, icmpType byte) bool {
	if version == model.IPv6 {
		return icmpType == icmpv6DestinationUnreachable
	}
	return icmpType == icmpDestinationUnreachable
}
