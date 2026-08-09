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
	sockExtendedErrSize          = 16
	icmpDestinationUnreachable   = 3
	icmpPortUnreachable          = 3
	icmpTimeExceeded             = 11
	icmpv6DestinationUnreachable = 1
	icmpv6PortUnreachable        = 4
	icmpv6TimeExceeded           = 3
)

// udpTraceICMPError preserves the ICMP response that terminated a UDP probe.
// It is intentionally distinct from a timeout: the network answered, but the
// answer was neither a hop-limit expiry nor the destination's closed UDP port.
type udpTraceICMPError struct {
	Version model.IPVersion
	Type    byte
	Code    byte
	Peer    net.IP
	Errno   syscall.Errno
}

func (e *udpTraceICMPError) Error() string {
	peer := "unknown peer"
	if e.Peer != nil {
		peer = e.Peer.String()
	}
	if e.Errno != 0 {
		return fmt.Sprintf("UDP traceroute received ICMP type %d code %d from %s: %v", e.Type, e.Code, peer, e.Errno)
	}
	return fmt.Sprintf("UDP traceroute received ICMP type %d code %d from %s", e.Type, e.Code, peer)
}

func (e *udpTraceICMPError) Unwrap() error {
	if e.Errno == 0 {
		return nil
	}
	return e.Errno
}

// udpTraceProbeWithTimeout sends one UDP datagram with the requested hop
// limit. Linux places the resulting ICMP error in the UDP socket error queue,
// so this probe does not need a raw ICMP socket or CAP_NET_RAW.
func udpTraceProbeWithTimeout(ctx context.Context, target net.IP, version model.IPVersion, port int, ttl int, timeout time.Duration) (time.Duration, net.IP, bool, error) {
	family := unix.AF_INET
	protocolLevel := unix.IPPROTO_IP
	recvErrOption := unix.IP_RECVERR
	ttlOption := unix.IP_TTL
	if version == model.IPv6 {
		family = unix.AF_INET6
		protocolLevel = unix.IPPROTO_IPV6
		recvErrOption = unix.IPV6_RECVERR
		ttlOption = unix.IPV6_UNICAST_HOPS
	}

	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return 0, nil, false, err
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, protocolLevel, recvErrOption, 1); err != nil {
		return 0, nil, false, err
	}
	if err := unix.SetsockoptInt(fd, protocolLevel, ttlOption, ttl); err != nil {
		return 0, nil, false, err
	}
	if err := unix.Connect(fd, udpTraceSockaddr(target, version, port)); err != nil {
		return 0, nil, false, err
	}

	start := time.Now()
	if err := unix.Send(fd, []byte("mtr-udp-trace"), 0); err != nil {
		return 0, nil, false, err
	}
	deadline := start.Add(normalizedRouteProbeTimeout(timeout))
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}

	pollFDs := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLERR}}
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, false, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, nil, false, os.ErrDeadlineExceeded
		}
		wait := remaining
		if wait > 100*time.Millisecond {
			wait = 100 * time.Millisecond
		}
		waitMS := int((wait + time.Millisecond - 1) / time.Millisecond)
		pollFDs[0].Revents = 0
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
		if pollFDs[0].Revents&unix.POLLERR != 0 {
			peer, reached, handled, err := readUDPTraceError(fd, version, target)
			if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
				return 0, nil, false, err
			}
			if handled {
				return time.Since(start), peer, reached, nil
			}
		}
		if pollFDs[0].Revents&unix.POLLIN != 0 {
			buf := make([]byte, 1500)
			if _, _, err := unix.Recvfrom(fd, buf, 0); err == nil {
				return time.Since(start), target, true, nil
			} else if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
				return 0, nil, false, err
			}
		}
	}
}

func udpTraceSockaddr(target net.IP, version model.IPVersion, port int) unix.Sockaddr {
	if version == model.IPv6 {
		address := [16]byte{}
		copy(address[:], target.To16())
		return &unix.SockaddrInet6{Port: port, Addr: address}
	}
	address := [4]byte{}
	copy(address[:], target.To4())
	return &unix.SockaddrInet4{Port: port, Addr: address}
}

func readUDPTraceError(fd int, version model.IPVersion, target net.IP) (net.IP, bool, bool, error) {
	payload := make([]byte, 1500)
	oob := make([]byte, 512)
	_, oobn, _, _, err := unix.Recvmsg(fd, payload, oob, unix.MSG_ERRQUEUE)
	if err != nil {
		return nil, false, false, err
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, false, false, err
	}
	for _, message := range messages {
		if !isUDPTraceErrorMessage(message, version) || len(message.Data) < sockExtendedErrSize {
			continue
		}
		errno := syscall.Errno(binary.NativeEndian.Uint32(message.Data[:4]))
		origin := message.Data[4]
		icmpType := message.Data[5]
		icmpCode := message.Data[6]
		peer := udpTraceOffenderIP(message.Data[sockExtendedErrSize:])
		responsePeer, reached, handled, classifyErr := classifyUDPTraceError(version, target, origin, icmpType, icmpCode, errno, peer)
		if handled || classifyErr != nil {
			return responsePeer, reached, handled, classifyErr
		}
	}
	return nil, false, false, nil
}

func classifyUDPTraceError(version model.IPVersion, target net.IP, origin byte, icmpType byte, icmpCode byte, errno syscall.Errno, peer net.IP) (net.IP, bool, bool, error) {
	if origin == unix.SO_EE_ORIGIN_LOCAL {
		if errno == 0 {
			errno = syscall.EIO
		}
		return nil, false, false, errno
	}
	if isUDPTimeExceeded(version, origin, icmpType) {
		return peer, false, true, nil
	}
	if isUDPPortUnreachable(version, origin, icmpType, icmpCode) {
		if peer == nil {
			return target, true, true, nil
		}
		return peer, peer.Equal(target), true, nil
	}
	if origin == unix.SO_EE_ORIGIN_ICMP || origin == unix.SO_EE_ORIGIN_ICMP6 {
		return peer, false, true, &udpTraceICMPError{
			Version: version,
			Type:    icmpType,
			Code:    icmpCode,
			Peer:    peer,
			Errno:   errno,
		}
	}
	if errno != 0 {
		return nil, false, false, errno
	}
	return nil, false, false, nil
}

func isUDPTraceErrorMessage(message unix.SocketControlMessage, version model.IPVersion) bool {
	if version == model.IPv6 {
		return message.Header.Level == unix.IPPROTO_IPV6 && message.Header.Type == unix.IPV6_RECVERR
	}
	return message.Header.Level == unix.IPPROTO_IP && message.Header.Type == unix.IP_RECVERR
}

func isUDPTimeExceeded(version model.IPVersion, origin byte, icmpType byte) bool {
	if version == model.IPv6 {
		return origin == unix.SO_EE_ORIGIN_ICMP6 && icmpType == icmpv6TimeExceeded
	}
	return origin == unix.SO_EE_ORIGIN_ICMP && icmpType == icmpTimeExceeded
}

func isUDPPortUnreachable(version model.IPVersion, origin byte, icmpType byte, icmpCode byte) bool {
	if version == model.IPv6 {
		return origin == unix.SO_EE_ORIGIN_ICMP6 && icmpType == icmpv6DestinationUnreachable && icmpCode == icmpv6PortUnreachable
	}
	return origin == unix.SO_EE_ORIGIN_ICMP && icmpType == icmpDestinationUnreachable && icmpCode == icmpPortUnreachable
}

func udpTraceOffenderIP(data []byte) net.IP {
	if len(data) < 2 {
		return nil
	}
	switch binary.NativeEndian.Uint16(data[:2]) {
	case unix.AF_INET:
		if len(data) < 8 {
			return nil
		}
		return net.IPv4(data[4], data[5], data[6], data[7])
	case unix.AF_INET6:
		if len(data) < 24 {
			return nil
		}
		return net.IP(append([]byte(nil), data[8:24]...))
	default:
		return nil
	}
}
