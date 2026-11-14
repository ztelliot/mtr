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

const DefaultPortRange = "1-65535"

const (
	DefaultTCPProbePort = "80"
	DefaultUDPProbePort = "33434"
	AutoProtocolsArg    = "auto_protocols"
	AutoHTTPVersionsArg = "auto_http_versions"
)

var (
	ErrInvalidPortRangeRule = errors.New("invalid port range rule")
	ErrPortNotAllowed       = errors.New("port is not allowed")
)

type Policy struct {
	Tool         model.Tool
	Timeout      time.Duration
	ProbeTimeout time.Duration
	AllowedArgs  map[string]string
}

type Set struct {
	byTool  map[model.Tool]Policy
	runtime config.Runtime
}

func FromConfig(cfg map[string]config.Policy, runtime config.Runtime) Set {
	return PoliciesWithRuntime(runtime).WithIntersection(cfg, runtime)
}

func DefaultPolicies() Set {
	return PoliciesWithRuntime(config.DefaultRuntime())
}

func PoliciesWithRuntime(runtime config.Runtime) Set {
	runtime = normalizeRuntime(runtime)
	probeTimeout := time.Duration(runtime.ProbeStepTimeoutSec) * time.Second
	return Set{runtime: runtime, byTool: map[model.Tool]Policy{
		model.ToolPing:       {Tool: model.ToolPing, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolPing}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": "icmp,udp,tcp", "port": DefaultPortRange}},
		model.ToolTraceroute: {Tool: model.ToolTraceroute, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolTraceroute}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": "icmp,udp,tcp", "port": DefaultPortRange}},
		model.ToolMTR:        {Tool: model.ToolMTR, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolMTR}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"protocol": "icmp,udp,tcp", "port": DefaultPortRange}},
		model.ToolHTTP:       {Tool: model.ToolHTTP, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolHTTP}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"method": "GET,HEAD", "http_version": "3,2,1.1"}},
		model.ToolDNS:        {Tool: model.ToolDNS, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolDNS}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"type": "A,AAAA,CNAME,MX,TXT,NS"}},
		model.ToolPort:       {Tool: model.ToolPort, Timeout: TimeoutForJobWithRuntime(model.Job{Tool: model.ToolPort}, runtime), ProbeTimeout: probeTimeout, AllowedArgs: map[string]string{"port": DefaultPortRange}},
	}}
}

func (s Set) WithIntersection(cfg map[string]config.Policy, runtime config.Runtime) Set {
	base := PoliciesWithRuntime(runtime)
	out := Set{runtime: base.runtime, byTool: make(map[model.Tool]Policy, len(base.byTool))}
	for tool, p := range base.byTool {
		if existing, ok := s.byTool[tool]; ok {
			p.AllowedArgs = cloneStringMap(existing.AllowedArgs)
			out.byTool[tool] = p
		}
	}
	for name, p := range cfg {
		t := model.Tool(name)
		existing, ok := out.byTool[t]
		if !p.Enabled {
			if _, known := base.byTool[t]; known {
				delete(out.byTool, t)
			}
			continue
		}
		if !ok {
			continue
		}
		if p.AllowedArgs != nil {
			existing.AllowedArgs = IntersectAllowedArgs(existing.AllowedArgs, p.AllowedArgs)
		}
		out.byTool[t] = existing
	}
	return out
}

func IntersectAllowedArgs(base map[string]string, limit map[string]string) map[string]string {
	if base == nil {
		return cloneStringMap(limit)
	}
	if limit == nil {
		return cloneStringMap(base)
	}
	out := map[string]string{}
	for key, baseRule := range base {
		limitRule, ok := limit[key]
		if !ok {
			continue
		}
		if key == "port" {
			rule, ok := IntersectPortRules(baseRule, limitRule)
			if ok {
				out[key] = rule
			}
			continue
		}
		values := intersectCSVValues(baseRule, limitRule)
		if len(values) > 0 {
			out[key] = strings.Join(values, ",")
		}
	}
	return out
}

func IntersectPortRules(left string, right string) (string, bool) {
	leftRanges, err := parsePortRanges(left)
	if err != nil {
		return "", false
	}
	rightRanges, err := parsePortRanges(right)
	if err != nil {
		return "", false
	}
	var out [][2]int
	for _, a := range leftRanges {
		for _, b := range rightRanges {
			start := max(a[0], b[0])
			end := min(a[1], b[1])
			if start <= end {
				out = append(out, [2]int{start, end})
			}
		}
	}
	if len(out) == 0 {
		return "", false
	}
	return formatPortRanges(out), true
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
	probeStepTimeout := saturatingDurationProduct(time.Second, runtime.ProbeStepTimeoutSec)
	var timeout time.Duration
	switch job.Tool {
	case model.ToolPing:
		timeout = saturatingDurationAdd(
			saturatingDurationProduct(probeStepTimeout, runtime.Count),
			saturatingDurationProduct(time.Second, runtime.Count-1),
		)
	case model.ToolTraceroute:
		timeout = saturatingDurationProduct(probeStepTimeout, runtime.MaxHops)
	case model.ToolMTR:
		timeout = saturatingDurationProduct(probeStepTimeout, runtime.Count, runtime.MaxHops)
	case model.ToolHTTP:
		timeout = saturatingDurationProduct(time.Second, runtime.HTTPTimeoutSec)
	case model.ToolDNS:
		timeout = saturatingDurationProduct(time.Second, runtime.DNSTimeoutSec)
	case model.ToolPort:
		timeout = probeStepTimeout
	default:
		timeout = probeStepTimeout
	}
	if timeout <= 0 {
		timeout = probeStepTimeout
	}
	maxToolTimeout := saturatingDurationProduct(time.Second, runtime.MaxToolTimeoutSec)
	if timeout > maxToolTimeout {
		return maxToolTimeout
	}
	return timeout
}

func saturatingDurationProduct(unit time.Duration, factors ...int) time.Duration {
	if unit <= 0 {
		return 0
	}
	const maximum = time.Duration(1<<63 - 1)
	result := unit
	for _, factor := range factors {
		if factor <= 0 {
			return 0
		}
		if result > maximum/time.Duration(factor) {
			return maximum
		}
		result *= time.Duration(factor)
	}
	return result
}

func saturatingDurationAdd(left, right time.Duration) time.Duration {
	const maximum = time.Duration(1<<63 - 1)
	if left >= maximum-right {
		return maximum
	}
	return left + right
}

func ServerArgs(tool model.Tool, args map[string]string) map[string]string {
	return ServerArgsWithRuntime(tool, args, config.DefaultRuntime())
}

func ServerArgsWithRuntime(tool model.Tool, args map[string]string, runtime config.Runtime) map[string]string {
	runtime = normalizeRuntime(runtime)
	out := make(map[string]string, len(args)+2)
	for k, v := range args {
		if k == "count" || k == "max_hops" {
			continue
		}
		out[k] = v
	}
	switch tool {
	case model.ToolPing:
		out["count"] = strconv.Itoa(runtime.Count)
		materializeRouteDefaults(out)
	case model.ToolTraceroute:
		out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		materializeRouteDefaults(out)
	case model.ToolMTR:
		out["count"] = strconv.Itoa(runtime.Count)
		out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		materializeRouteDefaults(out)
	case model.ToolHTTP:
		materializeHTTPDefaults(out)
	case model.ToolDNS:
		materializeDNSDefaults(out)
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
		materializeRouteDefaults(out)
	case model.ToolTraceroute:
		if out["max_hops"] == "" {
			out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		}
		materializeRouteDefaults(out)
	case model.ToolMTR:
		if out["count"] == "" {
			out["count"] = strconv.Itoa(runtime.Count)
		}
		if out["max_hops"] == "" {
			out["max_hops"] = strconv.Itoa(runtime.MaxHops)
		}
		materializeRouteDefaults(out)
	case model.ToolHTTP:
		materializeHTTPDefaults(out)
	case model.ToolDNS:
		materializeDNSDefaults(out)
	}
	return out
}

func materializeRouteDefaults(args map[string]string) {
	if strings.TrimSpace(args["protocol"]) == "" {
		args["protocol"] = "icmp"
	}
}

func materializeHTTPDefaults(args map[string]string) {
	if strings.TrimSpace(args["method"]) == "" {
		args["method"] = "GET"
	}
	if strings.TrimSpace(args["http_version"]) == "" {
		args["http_version"] = "1.1"
	}
}

func materializeDNSDefaults(args map[string]string) {
	if strings.TrimSpace(args["type"]) == "" {
		args["type"] = "A"
	}
}

func (s Set) ServerArgs(tool model.Tool, args map[string]string) map[string]string {
	out := ServerArgsWithRuntime(tool, args, s.runtime)
	p, _ := s.Get(tool)
	return ConstrainAutoArgs(tool, out, p.AllowedArgs)
}

func (s Set) AgentArgs(tool model.Tool, args map[string]string) map[string]string {
	out := AgentArgsWithRuntime(tool, args, s.runtime)
	p, _ := s.Get(tool)
	return ConstrainAutoArgs(tool, out, p.AllowedArgs)
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

func (s Set) Runtime() config.Runtime {
	return normalizeRuntime(s.runtime)
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
	if err := ValidateAllowedArgs(req.Tool, req.Args, p.AllowedArgs); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// EffectiveArgs returns the complete arguments a probe will use. Semantic
// defaults are materialized before authorization and dispatch.
func EffectiveArgs(tool model.Tool, args map[string]string) map[string]string {
	out := make(map[string]string, len(args)+2)
	for key, value := range args {
		if isServerOwnedArg(key) {
			continue
		}
		out[key] = value
	}
	switch tool {
	case model.ToolPing, model.ToolTraceroute, model.ToolMTR:
		if strings.TrimSpace(out["protocol"]) == "" {
			out["protocol"] = "icmp"
		}
		if strings.TrimSpace(out["port"]) == "" {
			switch out["protocol"] {
			case "tcp":
				out["port"] = DefaultTCPProbePort
			case "udp":
				out["port"] = DefaultUDPProbePort
			}
		}
	case model.ToolHTTP:
		if strings.TrimSpace(out["method"]) == "" {
			out["method"] = "GET"
		}
		if strings.TrimSpace(out["http_version"]) == "" {
			out["http_version"] = "1.1"
		}
	case model.ToolDNS:
		if strings.TrimSpace(out["type"]) == "" {
			out["type"] = "A"
		}
	}
	return out
}

// ValidateAllowedArgs validates both submitted arguments and semantic
// defaults. Auto probes select only authorized concrete candidates.
func ValidateAllowedArgs(tool model.Tool, args map[string]string, allowedArgs map[string]string) error {
	effective := EffectiveArgs(tool, args)
	for key, value := range effective {
		pattern, ok := allowedArgs[key]
		if !ok {
			return fmt.Errorf("argument %q is not allowed for %s", key, tool)
		}
		if key == "port" {
			if err := ValidatePortAllowed(pattern, value); err != nil {
				return fmt.Errorf("argument %q is invalid", key)
			}
			continue
		}
		if !AllowedArgPermits(tool, key, pattern, value) {
			return fmt.Errorf("argument %q is invalid", key)
		}
	}

	if isNetworkProbeTool(tool) && effective["protocol"] == "auto" && len(AutoProbeProtocols(args, allowedArgs)) == 0 {
		return fmt.Errorf("no allowed protocols for %s auto probe", tool)
	}
	if tool == model.ToolHTTP && effective["http_version"] == "auto" && len(AutoHTTPVersions(args, allowedArgs)) == 0 {
		return fmt.Errorf("no allowed HTTP versions for auto probe")
	}
	return nil
}

// AllowedArgPermits treats auto as selection among allowed concrete values,
// rather than a separately configurable permission.
func AllowedArgPermits(tool model.Tool, key string, rule string, value string) bool {
	if isNetworkProbeTool(tool) && key == "protocol" && value == "auto" {
		return AllowedArgContains(rule, "icmp") || AllowedArgContains(rule, "udp") || AllowedArgContains(rule, "tcp")
	}
	if tool == model.ToolHTTP && key == "http_version" && value == "auto" {
		return AllowedArgContains(rule, "3") || AllowedArgContains(rule, "2") || AllowedArgContains(rule, "1.1")
	}
	return AllowedArgContains(rule, value)
}

// AutoProbeProtocols returns the authorized fallback order, intersecting any
// previously recorded restriction with the current protocol and port policy.
func AutoProbeProtocols(args, allowedArgs map[string]string) []string {
	var out []string
	limit, limited := args[AutoProtocolsArg]
	for _, protocol := range []string{"icmp", "udp", "tcp"} {
		if !AllowedArgContains(allowedArgs["protocol"], protocol) || (limited && !AllowedArgContains(limit, protocol)) {
			continue
		}
		if protocol != "icmp" {
			port := args["port"]
			if strings.TrimSpace(port) == "" {
				port = DefaultTCPProbePort
				if protocol == "udp" {
					port = DefaultUDPProbePort
				}
			}
			if ValidatePortAllowed(allowedArgs["port"], port) != nil {
				continue
			}
		}
		out = append(out, protocol)
	}
	return out
}

// AutoHTTPVersions returns the authorized HTTP versions in fallback order.
func AutoHTTPVersions(args, allowedArgs map[string]string) []string {
	var out []string
	limit, limited := args[AutoHTTPVersionsArg]
	for _, version := range []string{"3", "2", "1.1"} {
		if AllowedArgContains(allowedArgs["http_version"], version) && (!limited || AllowedArgContains(limit, version)) {
			out = append(out, version)
		}
	}
	return out
}

// ConstrainAutoArgs records restrictions so later dispatch and execution
// cannot widen the candidates authorized when the job was created.
func ConstrainAutoArgs(tool model.Tool, args, allowedArgs map[string]string) map[string]string {
	var key string
	var candidates []string
	switch {
	case isNetworkProbeTool(tool) && args["protocol"] == "auto":
		key, candidates = AutoProtocolsArg, AutoProbeProtocols(args, allowedArgs)
	case tool == model.ToolHTTP && args["http_version"] == "auto":
		key, candidates = AutoHTTPVersionsArg, AutoHTTPVersions(args, allowedArgs)
	default:
		return args
	}
	out := cloneStringMap(args)
	out[key] = strings.Join(candidates, ",")
	return out
}

func isNetworkProbeTool(tool model.Tool) bool {
	return tool == model.ToolPing || tool == model.ToolTraceroute || tool == model.ToolMTR
}

func ValidatePortAllowed(rule string, value string) error {
	port, err := parseCanonicalPort(value)
	if err != nil {
		return ErrPortNotAllowed
	}
	ranges, err := parsePortRanges(rule)
	if err != nil {
		return err
	}
	for _, item := range ranges {
		if port >= item[0] && port <= item[1] {
			return nil
		}
	}
	return ErrPortNotAllowed
}

func ValidatePortRangeRule(rule string) error {
	_, err := parsePortRanges(rule)
	return err
}

func parsePortRanges(rule string) ([][2]int, error) {
	if rule == "" {
		return nil, ErrInvalidPortRangeRule
	}
	parts := strings.Split(rule, ",")
	ranges := make([][2]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, ErrInvalidPortRangeRule
		}
		bounds := strings.Split(part, "-")
		if len(bounds) > 2 {
			return nil, ErrInvalidPortRangeRule
		}
		start, err := parseCanonicalPort(bounds[0])
		if err != nil {
			return nil, ErrInvalidPortRangeRule
		}
		end := start
		if len(bounds) == 2 {
			end, err = parseCanonicalPort(bounds[1])
			if err != nil {
				return nil, ErrInvalidPortRangeRule
			}
		}
		if start > end {
			return nil, ErrInvalidPortRangeRule
		}
		ranges = append(ranges, [2]int{start, end})
	}
	if len(ranges) == 0 {
		return nil, ErrInvalidPortRangeRule
	}
	return ranges, nil
}

func parseCanonicalPort(value string) (int, error) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, ErrInvalidPortRangeRule
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, ErrInvalidPortRangeRule
		}
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, ErrInvalidPortRangeRule
	}
	return port, nil
}

func formatPortRanges(ranges [][2]int) string {
	parts := make([]string, 0, len(ranges))
	for _, item := range ranges {
		if item[0] == item[1] {
			parts = append(parts, strconv.Itoa(item[0]))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", item[0], item[1]))
	}
	return strings.Join(parts, ",")
}

func intersectCSVValues(left string, right string) []string {
	leftValues := csvValues(left)
	rightValues := csvValues(right)
	seen := map[string]struct{}{}
	for _, value := range rightValues {
		seen[value] = struct{}{}
	}
	var out []string
	for _, value := range leftValues {
		if _, ok := seen[value]; ok {
			out = append(out, value)
		}
	}
	return out
}

func AllowedArgContains(rule string, value string) bool {
	for _, allowed := range csvValues(rule) {
		if allowed == value {
			return true
		}
	}
	return false
}

func csvValues(rule string) []string {
	parts := strings.Split(rule, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func isServerOwnedArg(key string) bool {
	return key == "count" || key == "max_hops" || key == AutoProtocolsArg || key == AutoHTTPVersionsArg
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
	if protocols == 0 || protocols&^model.ProtocolAll != 0 {
		return false
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
	if embedded, ok := embeddedIPv4Address(addr); ok {
		if err := validateAllowedIP(embedded.String()); err != nil {
			return fmt.Errorf("target address %s embeds a disallowed IPv4 address: %w", addr, err)
		}
	}
	if !PublicNetworkAddressAllowed(addr) {
		return fmt.Errorf("target address %s is not allowed", addr)
	}
	return nil
}

// PublicNetworkAddressAllowed reports whether an address is suitable for an
// outbound request that must not reach private or special-use networks.
func PublicNetworkAddressAllowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || !addr.IsGlobalUnicast() || isBlockedSpecialUse(addr) {
		return false
	}
	if embedded, ok := embeddedIPv4Address(addr); ok {
		return PublicNetworkAddressAllowed(embedded)
	}
	return true
}

// ManagedAgentEndpointAddressAllowed reports whether an address may be used
// for a server-managed Agent endpoint. Managed Agents commonly live in a VPC
// or cluster network, so private IPv4 and IPv6 ULA addresses are permitted.
// Host-local, link-local, multicast, and cloud metadata endpoints remain
// forbidden; endpoint DNS is checked again for every connection.
func ManagedAgentEndpointAddressAllowed(addr netip.Addr) bool {
	if addr.Zone() != "" {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || !addr.IsGlobalUnicast() {
		return false
	}
	if embedded, ok := embeddedIPv4Address(addr); ok {
		return ManagedAgentEndpointAddressAllowed(embedded)
	}
	for _, metadataAddress := range [...]netip.Addr{
		netip.MustParseAddr("100.100.100.200"),
		netip.MustParseAddr("fd00:ec2::254"),
	} {
		if addr == metadataAddress {
			return false
		}
	}
	return true
}

func embeddedIPv4Address(addr netip.Addr) (netip.Addr, bool) {
	if addr.Is4In6() {
		return addr.Unmap(), true
	}
	if !addr.Is6() {
		return netip.Addr{}, false
	}
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("64:ff9b::/96"),
		netip.MustParsePrefix("64:ff9b:1::/48"),
	} {
		if !prefix.Contains(addr) {
			continue
		}
		bytes := addr.As16()
		var embedded [4]byte
		switch prefix.Bits() {
		case 48:
			embedded = [4]byte{bytes[6], bytes[7], bytes[9], bytes[10]}
		case 96:
			embedded = [4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}
		default:
			continue
		}
		return netip.AddrFrom4(embedded), true
	}
	if prefix := netip.MustParsePrefix("2002::/16"); prefix.Contains(addr) {
		bytes := addr.As16()
		return netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]}), true
	}
	return netip.Addr{}, false
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
		netip.MustParsePrefix("100::/64"),
		netip.MustParsePrefix("2001::/23"),
		netip.MustParsePrefix("2001:2::/48"),
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("2002::/16"),
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
