package policy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func TestValidateRejectsShellCharacters(t *testing.T) {
	_, err := DefaultPolicies().Validate(model.CreateJobRequest{Tool: model.ToolPing, Target: "1.1.1.1;rm -rf /"})
	if err == nil {
		t.Fatal("expected invalid target")
	}
}

func TestValidateResolvedTargetRejectsPrivateAndMulticast(t *testing.T) {
	tests := []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"224.0.0.1",
		"100.64.0.1",
		"::1",
		"fc00::1",
		"ff02::1",
	}
	for _, target := range tests {
		if err := ValidateResolvedTarget(context.Background(), model.ToolPing, target, model.IPAny); err == nil {
			t.Fatalf("expected %s to be blocked", target)
		}
	}
}

func TestValidateResolvedTargetAllowsPublicIP(t *testing.T) {
	if err := ValidateResolvedTarget(context.Background(), model.ToolPing, "1.1.1.1", model.IPAny); err != nil {
		t.Fatalf("expected public IP to be allowed: %v", err)
	}
}

func TestValidateResolvedTargetHonorsIPVersion(t *testing.T) {
	if err := ValidateResolvedTarget(context.Background(), model.ToolPing, "1.1.1.1", model.IPv6); err == nil {
		t.Fatal("expected IPv4 literal to fail IPv6 request")
	}
	if err := ValidateResolvedTarget(context.Background(), model.ToolPing, "2606:4700:4700::1111", model.IPv4); err == nil {
		t.Fatal("expected IPv6 literal to fail IPv4 request")
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

func TestValidateRejectsUnknownArgument(t *testing.T) {
	_, err := DefaultPolicies().Validate(model.CreateJobRequest{
		Tool:   model.ToolPing,
		Target: "1.1.1.1",
		Args:   map[string]string{"payload": "bad"},
	})
	if err == nil {
		t.Fatal("expected unknown argument rejection")
	}
}

func TestValidateRequiresAgentIDForPathTools(t *testing.T) {
	for _, tool := range []model.Tool{model.ToolTraceroute, model.ToolMTR} {
		_, err := DefaultPolicies().Validate(model.CreateJobRequest{
			Tool:   tool,
			Target: "1.1.1.1",
		})
		if err == nil {
			t.Fatalf("expected %s without agent_id to be rejected", tool)
		}
		_, err = DefaultPolicies().Validate(model.CreateJobRequest{
			Tool:    tool,
			Target:  "1.1.1.1",
			AgentID: "edge-1",
		})
		if err != nil {
			t.Fatalf("expected %s with agent_id to be allowed: %v", tool, err)
		}
	}
}

func TestHTTPToolUsesPolicy(t *testing.T) {
	_, err := DefaultPolicies().Validate(model.CreateJobRequest{Tool: model.ToolHTTP, Target: "https://example.com"})
	if err != nil {
		t.Fatalf("expected valid request: %v", err)
	}
}

func TestPortToolRequiresPort(t *testing.T) {
	_, err := DefaultPolicies().Validate(model.CreateJobRequest{Tool: model.ToolPort, Target: "1.1.1.1"})
	if err == nil {
		t.Fatal("expected missing port to be rejected")
	}
	_, err = DefaultPolicies().Validate(model.CreateJobRequest{
		Tool:   model.ToolPort,
		Target: "1.1.1.1",
		Args:   map[string]string{"port": "443"},
	})
	if err != nil {
		t.Fatalf("expected valid port probe: %v", err)
	}
	_, err = DefaultPolicies().Validate(model.CreateJobRequest{
		Tool:   model.ToolPort,
		Target: "1.1.1.1",
		Args:   map[string]string{"port": "70000"},
	})
	if err == nil {
		t.Fatal("expected out-of-range port to be rejected")
	}
}

func TestTimeoutForJobUsesServerPresetFormula(t *testing.T) {
	tests := []struct {
		name string
		job  model.Job
		want time.Duration
	}{
		{
			name: "ping count",
			job:  model.Job{Tool: model.ToolPing, Args: map[string]string{"count": "30"}},
			want: 19 * time.Second,
		},
		{
			name: "traceroute max hops",
			job:  model.Job{Tool: model.ToolTraceroute, Args: map[string]string{"max_hops": "20"}},
			want: 30 * time.Second,
		},
		{
			name: "mtr capped",
			job:  model.Job{Tool: model.ToolMTR, Args: map[string]string{"count": "100", "max_hops": "30"}},
			want: 5 * time.Minute,
		},
		{
			name: "http preset",
			job:  model.Job{Tool: model.ToolHTTP},
			want: 5 * time.Second,
		},
		{
			name: "dns preset",
			job:  model.Job{Tool: model.ToolDNS},
			want: 5 * time.Second,
		},
		{
			name: "port preset",
			job:  model.Job{Tool: model.ToolPort},
			want: time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TimeoutForJob(tt.job); got != tt.want {
				t.Fatalf("timeout = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestServerArgsOwnsCountAndMaxHops(t *testing.T) {
	args := ServerArgs(model.ToolMTR, map[string]string{
		"count":    "99",
		"max_hops": "3",
		"protocol": "tcp",
	})
	if args["count"] != "10" || args["max_hops"] != "30" {
		t.Fatalf("server args did not enforce fixed values: %#v", args)
	}
	if args["protocol"] != "tcp" {
		t.Fatalf("server args dropped dynamic protocol: %#v", args)
	}
}

func TestRuntimeConfigControlsServerOwnedArgsAndTimeouts(t *testing.T) {
	runtime := config.DefaultRuntime()
	runtime.Count = 2
	runtime.MaxHops = 4
	runtime.ProbeStepTimeoutSec = 5
	runtime.MaxToolTimeoutSec = 120

	policies := PoliciesWithRuntime(runtime)
	args := policies.ServerArgs(model.ToolMTR, map[string]string{
		"count":    "99",
		"max_hops": "99",
		"protocol": "icmp",
	})
	if args["count"] != "2" || args["max_hops"] != "4" {
		t.Fatalf("runtime args not applied: %#v", args)
	}
	if args["protocol"] != "icmp" {
		t.Fatalf("runtime args dropped allowed arg: %#v", args)
	}
	if got := policies.TimeoutForJob(model.Job{Tool: model.ToolMTR}); got != 40*time.Second {
		t.Fatalf("runtime timeout = %s, want 40s", got)
	}
	if p, ok := policies.Get(model.ToolMTR); !ok || p.ProbeTimeout != 5*time.Second {
		t.Fatalf("runtime probe timeout = %#v ok=%v, want 5s", p.ProbeTimeout, ok)
	}
}

func TestUDPProtocolIsRejected(t *testing.T) {
	_, err := DefaultPolicies().Validate(model.CreateJobRequest{
		Tool:   model.ToolPing,
		Target: "1.1.1.1",
		Args:   map[string]string{"protocol": "udp"},
	})
	if err == nil {
		t.Fatal("expected udp protocol to be rejected")
	}
}
