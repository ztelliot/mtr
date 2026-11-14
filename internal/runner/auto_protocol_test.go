package runner

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"golang.org/x/net/icmp"
)

func TestNetworkAutoRunsOnlyAuthorizedCandidates(t *testing.T) {
	oldPingListen, oldRouteListen := listenPingICMP, listenRouteICMP
	oldPingUDP, oldPingTCP := pingUDPProbe, pingTCPProbe
	oldRouteUDP, oldRouteTCP := routeUDPProbe, routeTCPProbe
	t.Cleanup(func() {
		listenPingICMP, listenRouteICMP = oldPingListen, oldRouteListen
		pingUDPProbe, pingTCPProbe = oldPingUDP, oldPingTCP
		routeUDPProbe, routeTCPProbe = oldRouteUDP, oldRouteTCP
	})
	for _, tool := range []model.Tool{model.ToolPing, model.ToolTraceroute, model.ToolMTR} {
		for _, tt := range []struct{ name, candidates, calls, actual string }{
			{"TCP only", "tcp", "tcp", "tcp"},
			{"ICMP disabled", "udp,tcp", "udp,tcp", "tcp"},
			{"UDP disabled", "icmp,tcp", "icmp,tcp", "tcp"},
			{"stop at last allowed protocol", "udp", "udp", "udp"},
			{"empty candidates", "", "", ""},
		} {
			t.Run(string(tool)+"/"+tt.name, func(t *testing.T) {
				var calls []string
				listen := func(model.IPVersion) (*icmp.PacketConn, error) {
					calls = append(calls, "icmp")
					return nil, errors.New("raw socket denied")
				}
				listenPingICMP, listenRouteICMP = listen, listen
				pingUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
					calls = append(calls, "udp")
					return 0, &net.DNSError{IsTimeout: true}
				}
				pingTCPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, error) {
					calls = append(calls, "tcp")
					return time.Millisecond, nil
				}
				routeUDPProbe = func(context.Context, net.IP, model.IPVersion, int, int, time.Duration) (time.Duration, net.IP, bool, error) {
					calls = append(calls, "udp")
					return 0, nil, false, &net.DNSError{IsTimeout: true}
				}
				routeTCPProbe = func(_ context.Context, target net.IP, _ model.IPVersion, _ int, _ int, _ time.Duration) (time.Duration, net.IP, bool, error) {
					calls = append(calls, "tcp")
					return time.Millisecond, target, true, nil
				}
				result, err := runBuiltin(context.Background(), model.Job{
					Tool: tool, Target: "127.0.0.1", IPVersion: model.IPv4,
					Args: map[string]string{"protocol": "auto", "count": "1", "max_hops": "1", policy.AutoProtocolsArg: tt.candidates},
				}, time.Second, nil)
				if (err != nil) != (tt.candidates == "") {
					t.Fatalf("unexpected execution error: %v", err)
				}
				if got := strings.Join(calls, ","); got != tt.calls {
					t.Fatalf("protocols attempted = %q, want %q", got, tt.calls)
				}
				if tt.actual != "" && (result.Summary["protocol"] != tt.actual || result.Summary["requested_protocol"] != "auto") {
					t.Fatalf("unexpected protocol summary: %#v", result.Summary)
				}
			})
		}
	}
}

func TestHTTPAutoRunsOnlyAuthorizedVersions(t *testing.T) {
	original := executeHTTPProbeAttempt
	t.Cleanup(func() { executeHTTPProbeAttempt = original })
	for _, tt := range []struct {
		name, target, versions, failedVersion, wantAttempts string
		wantErr                                             bool
	}{
		{"skip HTTP3", "https://example.com", "2,1.1", "2", "2,1.1", false},
		{"HTTP2 only", "https://example.com", "2", "", "2", false},
		{"no unauthorized fallback", "https://example.com", "2", "2", "2", true},
		{"HTTP11 only", "http://example.com", "1.1", "", "1.1", false},
		{"plain HTTP cannot use allowed HTTPS versions", "http://example.com", "3,2", "", "", true},
		{"empty candidates", "https://example.com", "", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts []string
			executeHTTPProbeAttempt = func(_ context.Context, job model.Job, _ string, version string, _ time.Duration) (*model.ToolResult, bool, error) {
				attempts = append(attempts, version)
				result := newBuiltinResult(job, 0)
				if version == tt.failedVersion {
					return result, false, errors.New("version unavailable")
				}
				return result, true, nil
			}
			_, err := runBuiltinHTTP(context.Background(), model.Job{
				Tool: model.ToolHTTP, Target: tt.target,
				Args: map[string]string{"http_version": "auto", policy.AutoHTTPVersionsArg: tt.versions},
			}, nil)
			if (err != nil) != tt.wantErr || strings.Join(attempts, ",") != tt.wantAttempts {
				t.Fatalf("attempts=%v err=%v, want attempts=%q wantErr=%t", attempts, err, tt.wantAttempts, tt.wantErr)
			}
		})
	}
}
