package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Server struct {
	HTTPAddr            string               `yaml:"http_addr"`
	GRPCAddr            string               `yaml:"grpc_addr"`
	LogLevel            string               `yaml:"log_level"`
	DatabaseURL         string               `yaml:"database_url"`
	GeoIPURL            string               `yaml:"geoip_url"`
	TrustedProxies      []string             `yaml:"trusted_proxies"`
	ClientIPHeaders     []string             `yaml:"client_ip_headers"`
	APITokenPermissions []APITokenPermission `yaml:"api_tokens"`
	RegisterToken       string               `yaml:"register_token"`
	TLS                 TLS                  `yaml:"tls"`
	RateLimit           RateLimit            `yaml:"rate_limit"`
	Runtime             Runtime              `yaml:"runtime"`
	Scheduler           Scheduler            `yaml:"scheduler"`
	OutboundTLS         TLS                  `yaml:"outbound_tls"`
	OutboundAgents      []OutboundAgent      `yaml:"outbound_agents"`
	ToolPolicies        map[string]Policy    `yaml:"tool_policies"`
}

type OutboundAgent struct {
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
	Global      LimitSpec                `yaml:"global"`
	IP          LimitSpec                `yaml:"ip"`
	CIDR        CIDRSpec                 `yaml:"cidr"`
	Tools       map[string]ToolLimitSpec `yaml:"tools"`
	ExemptCIDRs []string                 `yaml:"exempt_cidrs"`
}

type CIDRSpec struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	Burst             int `yaml:"burst"`
	IPv4Prefix        int `yaml:"ipv4_prefix"`
	IPv6Prefix        int `yaml:"ipv6_prefix"`
}

type LimitSpec struct {
	RequestsPerMinute int `yaml:"requests_per_minute"`
	Burst             int `yaml:"burst"`
}

type ToolLimitSpec struct {
	Global LimitSpec `yaml:"global"`
	CIDR   LimitSpec `yaml:"cidr"`
	IP     LimitSpec `yaml:"ip"`
}

type Runtime struct {
	Count                        int `yaml:"count"`
	MaxHops                      int `yaml:"max_hops"`
	ProbeStepTimeoutSec          int `yaml:"probe_step_timeout_sec"`
	MaxToolTimeoutSec            int `yaml:"max_tool_timeout_sec"`
	HTTPTimeoutSec               int `yaml:"http_timeout_sec"`
	DNSTimeoutSec                int `yaml:"dns_timeout_sec"`
	ResolveTimeoutSec            int `yaml:"resolve_timeout_sec"`
	OutboundInvokeAttempts       int `yaml:"outbound_invoke_attempts"`
	OutboundMaxHealthIntervalSec int `yaml:"outbound_max_health_interval_sec"`
}

type Scheduler struct {
	AgentOfflineAfterSec        int `yaml:"agent_offline_after_sec"`
	GRPCMaxInflightPerAgent     int `yaml:"grpc_max_inflight_per_agent"`
	OutboundMaxInflightPerAgent int `yaml:"outbound_max_inflight_per_agent"`
	PollIntervalSec             int `yaml:"poll_interval_sec"`
}

type Policy struct {
	Enabled       bool              `yaml:"enabled"`
	AllowedArgs   map[string]string `yaml:"allowed_args"`
	HideFirstHops int               `yaml:"hide_first_hops"`
}

type APITokenPermission struct {
	Secret         string                  `yaml:"secret"`
	All            bool                    `yaml:"all"`
	ScheduleAccess string                  `yaml:"schedule_access"`
	Agents         []string                `yaml:"agents"`
	Tools          map[string]APIToolScope `yaml:"tools"`
}

type APIToolScope struct {
	AllowedArgs    map[string]string `yaml:"allowed_args"`
	ResolveOnAgent *bool             `yaml:"resolve_on_agent"`
	IPVersions     []int             `yaml:"ip_versions"`
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
	defaultRateLimit(&cfg.RateLimit)
	defaultRuntime(&cfg.Runtime)
	if cfg.Scheduler.AgentOfflineAfterSec == 0 {
		cfg.Scheduler.AgentOfflineAfterSec = 90
	}
	if cfg.Scheduler.GRPCMaxInflightPerAgent == 0 {
		cfg.Scheduler.GRPCMaxInflightPerAgent = 4
	}
	if cfg.Scheduler.OutboundMaxInflightPerAgent == 0 {
		cfg.Scheduler.OutboundMaxInflightPerAgent = 1
	}
	if cfg.Scheduler.PollIntervalSec == 0 {
		cfg.Scheduler.PollIntervalSec = 2
	}
	if err := normalizeAPITokenPermissions(cfg.APITokenPermissions); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func normalizeAPITokenPermissions(perms []APITokenPermission) error {
	seen := map[string]struct{}{}
	for i := range perms {
		secret := strings.TrimSpace(perms[i].Secret)
		if secret == "" {
			return fmt.Errorf("api_tokens[%d].secret is required", i)
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
	}
	return nil
}

func DefaultRuntime() Runtime {
	var runtime Runtime
	defaultRuntime(&runtime)
	return runtime
}

func defaultRuntime(cfg *Runtime) {
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

func defaultRateLimit(cfg *RateLimit) {
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
