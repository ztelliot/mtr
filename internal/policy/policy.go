package policy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

var hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,253}$`)

type Policy struct {
	Tool          model.Tool
	Timeout       time.Duration
	ProbeTimeout  time.Duration
	AllowedArgs   map[string]string
	HideFirstHops int
}

type Set struct {
	byTool  map[model.Tool]Policy
	runtime config.Runtime
}

func FromConfig(cfg map[string]config.Policy, runtime config.Runtime) Set {
	out := PoliciesWithRuntime(runtime)
	for name, p := range cfg {
		if !p.Enabled {
			delete(out.byTool, model.Tool(name))
			continue
		}
		t := model.Tool(name)
		existing := out.byTool[t]
		if p.AllowedArgs != nil {
			existing.AllowedArgs = p.AllowedArgs
		}
		if p.HideFirstHops > 0 {
			existing.HideFirstHops = p.HideFirstHops
		}
		out.byTool[t] = existing
	}
	return out
}

func DefaultPolicies() Set {
	return PoliciesWithRuntime(config.DefaultRuntime())
}

func PoliciesWithRuntime(runtime config.Runtime) Set {
	runtime = normalizeRuntime(runtime)
	probeTimeout := time.Duration(runtime.ProbeStepTimeoutSec) * time.Second
	return Set{runtime: runtime, byTool: map[model.Tool]Policy{
		model.ToolPing:       {Tool: model.ToolPing, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolPing}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": `^(icmp|tcp)$`, "port": `^[1-9][0-9]{0,4}$`}},
		model.ToolTraceroute: {Tool: model.ToolTraceroute, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolTraceroute}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": `^(icmp|tcp)$`, "port": `^[1-9][0-9]{0,4}$`}},
		model.ToolMTR:        {Tool: model.ToolMTR, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolMTR}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": `^(icmp|tcp)$`, "port": `^[1-9][0-9]{0,4}$`}},
		model.ToolHTTP:       {Tool: model.ToolHTTP, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolHTTP}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"method": `^(GET|HEAD)$`}},
		model.ToolDNS:        {Tool: model.ToolDNS, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolDNS}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"type": `^(A|AAAA|CNAME|MX|TXT|NS)$`}},
		model.ToolPort:       {Tool: model.ToolPort, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolPort}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"port": `^[1-9][0-9]{0,4}$`}},
	}}
}

func normalizeRuntime(runtime config.Runtime) config.Runtime {
	defaults := config.DefaultRuntime()
	if runtime.Count <= 0 {
		runtime.Count = defaults.Count
	}
	if runtime.MaxHops <= 0 {
		runtime.MaxHops = defaults.MaxHops
	}
	if runtime.ProbeStepTimeoutSec <= 0 {
		runtime.ProbeStepTimeoutSec = defaults.ProbeStepTimeoutSec
	}
	if runtime.MaxToolTimeoutSec <= 0 {
		runtime.MaxToolTimeoutSec = defaults.MaxToolTimeoutSec
	}
	if runtime.HTTPTimeoutSec <= 0 {
		runtime.HTTPTimeoutSec = defaults.HTTPTimeoutSec
	}
	if runtime.DNSTimeoutSec <= 0 {
		runtime.DNSTimeoutSec = defaults.DNSTimeoutSec
	}
	if runtime.ResolveTimeoutSec <= 0 {
		runtime.ResolveTimeoutSec = defaults.ResolveTimeoutSec
	}
	if runtime.OutboundInvokeAttempts <= 0 {
		runtime.OutboundInvokeAttempts = defaults.OutboundInvokeAttempts
	}
	if runtime.OutboundMaxHealthIntervalSec <= 0 {
		runtime.OutboundMaxHealthIntervalSec = defaults.OutboundMaxHealthIntervalSec
	}
	return runtime
}

func TimeoutForJob(job model.Job) time.Duration {
	return TimeoutForJobWithRuntime(job, config.DefaultRuntime())
}

func TimeoutForJobWithRuntime(job model.Job, runtime config.Runtime) time.Duration {
	runtime = normalizeRuntime(runtime)
	probeStepTimeout := time.Duration(runtime.ProbeStepTimeoutSec) * time.Second
	var timeout time.Duration
	switch job.Tool {
	case model.ToolPing:
		timeout = time.Duration(runtime.Count)*probeStepTimeout + time.Duration(runtime.Count-1)*time.Second
	case model.ToolTraceroute:
		timeout = time.Duration(runtime.MaxHops) * probeStepTimeout
	case model.ToolMTR:
		timeout = time.Duration(runtime.Count*runtime.MaxHops) * probeStepTimeout
	case model.ToolHTTP:
		timeout = time.Duration(runtime.HTTPTimeoutSec) * time.Second
	case model.ToolDNS:
		timeout = time.Duration(runtime.DNSTimeoutSec) * time.Second
	case model.ToolPort:
		timeout = probeStepTimeout
	default:
		timeout = probeStepTimeout
	}
	if timeout <= 0 {
		timeout = probeStepTimeout
	}
	maxToolTimeout := time.Duration(runtime.MaxToolTimeoutSec) * time.Second
	if timeout > maxToolTimeout {
		return maxToolTimeout
	}
	return timeout
}

func ServerArgs(tool model.Tool, args map[string]string) map[string]string {
	return ServerArgsWithRuntime(tool, args, config.DefaultRuntime())
}

func ServerArgsWithRuntime(tool model.Tool, args map[string]string, runtime config.Runtime) map[string]string {
	runtime = normalizeRuntime(runtime)
	out := make(map[string]string, len(args)+2)
	for k, v := range args {
		if k == "count" || k == "max_hops" || k == "max_hop" {
			continue
		}
		out[k] = v
	}
	switch tool {
	case model.ToolPing:
		out["count"] = strconv.Itoa(runtime.Count)
	case model.ToolTraceroute:
		out["max_hops"] = strconv.Itoa(runtime.MaxHops)
	case model.ToolMTR:
		out["count"] = strconv.Itoa(runtime.Count)
		out["max_hops"] = strconv.Itoa(runtime.MaxHops)
	}
	return out
}

func AgentArgs(tool model.Tool, args map[string]string) map[string]string {
	return AgentArgsWithRuntime(tool, args, config.DefaultRuntime())
}

func AgentArgsWithRuntime(tool model.Tool, args map[string]string, runtime config.Runtime) map[string]string {
	runtime = normalizeRuntime(runtime)
	out := make(map[string]string, len(args)+2)
	for k, v := range args {
		out[k] = v
	}
	switch tool {
	case model.ToolPing:
		if out["count"] == "" {
			out["count"] = strconv.Itoa(runtime.Count)
		}
	case model.ToolTraceroute:
		if out["max_hops"] == "" && out["max_hop"] == "" {
			out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		}
	case model.ToolMTR:
		if out["count"] == "" {
			out["count"] = strconv.Itoa(runtime.Count)
		}
		if out["max_hops"] == "" && out["max_hop"] == "" {
			out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		}
	}
	return out
}

func (s Set) ServerArgs(tool model.Tool, args map[string]string) map[string]string {
	return ServerArgsWithRuntime(tool, args, s.runtime)
}

func (s Set) AgentArgs(tool model.Tool, args map[string]string) map[string]string {
	return AgentArgsWithRuntime(tool, args, s.runtime)
}

func (s Set) TimeoutForJob(job model.Job) time.Duration {
	return TimeoutForJobWithRuntime(job, s.runtime)
}

func (s Set) ProbeTimeout() time.Duration {
	return time.Duration(normalizeRuntime(s.runtime).ProbeStepTimeoutSec) * time.Second
}

func (s Set) MaxToolTimeout() time.Duration {
	return time.Duration(normalizeRuntime(s.runtime).MaxToolTimeoutSec) * time.Second
}

func (s Set) ResolveTimeoutSeconds() int {
	return normalizeRuntime(s.runtime).ResolveTimeoutSec
}

func (s Set) ValidateResolvedTarget(ctx context.Context, tool model.Tool, target string, version model.IPVersion) error {
	_, _, err := s.ResolveTarget(ctx, tool, target, version)
	return err
}

func (s Set) ResolveTarget(ctx context.Context, tool model.Tool, target string, version model.IPVersion) (string, model.IPVersion, error) {
	return ResolveTargetWithRuntime(ctx, tool, target, version, s.runtime)
}

func (s Set) Get(tool model.Tool) (Policy, bool) {
	p, ok := s.byTool[tool]
	return p, ok
}

func (s Set) Validate(req model.CreateJobRequest) (Policy, error) {
	req.Args = s.ServerArgs(req.Tool, req.Args)
	return s.validate(req, true)
}

func (s Set) ValidateSchedule(req model.CreateJobRequest) (Policy, error) {
	req.Args = s.ServerArgs(req.Tool, req.Args)
	return s.validate(req, false)
}

func (s Set) ValidateTrusted(req model.CreateJobRequest) (Policy, error) {
	req.Args = s.AgentArgs(req.Tool, req.Args)
	return s.validate(req, true)
}

func (s Set) validate(req model.CreateJobRequest, requirePinned bool) (Policy, error) {
	p, ok := s.Get(req.Tool)
	if !ok {
		return Policy{}, fmt.Errorf("tool %q is not enabled", req.Tool)
	}
	if err := ValidateTarget(req.Tool, req.Target); err != nil {
		return Policy{}, err
	}
	if requirePinned && requiresPinnedAgent(req.Tool) && strings.TrimSpace(req.AgentID) == "" {
		return Policy{}, fmt.Errorf("%s requires agent_id", req.Tool)
	}
	if requiresPortArg(req.Tool) && strings.TrimSpace(req.Args["port"]) == "" {
		return Policy{}, fmt.Errorf("%s requires port", req.Tool)
	}
	for k, v := range req.Args {
		if isServerOwnedArg(k) {
			continue
		}
		pattern, ok := p.AllowedArgs[k]
		if !ok {
			return Policy{}, fmt.Errorf("argument %q is not allowed for %s", k, req.Tool)
		}
		if pattern != "" && !regexp.MustCompile(pattern).MatchString(v) {
			return Policy{}, fmt.Errorf("argument %q is invalid", k)
		}
		if k == "port" {
			port, err := strconv.Atoi(v)
			if err != nil || port <= 0 || port > 65535 {
				return Policy{}, fmt.Errorf("argument %q is invalid", k)
			}
		}
	}
	return p, nil
}

func isServerOwnedArg(key string) bool {
	return key == "count" || key == "max_hops" || key == "max_hop"
}

func requiresPinnedAgent(tool model.Tool) bool {
	return tool == model.ToolTraceroute || tool == model.ToolMTR
}

func requiresPortArg(tool model.Tool) bool {
	return tool == model.ToolPort
}

func ValidateResolvedTarget(ctx context.Context, tool model.Tool, target string, version model.IPVersion) error {
	_, _, err := ResolveTarget(ctx, tool, target, version)
	return err
}

func ValidateResolvedTargetWithRuntime(ctx context.Context, tool model.Tool, target string, version model.IPVersion, runtime config.Runtime) error {
	_, _, err := ResolveTargetWithRuntime(ctx, tool, target, version, runtime)
	return err
}

func ResolveTarget(ctx context.Context, tool model.Tool, target string, version model.IPVersion) (string, model.IPVersion, error) {
	return ResolveTargetWithRuntime(ctx, tool, target, version, config.DefaultRuntime())
}

func ResolveTargetWithRuntime(ctx context.Context, tool model.Tool, target string, version model.IPVersion, runtime config.Runtime) (string, model.IPVersion, error) {
	runtime = normalizeRuntime(runtime)
	host, err := targetHost(tool, target)
	if err != nil {
		return "", model.IPAny, err
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := validateIPVersion(ip, version); err != nil {
			return "", model.IPAny, err
		}
		if err := validateAllowedIP(ip.String()); err != nil {
			return "", model.IPAny, err
		}
		return ip.String(), ipVersion(ip), nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(runtime.ResolveTimeoutSec)*time.Second)
	defer cancel()
	network := "ip"
	switch version {
	case model.IPv4:
		network = "ip4"
	case model.IPv6:
		network = "ip6"
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, network, host)
	if err != nil {
		return "", model.IPAny, fmt.Errorf("resolve target: %w", err)
	}
	if len(ips) == 0 {
		return "", model.IPAny, errors.New("target resolved to no addresses")
	}
	validIPs := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if err := validateIPVersion(ip, version); err != nil {
			return "", model.IPAny, err
		}
		if err := validateAllowedIP(ip.String()); err != nil {
			return "", model.IPAny, err
		}
		validIPs = append(validIPs, ip)
	}
	first := preferredIP(validIPs, version)
	if first == nil {
		return "", model.IPAny, errors.New("target resolved to no addresses")
	}
	return first.String(), ipVersion(first), nil
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

func ValidateLiteralTarget(ctx context.Context, tool model.Tool, target string, version model.IPVersion) error {
	_ = ctx
	host, err := targetHost(tool, target)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := validateIPVersion(ip, version); err != nil {
			return err
		}
		return validateAllowedIP(ip.String())
	}
	return nil
}

func LiteralIPVersion(tool model.Tool, target string) (model.IPVersion, bool, error) {
	host, err := targetHost(tool, target)
	if err != nil {
		return model.IPAny, false, err
	}
	if ip := net.ParseIP(host); ip != nil {
		return ipVersion(ip), true, nil
	}
	return model.IPAny, false, nil
}

func ValidateResolvedIP(raw string, version model.IPVersion) error {
	ip := net.ParseIP(raw)
	if ip == nil {
		return fmt.Errorf("invalid resolved ip %q", raw)
	}
	if err := validateIPVersion(ip, version); err != nil {
		return err
	}
	return validateAllowedIP(ip.String())
}

func ipVersion(ip net.IP) model.IPVersion {
	if ip.To4() != nil {
		return model.IPv4
	}
	return model.IPv6
}

func RequiredProtocol(version model.IPVersion) model.ProtocolMask {
	switch version {
	case model.IPv4:
		return model.ProtocolIPv4
	case model.IPv6:
		return model.ProtocolIPv6
	default:
		return 0
	}
}

func AgentSupports(a model.Agent, tool model.Tool, version model.IPVersion) bool {
	if a.Status != model.AgentOnline {
		return false
	}
	hasTool := false
	for _, cap := range a.Capabilities {
		if cap == tool {
			hasTool = true
			break
		}
	}
	if !hasTool {
		return false
	}
	protocols := a.Protocols
	if protocols == 0 {
		protocols = model.ProtocolAll
	}
	required := RequiredProtocol(version)
	return required == 0 || protocols&required != 0
}

func ValidateTarget(tool model.Tool, target string) error {
	if target == "" || len(target) > 512 {
		return errors.New("target is required")
	}
	if tool == model.ToolHTTP {
		u, err := url.Parse(target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("http target must be an http or https URL")
		}
		return nil
	}
	host := target
	if strings.Contains(target, "://") || strings.ContainsAny(target, " \t\r\n;&|`$<>") {
		return errors.New("target contains forbidden characters")
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if !hostnameRE.MatchString(host) || strings.Contains(host, "..") {
		return errors.New("target must be an IP address or hostname")
	}
	return nil
}

func targetHost(tool model.Tool, target string) (string, error) {
	if tool == model.ToolHTTP {
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return "", errors.New("http target must be an http or https URL")
		}
		return u.Hostname(), nil
	}
	return target, nil
}

func validateAllowedIP(raw string) error {
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return fmt.Errorf("invalid resolved ip %q", raw)
	}
	if addr.Is4In6() {
		return validateAllowedIP(addr.Unmap().String())
	}
	if addr.IsUnspecified() ||
		addr.IsLoopback() ||
		addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() ||
		addr.IsMulticast() ||
		!addr.IsGlobalUnicast() ||
		isBlockedSpecialUse(addr) {
		return fmt.Errorf("target address %s is not allowed", addr)
	}
	return nil
}

func validateIPVersion(ip net.IP, version model.IPVersion) error {
	switch version {
	case model.IPv4:
		if ip.To4() == nil {
			return fmt.Errorf("target address %s is not IPv4", ip)
		}
	case model.IPv6:
		if ip.To4() != nil {
			return fmt.Errorf("target address %s is not IPv6", ip)
		}
	}
	return nil
}

func isBlockedSpecialUse(addr netip.Addr) bool {
	blocked := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
		netip.MustParsePrefix("ff00::/8"),
	}
	for _, prefix := range blocked {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
