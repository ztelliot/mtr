package config

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/net/http/httpguts"
	"gopkg.in/yaml.v3"
)

const (
	AgentAllLabel      = "agent"
	AgentGRPCLabel     = "agent:grpc"
	AgentHTTPLabel     = "agent:http"
	AgentEventLabel    = "agent:event"
	AgentIDLabelPrefix = "id:"
)

type Server struct {
	HTTPAddr                string   `yaml:"http_addr"`
	GRPCAddr                string   `yaml:"grpc_addr"`
	LogLevel                string   `yaml:"log_level"`
	DatabaseURL             string   `yaml:"database_url"`
	BootstrapAdminTokenFile string   `yaml:"bootstrap_admin_token_file"`
	GeoIPURL                string   `yaml:"geoip_url"`
	TrustedProxies          []string `yaml:"trusted_proxies"`
	ClientIPHeaders         []string `yaml:"client_ip_headers"`
	TLS                     TLS      `yaml:"tls"`
}

type Agent struct {
	ID             string    `yaml:"id"`
	Mode           string    `yaml:"mode"`
	LogLevel       string    `yaml:"log_level"`
	Country        string    `yaml:"country"`
	Region         string    `yaml:"region"`
	Provider       string    `yaml:"provider"`
	ISP            string    `yaml:"isp"`
	ServerAddr     string    `yaml:"server_addr"`
	RegisterToken  string    `yaml:"register_token"`
	HTTPAuth       HTTPAuth  `yaml:"http_auth"`
	HTTPAddr       string    `yaml:"http_addr"`
	HTTPPathPrefix string    `yaml:"http_path_prefix"`
	Capabilities   []string  `yaml:"capabilities"`
	Protocols      uint8     `yaml:"protocols"`
	HideFirstHops  int       `yaml:"hide_first_hops"`
	TLS            TLS       `yaml:"tls"`
	HTTPTLS        TLS       `yaml:"http_tls"`
	Speedtest      Speedtest `yaml:"speedtest"`
}

type Speedtest struct {
	DefaultBytes            int64 `yaml:"default_bytes"`
	MaxBytes                int64 `yaml:"max_bytes"`
	GlobalRequestsPerMinute int   `yaml:"global_requests_per_minute"`
	GlobalBurst             int   `yaml:"global_burst"`
	IPRequestsPerMinute     int   `yaml:"ip_requests_per_minute"`
	IPBurst                 int   `yaml:"ip_burst"`
}

const (
	HTTPAuthNone   = "none"
	HTTPAuthBearer = "bearer"
	HTTPAuthHeader = "header"
)

type HTTPAuth struct {
	Type   string `yaml:"type"`
	Header string `yaml:"header,omitempty"`
	Secret string `yaml:"secret,omitempty"`
}

type TLS struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	CAFiles    []string `yaml:"ca_files" json:"ca_files"`
	CertFile   string   `yaml:"cert_file" json:"cert_file"`
	KeyFile    string   `yaml:"key_file" json:"key_file"`
	ServerName string   `yaml:"server_name" json:"server_name"`
}

type RateLimit struct {
	Global      LimitSpec                `yaml:"global" json:"global"`
	IP          LimitSpec                `yaml:"ip" json:"ip"`
	CIDR        CIDRSpec                 `yaml:"cidr" json:"cidr"`
	GeoIP       LimitSpec                `yaml:"geoip" json:"geoip"`
	Tools       map[string]ToolLimitSpec `yaml:"tools" json:"tools,omitempty"`
	ExemptCIDRs []string                 `yaml:"exempt_cidrs" json:"exempt_cidrs,omitempty"`
}

type CIDRSpec struct {
	RequestsPerMinute int `yaml:"requests_per_minute" json:"requests_per_minute"`
	Burst             int `yaml:"burst" json:"burst"`
	IPv4Prefix        int `yaml:"ipv4_prefix" json:"ipv4_prefix"`
	IPv6Prefix        int `yaml:"ipv6_prefix" json:"ipv6_prefix"`
}

type LimitSpec struct {
	RequestsPerMinute int `yaml:"requests_per_minute" json:"requests_per_minute"`
	Burst             int `yaml:"burst" json:"burst"`
}

type ToolLimitSpec struct {
	Global LimitSpec `yaml:"global" json:"global"`
	CIDR   LimitSpec `yaml:"cidr" json:"cidr"`
	IP     LimitSpec `yaml:"ip" json:"ip"`
}

type Runtime struct {
	Count                        int `yaml:"count" json:"count"`
	MaxHops                      int `yaml:"max_hops" json:"max_hops"`
	ProbeStepTimeoutSec          int `yaml:"probe_step_timeout_sec" json:"probe_step_timeout_sec"`
	MaxToolTimeoutSec            int `yaml:"max_tool_timeout_sec" json:"max_tool_timeout_sec"`
	HTTPTimeoutSec               int `yaml:"http_timeout_sec" json:"http_timeout_sec"`
	DNSTimeoutSec                int `yaml:"dns_timeout_sec" json:"dns_timeout_sec"`
	ResolveTimeoutSec            int `yaml:"resolve_timeout_sec" json:"resolve_timeout_sec"`
	OutboundInvokeAttempts       int `yaml:"outbound_invoke_attempts" json:"outbound_invoke_attempts"`
	OutboundMaxHealthIntervalSec int `yaml:"outbound_max_health_interval_sec" json:"outbound_max_health_interval_sec"`
}

type Scheduler struct {
	AgentOfflineAfterSec int `yaml:"agent_offline_after_sec" json:"agent_offline_after_sec"`
	MaxInflightPerAgent  int `yaml:"max_inflight_per_agent" json:"max_inflight_per_agent"`
	PollIntervalSec      int `yaml:"poll_interval_sec" json:"poll_interval_sec"`
}

type Policy struct {
	Enabled     bool              `yaml:"enabled" json:"enabled"`
	AllowedArgs map[string]string `yaml:"allowed_args" json:"allowed_args,omitempty"`
}

type APITokenPermission struct {
	ID             string                  `yaml:"id" json:"id,omitempty"`
	Name           string                  `yaml:"name" json:"name,omitempty"`
	Secret         string                  `yaml:"secret" json:"secret"`
	Rotate         bool                    `yaml:"rotate" json:"rotate,omitempty"`
	All            bool                    `yaml:"all" json:"all,omitempty"`
	ScheduleAccess string                  `yaml:"schedule_access" json:"schedule_access,omitempty"`
	ManageAccess   string                  `yaml:"manage_access" json:"manage_access,omitempty"`
	Agents         []string                `yaml:"agents" json:"agents,omitempty"`
	DeniedAgents   []string                `yaml:"denied_agents" json:"denied_agents,omitempty"`
	AgentTags      []string                `yaml:"agent_tags" json:"agent_tags,omitempty"`
	DeniedTags     []string                `yaml:"denied_tags" json:"denied_tags,omitempty"`
	Tools          map[string]APIToolScope `yaml:"tools" json:"tools,omitempty"`
}

type APIToolScope struct {
	AllowedArgs    map[string]string `yaml:"allowed_args" json:"allowed_args,omitempty"`
	ResolveOnAgent *bool             `yaml:"resolve_on_agent" json:"resolve_on_agent,omitempty"`
	IPVersions     []int             `yaml:"ip_versions" json:"ip_versions,omitempty"`
}

type RegisterToken struct {
	ID              string   `yaml:"id" json:"id,omitempty"`
	Name            string   `yaml:"name" json:"name,omitempty"`
	Token           string   `yaml:"token" json:"token"`
	RemainingUses   int      `yaml:"remaining_uses" json:"remaining_uses"`
	AllowedAgentIDs []string `yaml:"allowed_agent_ids" json:"allowed_agent_ids,omitempty"`
	Rotate          bool     `yaml:"rotate" json:"rotate,omitempty"`
}

type ManagedSettings struct {
	Revision       int64                  `yaml:"revision" json:"revision,omitempty"`
	UpdatedAt      string                 `yaml:"updated_at" json:"updated_at,omitempty"`
	RateLimit      RateLimit              `yaml:"rate_limit" json:"rate_limit"`
	LabelConfigs   map[string]LabelConfig `yaml:"label_configs" json:"label_configs,omitempty"`
	APITokens      []APITokenPermission   `yaml:"api_tokens" json:"api_tokens,omitempty"`
	RegisterTokens []RegisterToken        `yaml:"register_tokens" json:"register_tokens,omitempty"`
}

type LabelConfig struct {
	Runtime      *Runtime          `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Scheduler    *Scheduler        `yaml:"scheduler,omitempty" json:"scheduler,omitempty"`
	ToolPolicies map[string]Policy `yaml:"tool_policies,omitempty" json:"tool_policies,omitempty"`
}

// IsPolicyLabel identifies labels that may configure agent execution. Other
// labels only group nodes for selection and access scopes.
func IsPolicyLabel(label string) bool {
	return label == AgentAllLabel || strings.HasPrefix(label, "agent:") || strings.HasPrefix(label, AgentIDLabelPrefix)
}

const (
	OutboundTransportHTTP  = "http"
	OutboundTransportEvent = "event"

	OutboundAuthNone            = "none"
	OutboundAuthBearer          = "bearer"
	OutboundAuthHeader          = "header"
	OutboundAuthHuaweiCloudAKSK = "huaweicloud_aksk"

	EventResponseDirect      = "direct"
	EventResponseResultField = "result_field"
)

type OutboundAgentAuth struct {
	Type      string `json:"type" yaml:"type"`
	Header    string `json:"header,omitempty" yaml:"header,omitempty"`
	AccessKey string `json:"access_key,omitempty" yaml:"access_key,omitempty"`
	ProjectID string `json:"project_id,omitempty" yaml:"project_id,omitempty"`
	Secret    string `json:"secret,omitempty" yaml:"secret,omitempty"`
}

type OutboundAgent struct {
	ID            string            `json:"id" yaml:"id"`
	Transport     string            `json:"transport" yaml:"transport"`
	Enabled       bool              `json:"enabled" yaml:"enabled"`
	URL           string            `json:"url" yaml:"url"`
	Headers       map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Auth          OutboundAgentAuth `json:"auth" yaml:"auth"`
	EventResponse string            `json:"event_response,omitempty" yaml:"event_response,omitempty"`
	Labels        []string          `json:"labels,omitempty" yaml:"labels"`
	TLS           TLS               `json:"tls" yaml:"tls"`
	CreatedAt     string            `json:"created_at,omitempty" yaml:"-"`
	UpdatedAt     string            `json:"updated_at,omitempty" yaml:"-"`
}

type AgentConfig struct {
	ID        string   `json:"id" yaml:"id"`
	Disabled  bool     `json:"disabled" yaml:"disabled"`
	Labels    []string `json:"labels,omitempty" yaml:"labels"`
	CreatedAt string   `json:"created_at,omitempty" yaml:"-"`
	UpdatedAt string   `json:"updated_at,omitempty" yaml:"-"`
}

func LoadServer(path string) (Server, error) {
	var cfg Server
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := loadYAML(path, &cfg); err != nil {
		return cfg, err
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}
	if cfg.GRPCAddr == "" {
		cfg.GRPCAddr = ":8443"
	}
	return cfg, nil
}

func NormalizeManagedSettings(settings *ManagedSettings) error {
	DefaultRateLimit(&settings.RateLimit)
	settings.LabelConfigs = NormalizeLabelConfigs(settings.LabelConfigs)
	EnsureAgentLabelConfig(settings)
	if err := validateManagedRuntimeBounds(settings.LabelConfigs); err != nil {
		return err
	}
	if err := NormalizeAPITokenPermissions(settings.APITokens); err != nil {
		return err
	}
	var err error
	settings.RegisterTokens, err = NormalizeRegisterTokens(settings.RegisterTokens)
	if err != nil {
		return err
	}
	ClearManagedSettingsRotateFlags(settings)
	return nil
}

func validateManagedRuntimeBounds(configs map[string]LabelConfig) error {
	for label, cfg := range configs {
		if cfg.Runtime != nil {
			checks := []struct {
				name                    string
				value, minimum, maximum int
			}{
				{name: "count", value: cfg.Runtime.Count, minimum: 1, maximum: 10},
				{name: "max_hops", value: cfg.Runtime.MaxHops, minimum: 1, maximum: 30},
				{name: "probe_step_timeout_sec", value: cfg.Runtime.ProbeStepTimeoutSec, minimum: 1, maximum: 1},
				{name: "max_tool_timeout_sec", value: cfg.Runtime.MaxToolTimeoutSec, minimum: 1, maximum: 300},
				{name: "http_timeout_sec", value: cfg.Runtime.HTTPTimeoutSec, minimum: 1, maximum: 5},
				{name: "dns_timeout_sec", value: cfg.Runtime.DNSTimeoutSec, minimum: 1, maximum: 5},
				{name: "resolve_timeout_sec", value: cfg.Runtime.ResolveTimeoutSec, minimum: 1, maximum: 3},
				{name: "outbound_invoke_attempts", value: cfg.Runtime.OutboundInvokeAttempts, minimum: 1, maximum: 3},
				{name: "outbound_max_health_interval_sec", value: cfg.Runtime.OutboundMaxHealthIntervalSec, minimum: 1, maximum: 300},
			}
			for _, check := range checks {
				if check.value < check.minimum || check.value > check.maximum {
					return fmt.Errorf("label_configs[%q].runtime.%s must be between %d and %d", label, check.name, check.minimum, check.maximum)
				}
			}
		}
		if cfg.Scheduler != nil {
			checks := []struct {
				name                    string
				value, minimum, maximum int
			}{
				{name: "agent_offline_after_sec", value: cfg.Scheduler.AgentOfflineAfterSec, minimum: 1, maximum: 90},
				{name: "max_inflight_per_agent", value: cfg.Scheduler.MaxInflightPerAgent, minimum: 1, maximum: 4},
				{name: "poll_interval_sec", value: cfg.Scheduler.PollIntervalSec, minimum: 1, maximum: 2},
			}
			for _, check := range checks {
				if check.value < check.minimum || check.value > check.maximum {
					return fmt.Errorf("label_configs[%q].scheduler.%s must be between %d and %d", label, check.name, check.minimum, check.maximum)
				}
			}
		}
	}
	return nil
}

func ClearManagedSettingsRotateFlags(settings *ManagedSettings) {
	for i := range settings.APITokens {
		settings.APITokens[i].Rotate = false
	}
	for i := range settings.RegisterTokens {
		settings.RegisterTokens[i].Rotate = false
	}
}

func DefaultManagedSettings() ManagedSettings {
	settings := ManagedSettings{}
	_ = NormalizeManagedSettings(&settings)
	return settings
}

func ManagedSettingsWithAdminToken(secret string) ManagedSettings {
	settings := DefaultManagedSettings()
	settings.APITokens = []APITokenPermission{DefaultAdminAPIToken(secret)}
	_ = NormalizeManagedSettings(&settings)
	return settings
}

func DefaultAdminAPIToken(secret string) APITokenPermission {
	return APITokenPermission{
		Name:           "admin",
		Secret:         secret,
		All:            true,
		ScheduleAccess: "write",
		ManageAccess:   "write",
		Agents:         []string{"*"},
	}
}

func CloneManagedSettings(settings ManagedSettings) ManagedSettings {
	settings.RateLimit = cloneRateLimit(settings.RateLimit)
	settings.LabelConfigs = cloneLabelConfigs(settings.LabelConfigs)
	settings.APITokens = cloneAPITokenPermissions(settings.APITokens)
	settings.RegisterTokens = cloneRegisterTokens(settings.RegisterTokens)
	return settings
}

func EnsureAgentLabelConfig(settings *ManagedSettings) {
	if settings.LabelConfigs == nil {
		settings.LabelConfigs = map[string]LabelConfig{}
	}
	cfg := settings.LabelConfigs[AgentAllLabel]
	if cfg.Runtime == nil {
		runtime := DefaultRuntime()
		cfg.Runtime = &runtime
	}
	if cfg.Scheduler == nil {
		var scheduler Scheduler
		DefaultScheduler(&scheduler)
		cfg.Scheduler = &scheduler
	}
	if cfg.ToolPolicies == nil {
		cfg.ToolPolicies = map[string]Policy{}
	}
	settings.LabelConfigs[AgentAllLabel] = cfg
}

func NormalizeLabelConfigs(configs map[string]LabelConfig) map[string]LabelConfig {
	if len(configs) == 0 {
		return nil
	}
	out := make(map[string]LabelConfig, len(configs))
	for label, cfg := range configs {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		if !IsPolicyLabel(label) {
			// Keep legacy group labels, but discard their execution settings.
			out[label] = LabelConfig{}
			continue
		}
		if cfg.Runtime != nil {
			runtime := *cfg.Runtime
			DefaultRuntimeValues(&runtime)
			cfg.Runtime = &runtime
		}
		if cfg.Scheduler != nil {
			scheduler := *cfg.Scheduler
			DefaultScheduler(&scheduler)
			cfg.Scheduler = &scheduler
		}
		cfg.ToolPolicies = clonePolicies(cfg.ToolPolicies)
		out[label] = cfg
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneRateLimit(rateLimit RateLimit) RateLimit {
	rateLimit.Tools = cloneToolLimits(rateLimit.Tools)
	rateLimit.ExemptCIDRs = cloneStringSlice(rateLimit.ExemptCIDRs)
	return rateLimit
}

func clonePolicies(policies map[string]Policy) map[string]Policy {
	if policies == nil {
		return nil
	}
	out := make(map[string]Policy, len(policies))
	for name, policy := range policies {
		policy.AllowedArgs = cloneStringMap(policy.AllowedArgs)
		out[name] = policy
	}
	return out
}

func cloneLabelConfigs(configs map[string]LabelConfig) map[string]LabelConfig {
	if configs == nil {
		return nil
	}
	out := make(map[string]LabelConfig, len(configs))
	for label, cfg := range configs {
		if cfg.Runtime != nil {
			runtime := *cfg.Runtime
			cfg.Runtime = &runtime
		}
		if cfg.Scheduler != nil {
			scheduler := *cfg.Scheduler
			cfg.Scheduler = &scheduler
		}
		cfg.ToolPolicies = clonePolicies(cfg.ToolPolicies)
		out[label] = cfg
	}
	return out
}

func cloneToolLimits(limits map[string]ToolLimitSpec) map[string]ToolLimitSpec {
	if limits == nil {
		return nil
	}
	out := make(map[string]ToolLimitSpec, len(limits))
	for name, limit := range limits {
		out[name] = limit
	}
	return out
}

func cloneAPITokenPermissions(tokens []APITokenPermission) []APITokenPermission {
	if tokens == nil {
		return nil
	}
	out := make([]APITokenPermission, len(tokens))
	for i, token := range tokens {
		token.Agents = cloneStringSlice(token.Agents)
		token.DeniedAgents = cloneStringSlice(token.DeniedAgents)
		token.AgentTags = cloneStringSlice(token.AgentTags)
		token.DeniedTags = cloneStringSlice(token.DeniedTags)
		token.Tools = cloneAPIToolScopes(token.Tools)
		out[i] = token
	}
	return out
}

func cloneRegisterTokens(tokens []RegisterToken) []RegisterToken {
	if tokens == nil {
		return nil
	}
	out := make([]RegisterToken, len(tokens))
	for i, token := range tokens {
		token.AllowedAgentIDs = cloneStringSlice(token.AllowedAgentIDs)
		out[i] = token
	}
	return out
}

func cloneAPIToolScopes(scopes map[string]APIToolScope) map[string]APIToolScope {
	if scopes == nil {
		return nil
	}
	out := make(map[string]APIToolScope, len(scopes))
	for name, scope := range scopes {
		scope.AllowedArgs = cloneStringMap(scope.AllowedArgs)
		scope.ResolveOnAgent = cloneBoolPtr(scope.ResolveOnAgent)
		scope.IPVersions = cloneIntSlice(scope.IPVersions)
		out[name] = scope
	}
	return out
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func cloneIntSlice(values []int) []int {
	if values == nil {
		return nil
	}
	out := make([]int, len(values))
	copy(out, values)
	return out
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func NormalizeAPITokenPermissions(perms []APITokenPermission) error {
	seen := map[string]struct{}{}
	seenIDs := map[string]struct{}{}
	for i := range perms {
		id := strings.TrimSpace(perms[i].ID)
		if id == "" {
			id = uuid.NewString()
		}
		if _, ok := seenIDs[id]; ok {
			return fmt.Errorf("api_tokens[%d].id is duplicated", i)
		}
		seenIDs[id] = struct{}{}
		perms[i].ID = id
		secret := strings.TrimSpace(perms[i].Secret)
		if secret == "" {
			secret = uuid.NewString()
		}
		if _, ok := seen[secret]; ok {
			return fmt.Errorf("api_tokens[%d].secret is duplicated", i)
		}
		seen[secret] = struct{}{}
		perms[i].Secret = secret
		switch access := strings.TrimSpace(strings.ToLower(perms[i].ScheduleAccess)); access {
		case "", "none":
			perms[i].ScheduleAccess = "none"
		case "read", "write":
			perms[i].ScheduleAccess = access
		default:
			return fmt.Errorf("api_tokens[%d].schedule_access must be none, read, or write", i)
		}
		switch access := strings.TrimSpace(strings.ToLower(perms[i].ManageAccess)); access {
		case "", "none":
			perms[i].ManageAccess = "none"
		case "read", "write":
			perms[i].ManageAccess = access
		default:
			return fmt.Errorf("api_tokens[%d].manage_access must be none, read, or write", i)
		}
	}
	return nil
}

func NormalizeRegisterTokens(tokens []RegisterToken) ([]RegisterToken, error) {
	out := make([]RegisterToken, 0, len(tokens))
	seen := map[string]struct{}{}
	seenIDs := map[string]struct{}{}
	for i, item := range tokens {
		item.ID = strings.TrimSpace(item.ID)
		if item.ID == "" {
			item.ID = uuid.NewString()
		}
		if _, ok := seenIDs[item.ID]; ok {
			return nil, fmt.Errorf("register_tokens[%d].id is duplicated", i)
		}
		seenIDs[item.ID] = struct{}{}
		item.Name = strings.TrimSpace(item.Name)
		var err error
		item.AllowedAgentIDs, err = validateRegisterTokenAgentIDs(i, item.AllowedAgentIDs)
		if err != nil {
			return nil, err
		}
		if item.RemainingUses < 0 {
			return nil, fmt.Errorf("register_tokens[%d].remaining_uses must be non-negative", i)
		}
		item.Token = strings.TrimSpace(item.Token)
		if item.Token == "" {
			item.Token = uuid.NewString()
		}
		if _, ok := seen[item.Token]; ok {
			return nil, fmt.Errorf("register_tokens[%d].token is duplicated", i)
		}
		seen[item.Token] = struct{}{}
		out = append(out, item)
	}
	return out, nil
}

func validateRegisterTokenAgentIDs(tokenIndex int, values []string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for valueIndex, value := range values {
		if err := ValidateAgentID(value); err != nil {
			return nil, fmt.Errorf("register_tokens[%d].allowed_agent_ids[%d]: %w", tokenIndex, valueIndex, err)
		}
		if _, ok := seen[value]; ok {
			return nil, fmt.Errorf("register_tokens[%d].allowed_agent_ids contains duplicate agent id %q", tokenIndex, value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

// ValidateAgentID enforces the canonical identifier shared by configuration,
// persistence, and the Agent wire protocol.
func ValidateAgentID(id string) error {
	if id == "" {
		return fmt.Errorf("agent id is required")
	}
	if len(id) > 128 {
		return fmt.Errorf("agent id exceeds 128 bytes")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("agent id %q is reserved", id)
	}
	if !isASCIIAlphaNumeric(id[0]) {
		return fmt.Errorf("agent id must start with an ASCII letter or digit")
	}
	for i := 1; i < len(id); i++ {
		if !isASCIIAlphaNumeric(id[i]) && id[i] != '.' && id[i] != '_' && id[i] != '-' {
			return fmt.Errorf("agent id may only contain ASCII letters, digits, '.', '_', or '-'")
		}
	}
	return nil
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func DefaultRuntime() Runtime {
	var runtime Runtime
	DefaultRuntimeValues(&runtime)
	return runtime
}

func DefaultRuntimeValues(cfg *Runtime) {
	if cfg.Count == 0 {
		cfg.Count = 10
	}
	if cfg.MaxHops == 0 {
		cfg.MaxHops = 30
	}
	if cfg.ProbeStepTimeoutSec == 0 {
		cfg.ProbeStepTimeoutSec = 1
	}
	if cfg.MaxToolTimeoutSec == 0 {
		cfg.MaxToolTimeoutSec = 300
	}
	if cfg.HTTPTimeoutSec == 0 {
		cfg.HTTPTimeoutSec = 5
	}
	if cfg.DNSTimeoutSec == 0 {
		cfg.DNSTimeoutSec = 5
	}
	if cfg.ResolveTimeoutSec == 0 {
		cfg.ResolveTimeoutSec = 3
	}
	if cfg.OutboundInvokeAttempts == 0 {
		cfg.OutboundInvokeAttempts = 3
	}
	if cfg.OutboundMaxHealthIntervalSec == 0 {
		cfg.OutboundMaxHealthIntervalSec = 300
	}
}

func DefaultScheduler(cfg *Scheduler) {
	if cfg.AgentOfflineAfterSec == 0 {
		cfg.AgentOfflineAfterSec = 90
	}
	if cfg.MaxInflightPerAgent == 0 {
		cfg.MaxInflightPerAgent = 4
	}
	if cfg.PollIntervalSec == 0 {
		cfg.PollIntervalSec = 2
	}
}

func DefaultRateLimit(cfg *RateLimit) {
	if cfg.Global.RequestsPerMinute == 0 {
		cfg.Global.RequestsPerMinute = 600
	}
	if cfg.Global.Burst == 0 {
		cfg.Global.Burst = 200
	}
	if cfg.IP.RequestsPerMinute == 0 {
		cfg.IP.RequestsPerMinute = 60
	}
	if cfg.IP.Burst == 0 {
		cfg.IP.Burst = 20
	}
	if cfg.CIDR.RequestsPerMinute == 0 {
		cfg.CIDR.RequestsPerMinute = 300
	}
	if cfg.CIDR.Burst == 0 {
		cfg.CIDR.Burst = 100
	}
	if cfg.CIDR.IPv4Prefix == 0 {
		cfg.CIDR.IPv4Prefix = 32
	}
	if cfg.CIDR.IPv6Prefix == 0 {
		cfg.CIDR.IPv6Prefix = 128
	}
	if cfg.GeoIP.RequestsPerMinute == 0 {
		cfg.GeoIP.RequestsPerMinute = 120
	}
	if cfg.GeoIP.Burst == 0 {
		cfg.GeoIP.Burst = 60
	}
}

func LoadAgent(path string) (Agent, error) {
	var cfg Agent
	if err := applyEnv(&cfg); err != nil {
		return cfg, err
	}
	if err := loadYAML(path, &cfg); err != nil {
		return cfg, err
	}
	defaultAgent(&cfg)
	var err error
	cfg.HTTPAuth, err = NormalizeAgentHTTPAuth(cfg.Mode, cfg.HTTPAuth)
	if err != nil {
		return cfg, fmt.Errorf("http_auth: %w", err)
	}
	if err := ValidateAgentID(cfg.ID); err != nil {
		return cfg, fmt.Errorf("id: %w", err)
	}
	cfg.Capabilities, err = normalizeAgentCapabilities(cfg.Capabilities)
	if err != nil {
		return cfg, err
	}
	if len(cfg.Capabilities) == 0 {
		return cfg, fmt.Errorf("capabilities must contain at least one tool")
	}
	if cfg.Protocols == 0 {
		return cfg, fmt.Errorf("protocols must explicitly enable IPv4, IPv6, or both")
	}
	if unsupported := cfg.Protocols &^ uint8(3); unsupported != 0 {
		return cfg, fmt.Errorf("protocols contains unsupported bits: 0x%x", unsupported)
	}
	cfg.HTTPPathPrefix, err = NormalizePathPrefix(cfg.HTTPPathPrefix)
	if err != nil {
		return cfg, fmt.Errorf("http_path_prefix: %w", err)
	}
	return cfg, nil
}

func NormalizeAgentHTTPAuth(mode string, auth HTTPAuth) (HTTPAuth, error) {
	var hasHTTP, hasEvent bool
	for _, part := range strings.Split(mode, ",") {
		switch part {
		case "http":
			hasHTTP = true
		case "event":
			hasEvent = true
		}
	}
	if auth.Type != strings.TrimSpace(auth.Type) {
		return auth, fmt.Errorf("type must not have surrounding whitespace")
	}
	if auth.Header != strings.TrimSpace(auth.Header) {
		return auth, fmt.Errorf("header must not have surrounding whitespace")
	}
	if auth.Secret != strings.TrimSpace(auth.Secret) {
		return auth, fmt.Errorf("secret must not have surrounding whitespace")
	}
	if auth.Type == "" {
		if auth.Header != "" || auth.Secret != "" {
			return auth, fmt.Errorf("type is required when header or secret is configured")
		}
		if hasHTTP || hasEvent {
			return auth, fmt.Errorf("type must be explicitly configured for http or event mode")
		}
		return auth, nil
	}

	switch auth.Type {
	case HTTPAuthNone:
		if auth.Header != "" || auth.Secret != "" {
			return auth, fmt.Errorf("none auth must not include header or secret")
		}
	case HTTPAuthBearer:
		if auth.Header != "" {
			return auth, fmt.Errorf("bearer auth must not include header")
		}
		if auth.Secret == "" {
			return auth, fmt.Errorf("bearer auth secret is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return auth, fmt.Errorf("bearer auth secret is not a valid header value")
		}
	case HTTPAuthHeader:
		if auth.Header == "" || !httpguts.ValidHeaderFieldName(auth.Header) {
			return auth, fmt.Errorf("header auth header is invalid")
		}
		auth.Header = http.CanonicalHeaderKey(auth.Header)
		if ReservedHTTPAuthHeader(auth.Header) {
			return auth, fmt.Errorf("header auth header %q is reserved", auth.Header)
		}
		if auth.Secret == "" {
			return auth, fmt.Errorf("header auth secret is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return auth, fmt.Errorf("header auth secret is not a valid header value")
		}
	default:
		return auth, fmt.Errorf("type must be none, bearer, or header")
	}

	if hasHTTP && auth.Type != HTTPAuthBearer && auth.Type != HTTPAuthHeader {
		return auth, fmt.Errorf("mode containing http requires bearer or header auth")
	}
	if !hasHTTP && !hasEvent {
		return auth, fmt.Errorf("grpc-only mode requires http_auth to be omitted")
	}
	return auth, nil
}

func ReservedHTTPAuthHeader(header string) bool {
	header = strings.ToLower(header)
	if header == "x-forwarded" || strings.HasPrefix(header, "x-forwarded-") {
		return true
	}
	switch header {
	case "accept", "authorization", "connection", "content-length", "content-type", "host", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade",
		"forwarded", "x-real-ip", "via", "true-client-ip", "cf-connecting-ip", "x-client-ip", "x-envoy-external-address",
		"x-sdk-date", "x-sdk-content-sha256", "x-project-id", "x-security-token":
		return true
	default:
		return false
	}
}

// ValidHuaweiCloudAccessKey reports whether an access key can be embedded in
// the unquoted Access component of an SDK-HMAC-SHA256 Authorization header.
func ValidHuaweiCloudAccessKey(accessKey string) bool {
	return accessKey != "" &&
		httpguts.ValidHeaderFieldValue(accessKey) &&
		!strings.ContainsRune(accessKey, ',') &&
		strings.IndexFunc(accessKey, unicode.IsSpace) < 0
}

func normalizeAgentCapabilities(capabilities []string) ([]string, error) {
	known := map[string]struct{}{
		"ping":       {},
		"traceroute": {},
		"mtr":        {},
		"http":       {},
		"dns":        {},
		"port":       {},
	}
	out := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability == "" || capability != strings.TrimSpace(capability) {
			return nil, fmt.Errorf("agent capability %q is not canonical", capability)
		}
		if _, ok := known[capability]; !ok {
			return nil, fmt.Errorf("unknown agent capability %q", capability)
		}
		if _, ok := seen[capability]; ok {
			return nil, fmt.Errorf("agent capability %q is duplicated", capability)
		}
		seen[capability] = struct{}{}
		out = append(out, capability)
	}
	return out, nil
}

func NormalizePathPrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	if prefix == "" || prefix == "/" {
		return "", nil
	}
	if strings.ContainsAny(prefix, "?#") {
		return "", fmt.Errorf("must not contain query or fragment")
	}
	if strings.ContainsAny(prefix, " \t\r\n") {
		return "", fmt.Errorf("must not contain whitespace")
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimRight(prefix, "/"), nil
}

func defaultAgent(cfg *Agent) {
	if cfg.Mode == "" {
		cfg.Mode = "grpc"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultAgentHTTPAddr(cfg.Mode)
	}
	defaultSpeedtest(&cfg.Speedtest)
}

func defaultAgentHTTPAddr(mode string) string {
	for _, part := range strings.Split(mode, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "event") {
			return ":8000"
		}
	}
	return ":9000"
}

func defaultSpeedtest(cfg *Speedtest) {
	if cfg.DefaultBytes <= 0 {
		cfg.DefaultBytes = 1 << 20
	}
	if cfg.MaxBytes < 0 {
		cfg.MaxBytes = 0
	}
	if cfg.MaxBytes > 0 && cfg.DefaultBytes > cfg.MaxBytes {
		cfg.DefaultBytes = cfg.MaxBytes
	}
	if cfg.GlobalRequestsPerMinute == 0 {
		cfg.GlobalRequestsPerMinute = 120
	}
	if cfg.GlobalBurst == 0 {
		cfg.GlobalBurst = 20
	}
	if cfg.IPRequestsPerMinute == 0 {
		cfg.IPRequestsPerMinute = 12
	}
	if cfg.IPBurst == 0 {
		cfg.IPBurst = 3
	}
}

func applyEnv(out any) error {
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config env target must be pointer to struct")
	}
	allowed := make(map[string]struct{})
	collectEnvNames(v.Elem().Type(), nil, allowed)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(name, "MTR_") {
			continue
		}
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("unknown config environment variable %s", name)
		}
	}
	return applyEnvValue(v.Elem(), nil)
}

func collectEnvNames(t reflect.Type, path []string, out map[string]struct{}) {
	if len(path) > 0 {
		out[envName(path)] = struct{}{}
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := yamlFieldName(field)
		if name == "" || name == "-" {
			continue
		}
		collectEnvNames(field.Type, append(path, name), out)
	}
}

func applyEnvValue(v reflect.Value, path []string) error {
	if len(path) > 0 {
		name := envName(path)
		if raw, ok := os.LookupEnv(name); ok {
			if err := setEnvValue(v, raw); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := yamlFieldName(field)
		if name == "" || name == "-" {
			continue
		}
		if err := applyEnvValue(v.Field(i), append(path, name)); err != nil {
			return err
		}
	}
	return nil
}

func setEnvValue(v reflect.Value, raw string) error {
	if !v.CanSet() {
		return fmt.Errorf("field cannot be set")
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(raw, 0, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(parsed)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(raw, 0, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(parsed)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String && !looksStructured(raw) {
			items := splitEnvList(raw)
			slice := reflect.MakeSlice(v.Type(), len(items), len(items))
			for i, item := range items {
				slice.Index(i).SetString(item)
			}
			v.Set(slice)
			return nil
		}
		return unmarshalEnvYAML(v, raw)
	case reflect.Map, reflect.Struct:
		return unmarshalEnvYAML(v, raw)
	default:
		return fmt.Errorf("unsupported config field type %s", v.Type())
	}
	return nil
}

func unmarshalEnvYAML(v reflect.Value, raw string) error {
	holder := reflect.New(v.Type())
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(holder.Interface()); err != nil {
		return err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("environment config must contain exactly one document")
		}
		return err
	}
	v.Set(holder.Elem())
	return nil
}

func envName(path []string) string {
	parts := make([]string, 0, len(path)+1)
	parts = append(parts, "MTR")
	for _, part := range path {
		parts = append(parts, strings.ToUpper(strings.ReplaceAll(part, "-", "_")))
	}
	return strings.Join(parts, "_")
}

func yamlFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(field.Name)
	}
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func looksStructured(raw string) bool {
	raw = strings.TrimSpace(raw)
	return strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "{") || strings.Contains(raw, "\n")
}

func splitEnvList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func loadYAML(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return err
	}
	document := yamlDocumentValue(&root)
	if document == nil || document.Kind == 0 || document.Kind == yaml.ScalarNode && (document.Tag == "!!null" || strings.TrimSpace(document.Value) == "") {
		return nil
	}
	if document.Kind != yaml.MappingNode {
		return fmt.Errorf("config yaml document must be a mapping")
	}
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config yaml target must be pointer to struct")
	}
	fileValue := reflect.New(v.Elem().Type())
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(fileValue.Interface()); err != nil {
		return err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("config yaml must contain exactly one document")
		}
		return err
	}
	mergeYAMLValue(v.Elem(), fileValue.Elem(), document)
	return nil
}

func mergeYAMLValue(dst reflect.Value, src reflect.Value, node *yaml.Node) {
	if node == nil {
		return
	}
	if dst.Kind() != reflect.Struct || src.Kind() != reflect.Struct || node.Kind != yaml.MappingNode {
		if dst.CanSet() {
			dst.Set(src)
		}
		return
	}
	t := dst.Type()
	for i := 0; i < dst.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := yamlFieldName(field)
		if name == "" || name == "-" {
			continue
		}
		child := yamlMappingValue(node, name)
		if child == nil {
			continue
		}
		dstField := dst.Field(i)
		srcField := src.Field(i)
		if dstField.Kind() == reflect.Struct && srcField.Kind() == reflect.Struct && child.Kind == yaml.MappingNode {
			mergeYAMLValue(dstField, srcField, child)
			continue
		}
		if dstField.CanSet() {
			dstField.Set(srcField)
		}
	}
}

func yamlDocumentValue(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		return root.Content[0]
	}
	return root
}

func yamlMappingValue(node *yaml.Node, name string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == name {
			return node.Content[i+1]
		}
	}
	return nil
}
