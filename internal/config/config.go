package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	AgentAllLabel      = "agent"
	AgentGRPCLabel     = "agent:grpc"
	AgentHTTPLabel     = "agent:http"
	AgentIDLabelPrefix = "id:"
)

type Server struct {
	HTTPAddr        string   `yaml:"http_addr"`
	GRPCAddr        string   `yaml:"grpc_addr"`
	LogLevel        string   `yaml:"log_level"`
	DatabaseURL     string   `yaml:"database_url"`
	GeoIPURL        string   `yaml:"geoip_url"`
	TrustedProxies  []string `yaml:"trusted_proxies"`
	ClientIPHeaders []string `yaml:"client_ip_headers"`
	TLS             TLS      `yaml:"tls"`
}

type HTTPPeer struct {
	ID        string `yaml:"id"`
	BaseURL   string `yaml:"base_url"`
	HTTPToken string `yaml:"http_token"`
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
	HTTPToken      string    `yaml:"http_token"`
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

type TLS struct {
	Enabled  bool     `yaml:"enabled"`
	CAFiles  []string `yaml:"ca_files"`
	CertFile string   `yaml:"cert_file"`
	KeyFile  string   `yaml:"key_file"`
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
	Count                    int `yaml:"count" json:"count"`
	MaxHops                  int `yaml:"max_hops" json:"max_hops"`
	ProbeStepTimeoutSec      int `yaml:"probe_step_timeout_sec" json:"probe_step_timeout_sec"`
	MaxToolTimeoutSec        int `yaml:"max_tool_timeout_sec" json:"max_tool_timeout_sec"`
	HTTPTimeoutSec           int `yaml:"http_timeout_sec" json:"http_timeout_sec"`
	DNSTimeoutSec            int `yaml:"dns_timeout_sec" json:"dns_timeout_sec"`
	ResolveTimeoutSec        int `yaml:"resolve_timeout_sec" json:"resolve_timeout_sec"`
	HTTPInvokeAttempts       int `yaml:"http_invoke_attempts" json:"http_invoke_attempts"`
	HTTPMaxHealthIntervalSec int `yaml:"http_max_health_interval_sec" json:"http_max_health_interval_sec"`
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
	ID     string `yaml:"id" json:"id,omitempty"`
	Name   string `yaml:"name" json:"name,omitempty"`
	Token  string `yaml:"token" json:"token"`
	Rotate bool   `yaml:"rotate" json:"rotate,omitempty"`
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

type HTTPAgent struct {
	ID        string   `json:"id" yaml:"id"`
	Enabled   bool     `json:"enabled" yaml:"enabled"`
	BaseURL   string   `json:"base_url" yaml:"base_url"`
	HTTPToken string   `json:"http_token,omitempty" yaml:"http_token"`
	Labels    []string `json:"labels,omitempty" yaml:"labels"`
	TLS       TLS      `json:"tls" yaml:"tls"`
	CreatedAt string   `json:"created_at,omitempty" yaml:"-"`
	UpdatedAt string   `json:"updated_at,omitempty" yaml:"-"`
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
	settings.APITokens = []APITokenPermission{DefaultAdminAPIToken()}
	_ = NormalizeManagedSettings(&settings)
	return settings
}

func DefaultAdminAPIToken() APITokenPermission {
	return APITokenPermission{
		Name:           "admin",
		Secret:         uuid.NewString(),
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
	copy(out, tokens)
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
	if cfg.HTTPInvokeAttempts == 0 {
		cfg.HTTPInvokeAttempts = 3
	}
	if cfg.HTTPMaxHealthIntervalSec == 0 {
		cfg.HTTPMaxHealthIntervalSec = 300
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
	cfg.HTTPPathPrefix, err = NormalizePathPrefix(cfg.HTTPPathPrefix)
	if err != nil {
		return cfg, fmt.Errorf("http_path_prefix: %w", err)
	}
	return cfg, nil
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
		cfg.HTTPAddr = ":9000"
	}
	if cfg.Protocols == 0 {
		cfg.Protocols = 3
	}
	if len(cfg.Capabilities) == 0 {
		cfg.Capabilities = []string{"ping", "traceroute", "mtr", "http", "dns", "port"}
	}
	defaultSpeedtest(&cfg.Speedtest)
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
	return applyEnvValue(v.Elem(), nil)
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
	if err := yaml.Unmarshal([]byte(raw), holder.Interface()); err != nil {
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
	v := reflect.ValueOf(out)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config yaml target must be pointer to struct")
	}
	fileValue := reflect.New(v.Elem().Type())
	if err := yaml.Unmarshal(b, fileValue.Interface()); err != nil {
		return err
	}
	mergeYAMLValue(v.Elem(), fileValue.Elem(), yamlDocumentValue(&root))
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
