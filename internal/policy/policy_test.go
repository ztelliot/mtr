package policy

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func TestPublicNetworkAddressAllowedRejectsIPv6SpecialUseAndEmbeddedPrivateIPv4(t *testing.T) {
	blocked := []string{
		"100::1", "2001::1", "2001:2::1", "2001:db8::1", "2002:0a00:0001::1",
		"64:ff9b::a00:1", "64:ff9b:1:0:a00:1::",
	}
	for _, raw := range blocked {
		if PublicNetworkAddressAllowed(netip.MustParseAddr(raw)) {
			t.Fatalf("special-use address %s was allowed", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !PublicNetworkAddressAllowed(netip.MustParseAddr(raw)) {
			t.Fatalf("public address %s was blocked", raw)
		}
	}
}

func TestManagedAgentEndpointAddressAllowed(t *testing.T) {
	if ManagedAgentEndpointAddressAllowed(netip.Addr{}) {
		t.Fatal("invalid managed Agent endpoint address was allowed")
	}
	for _, raw := range []string{
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "fd12:3456:789a::1",
		"1.1.1.1", "2606:4700:4700::1111", "64:ff9b::a00:1",
	} {
		if !ManagedAgentEndpointAddressAllowed(netip.MustParseAddr(raw)) {
			t.Fatalf("managed Agent endpoint address %s was blocked", raw)
		}
	}
	for _, raw := range []string{
		"0.0.0.0", "127.0.0.1", "169.254.169.254", "224.0.0.1",
		"::", "::1", "fe80::1", "ff02::1", "100.100.100.200", "fd00:ec2::254", "fd00:ec2::254%eth0",
		"::ffff:127.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::6464:64c8",
		"2002:7f00:0001::1",
	} {
		if ManagedAgentEndpointAddressAllowed(netip.MustParseAddr(raw)) {
			t.Fatalf("forbidden managed Agent endpoint address %s was allowed", raw)
		}
	}
}

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

func TestValidateResolvedTargetRejectsPrivateIPv4EmbeddedInNAT64(t *testing.T) {
	for _, target := range []string{
		"64:ff9b::c0a8:101",
		"64:ff9b:1:c0a8:1:100::",
	} {
		if err := ValidateResolvedTarget(context.Background(), model.ToolPing, target, model.IPAny); err == nil {
			t.Fatalf("expected NAT64 target %s embedding a private IPv4 address to be blocked", target)
		}
	}
	for _, target := range []string{
		"64:ff9b::101:101",
		"64:ff9b:1:101:1:100::",
	} {
		if err := ValidateResolvedTarget(context.Background(), model.ToolPing, target, model.IPAny); err != nil {
			t.Fatalf("expected NAT64 target %s embedding a public IPv4 address to be allowed: %v", target, err)
		}
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

func TestValidateRequiresPathToolsAgentIDButScheduleDoesNot(t *testing.T) {
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
		_, err = DefaultPolicies().ValidateSchedule(model.CreateJobRequest{
			Tool:   tool,
			Target: "1.1.1.1",
		})
		if err != nil {
			t.Fatalf("expected scheduled %s without agent_id to be allowed: %v", tool, err)
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

func TestValidatePortRangeRule(t *testing.T) {
	if err := ValidatePortRangeRule("1-3000,6000-50000,50005-65535"); err != nil {
		t.Fatalf("expected port ranges to be valid: %v", err)
	}
	if err := ValidatePortAllowed("1-3000,6000-50000,50005-65535", "50004"); err == nil {
		t.Fatal("expected port outside ranges to be rejected")
	}
	if err := ValidatePortAllowed("1-3000,6000-50000,50005-65535", "50005"); err != nil {
		t.Fatalf("expected port inside ranges to be allowed: %v", err)
	}
	if err := ValidatePortRangeRule("1-3000;6000-7000"); err == nil {
		t.Fatal("expected semicolon-separated ranges to be rejected")
	}
	if err := ValidatePortRangeRule("3000-1"); err == nil {
		t.Fatal("expected reversed range to be rejected")
	}
	for _, rule := range []string{"", "80,,443", "80,", ",80", "+80", "080", "80-080", "80 -443"} {
		if err := ValidatePortRangeRule(rule); err == nil {
			t.Fatalf("expected non-canonical port rule %q to be rejected", rule)
		}
	}
	for _, value := range []string{"+80", "080", " 80", "80 "} {
		if err := ValidatePortAllowed("1-65535", value); err == nil {
			t.Fatalf("expected non-canonical port value %q to be rejected", value)
		}
	}
}

func TestPolicyIntersectionsUseMostRestrictiveArgs(t *testing.T) {
	args := IntersectAllowedArgs(
		map[string]string{"method": "GET,HEAD", "port": "1-3000,6000-50000"},
		map[string]string{"method": "HEAD", "port": "1000-7000"},
	)
	if args["method"] != "HEAD" {
		t.Fatalf("method intersection = %q, want HEAD", args["method"])
	}
	if args["port"] != "1000-3000,6000-7000" {
		t.Fatalf("port intersection = %q", args["port"])
	}
}

func TestPolicyIntersectionDoesNotInferMissingHTTPVersion(t *testing.T) {
	args := IntersectAllowedArgs(
		map[string]string{"method": "GET,HEAD", "http_version": "3,2,1.1"},
		map[string]string{"method": "HEAD"},
	)
	if args["method"] != "HEAD" {
		t.Fatalf("HTTP method intersection = %#v, want HEAD", args)
	}
	if _, ok := args["http_version"]; ok {
		t.Fatalf("missing HTTP version must remain unauthorized: %#v", args)
	}
}

func TestCurrentPoliciesSupportAllAdvertisedProtocols(t *testing.T) {
	compatible := DefaultPolicies()
	for _, request := range []model.CreateJobRequest{
		{Tool: model.ToolTraceroute, Target: "1.1.1.1", AgentID: "new", Args: map[string]string{"protocol": "udp"}},
		{Tool: model.ToolMTR, Target: "1.1.1.1", AgentID: "new", Args: map[string]string{"protocol": "auto"}},
		{Tool: model.ToolHTTP, Target: "https://example.com", AgentID: "new", Args: map[string]string{"http_version": "auto"}},
	} {
		if _, err := compatible.Validate(request); err != nil {
			t.Fatalf("negotiated request %#v rejected: %v", request, err)
		}
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

func TestTimeoutForJobWithRuntimeDoesNotOverflow(t *testing.T) {
	maximumInt := int(^uint(0) >> 1)
	runtime := config.Runtime{
		Count:               maximumInt,
		MaxHops:             maximumInt,
		ProbeStepTimeoutSec: maximumInt,
		MaxToolTimeoutSec:   maximumInt,
	}
	if got := TimeoutForJobWithRuntime(model.Job{Tool: model.ToolMTR}, runtime); got <= 0 {
		t.Fatalf("overflow produced invalid timeout: %v", got)
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

func TestNetworkToolsAllowUDPAndAutoProtocols(t *testing.T) {
	for _, tool := range []model.Tool{model.ToolPing, model.ToolTraceroute, model.ToolMTR} {
		for _, protocol := range []string{"udp", "auto"} {
			request := model.CreateJobRequest{
				Tool:   tool,
				Target: "1.1.1.1",
				Args:   map[string]string{"protocol": protocol},
			}
			if tool == model.ToolTraceroute || tool == model.ToolMTR {
				request.AgentID = "edge-1"
			}
			if _, err := DefaultPolicies().Validate(request); err != nil {
				t.Fatalf("expected %s protocol %q to be allowed: %v", tool, protocol, err)
			}
		}
	}
}

func TestNetworkProtocolDefaultsAndAutoAreValidatedAgainstPolicy(t *testing.T) {
	tests := []struct {
		name        string
		protocol    string
		allowedPort string
		wantErr     bool
	}{
		{name: "tcp default allowed", protocol: "tcp", allowedPort: DefaultTCPProbePort},
		{name: "tcp default denied", protocol: "tcp", allowedPort: "443", wantErr: true},
		{name: "udp default allowed", protocol: "udp", allowedPort: DefaultUDPProbePort},
		{name: "udp default denied", protocol: "udp", allowedPort: "53", wantErr: true},
		{name: "auto skips denied default ports", protocol: "auto", allowedPort: DefaultUDPProbePort},
		{name: "auto defaults allowed", protocol: "auto", allowedPort: DefaultTCPProbePort + "," + DefaultUDPProbePort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policies := FromConfig(map[string]config.Policy{
				string(model.ToolPing): {
					Enabled:     true,
					AllowedArgs: map[string]string{"protocol": "icmp,udp,tcp", "port": tt.allowedPort},
				},
			}, config.DefaultRuntime())
			_, err := policies.Validate(model.CreateJobRequest{
				Tool:   model.ToolPing,
				Target: "1.1.1.1",
				Args:   map[string]string{"protocol": tt.protocol},
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestAutoAllowsConcreteSubsets(t *testing.T) {
	policies := FromConfig(map[string]config.Policy{
		string(model.ToolPing): {
			Enabled:     true,
			AllowedArgs: map[string]string{"protocol": "icmp,udp", "port": DefaultPortRange},
		},
		string(model.ToolHTTP): {
			Enabled:     true,
			AllowedArgs: map[string]string{"method": "GET,HEAD", "http_version": "3,2"},
		},
	}, config.DefaultRuntime())
	if _, err := policies.Validate(model.CreateJobRequest{
		Tool:   model.ToolPing,
		Target: "1.1.1.1",
		Args:   map[string]string{"protocol": "auto", "port": "443"},
	}); err != nil {
		t.Fatalf("expected protocol auto to use the allowed subset: %v", err)
	}
	if _, err := policies.Validate(model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "HEAD", "http_version": "auto"},
	}); err != nil {
		t.Fatalf("expected HTTP auto to use the allowed subset: %v", err)
	}
}

func TestHTTPAllowsExplicitAndAutoVersions(t *testing.T) {
	for _, version := range []string{"auto", "3", "2", "1.1"} {
		if _, err := DefaultPolicies().Validate(model.CreateJobRequest{
			Tool:   model.ToolHTTP,
			Target: "https://example.com",
			Args:   map[string]string{"method": "HEAD", "http_version": version},
		}); err != nil {
			t.Fatalf("expected HTTP version %q to be allowed: %v", version, err)
		}
	}
}

func TestHTTPPolicyWithoutVersionDoesNotAuthorizeImplicitDefault(t *testing.T) {
	policies := FromConfig(map[string]config.Policy{
		string(model.ToolHTTP): {
			Enabled:     true,
			AllowedArgs: map[string]string{"method": "GET,HEAD"},
		},
	}, config.DefaultRuntime())
	p, ok := policies.Get(model.ToolHTTP)
	if !ok {
		t.Fatal("HTTP policy missing")
	}
	if _, ok := p.AllowedArgs["http_version"]; ok {
		t.Fatalf("HTTP version must not be inferred: %#v", p)
	}
	if _, err := policies.Validate(model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
	}); err == nil {
		t.Fatal("implicit HTTP/1.1 must remain unauthorized")
	}
	if _, err := policies.Validate(model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
		Args:   map[string]string{"method": "GET", "http_version": "2"},
	}); err == nil {
		t.Fatal("expected method-only policy to reject HTTP/2")
	}

	http3Only := FromConfig(map[string]config.Policy{
		string(model.ToolHTTP): {
			Enabled:     true,
			AllowedArgs: map[string]string{"method": "GET,HEAD", "http_version": "3"},
		},
	}, config.DefaultRuntime())
	if _, err := http3Only.Validate(model.CreateJobRequest{
		Tool:   model.ToolHTTP,
		Target: "https://example.com",
	}); err == nil {
		t.Fatal("expected an omitted HTTP version to be rejected by an HTTP/3-only policy")
	}
}

func TestAutoProbeCandidatesRespectProtocolPortAndPriorRestrictions(t *testing.T) {
	for _, tt := range []struct {
		name, protocols, ports, prior, want string
		limited                             bool
	}{
		{name: "TCP only", protocols: "tcp", ports: "80", want: "tcp"},
		{name: "UDP only", protocols: "udp", ports: "33434", want: "udp"},
		{name: "ICMP needs no port", protocols: "icmp", want: "icmp"},
		{name: "skip denied UDP port", protocols: "icmp,udp,tcp", ports: "80", want: "icmp,tcp"},
		{name: "no usable default", protocols: "tcp", ports: "443"},
		{name: "auto alone grants no probe", protocols: "auto", ports: DefaultPortRange},
		{name: "preserve token restriction", protocols: "icmp,udp,tcp", ports: DefaultPortRange, limited: true, prior: "udp", want: "udp"},
		{name: "empty intersection stays empty", protocols: "tcp", ports: DefaultPortRange, limited: true, prior: "udp"},
		{name: "empty candidates stay empty", protocols: "icmp,udp,tcp", ports: DefaultPortRange, limited: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]string{"protocol": "auto"}
			if tt.limited {
				args[AutoProtocolsArg] = tt.prior
			}
			allowed := map[string]string{"protocol": tt.protocols}
			if tt.ports != "" {
				allowed["port"] = tt.ports
			}
			got := ConstrainAutoArgs(model.ToolPing, args, allowed)
			if got[AutoProtocolsArg] != tt.want {
				t.Fatalf("candidates = %q, want %q", got[AutoProtocolsArg], tt.want)
			}
			err := ValidateAllowedArgs(model.ToolPing, got, allowed)
			if (err != nil) != (tt.want == "") {
				t.Fatalf("validation error = %v, candidates = %q", err, tt.want)
			}
		})
	}
}

func TestHTTPAutoCandidatesCannotWidenRestrictions(t *testing.T) {
	args := ConstrainAutoArgs(model.ToolHTTP, map[string]string{"http_version": "auto"}, map[string]string{"http_version": "2,1.1"})
	args = ConstrainAutoArgs(model.ToolHTTP, args, map[string]string{"http_version": "3,2"})
	if got := args[AutoHTTPVersionsArg]; got != "2" {
		t.Fatalf("HTTP version intersection = %q, want 2", got)
	}
	args = ConstrainAutoArgs(model.ToolHTTP, args, map[string]string{"http_version": "1.1"})
	if args[AutoHTTPVersionsArg] != "" || ValidateAllowedArgs(model.ToolHTTP, args, map[string]string{"http_version": "1.1", "method": "GET"}) == nil {
		t.Fatal("disjoint HTTP version restrictions must reject Auto")
	}
}
