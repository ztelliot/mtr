package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
	"github.com/ztelliot/mtr/internal/policy"
	"golang.org/x/net/http/httpguts"
)

func prepareAgentRegistration(settings *config.ManagedSettings, current *model.Agent, next model.Agent, suppliedToken string) (model.Agent, bool, error) {
	suppliedToken = strings.TrimSpace(suppliedToken)
	var matched *config.RegisterToken
	for i := range settings.RegisterTokens {
		if suppliedToken != "" && settings.RegisterTokens[i].Token == suppliedToken {
			matched = &settings.RegisterTokens[i]
			break
		}
	}
	if matched == nil {
		return model.Agent{}, false, ErrInvalidRegisterToken
	}

	keyHash := registrationTokenHash(matched.ID)
	if current != nil {
		if current.TokenHash != keyHash {
			return model.Agent{}, false, ErrInvalidRegisterToken
		}
		next.Country = current.Country
		next.Region = current.Region
		next.Provider = current.Provider
		next.ISP = current.ISP
		next.CreatedAt = current.CreatedAt
		next.TokenHash = keyHash
		return next, false, nil
	}

	if len(matched.AllowedAgentIDs) > 0 && !containsExactString(matched.AllowedAgentIDs, next.ID) {
		return model.Agent{}, false, ErrAgentIDNotAllowed
	}
	if matched.RemainingUses <= 0 {
		return model.Agent{}, false, ErrRegisterTokenExhausted
	}
	matched.RemainingUses--
	next.TokenHash = keyHash
	return next, true, nil
}

func containsExactString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func registrationTokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func preserveAgentRegistrationMetadata(current, next model.Agent) model.Agent {
	// Managed outbound agents may be inserted before their first successful health
	// probe. Allow each still-empty field to be initialized once, then lock it.
	if current.Country != "" {
		next.Country = current.Country
	}
	if current.Region != "" {
		next.Region = current.Region
	}
	if current.Provider != "" {
		next.Provider = current.Provider
	}
	if current.ISP != "" {
		next.ISP = current.ISP
	}
	if current.TokenHash != "" {
		next.TokenHash = current.TokenHash
	}
	next.CreatedAt = current.CreatedAt
	return next
}

func normalizeOutboundAgent(node *config.OutboundAgent) error {
	if err := ValidateAgentID(node.ID); err != nil {
		return fmt.Errorf("outbound agent id: %w", err)
	}
	if node.Transport != strings.TrimSpace(node.Transport) {
		return fmt.Errorf("outbound agent transport must not have surrounding whitespace")
	}
	if node.URL != strings.TrimSpace(node.URL) {
		return fmt.Errorf("outbound agent url must not have surrounding whitespace")
	}
	if node.Auth.Type != strings.TrimSpace(node.Auth.Type) {
		return fmt.Errorf("outbound agent auth type must not have surrounding whitespace")
	}
	if node.Auth.Header != strings.TrimSpace(node.Auth.Header) {
		return fmt.Errorf("outbound agent auth header must not have surrounding whitespace")
	}
	if node.Auth.AccessKey != strings.TrimSpace(node.Auth.AccessKey) {
		return fmt.Errorf("outbound agent auth access_key must not have surrounding whitespace")
	}
	if node.Auth.ProjectID != strings.TrimSpace(node.Auth.ProjectID) {
		return fmt.Errorf("outbound agent auth project_id must not have surrounding whitespace")
	}
	if node.Transport != config.OutboundTransportHTTP && node.Transport != config.OutboundTransportEvent {
		return fmt.Errorf("outbound agent transport must be http or event")
	}
	if err := validateOutboundAgentURL(node.Transport, node.URL); err != nil {
		return err
	}
	if err := normalizeOutboundAgentAuth(&node.Auth); err != nil {
		return err
	}
	parsedURL, err := url.Parse(node.URL)
	if err != nil {
		return fmt.Errorf("parse outbound agent url: %w", err)
	}
	if err := validateOutboundAgentTLS(node.TLS, parsedURL.Scheme); err != nil {
		return err
	}
	headers, err := normalizeOutboundAgentHeaders(node.Headers)
	if err != nil {
		return err
	}
	node.Headers = headers
	switch node.Transport {
	case config.OutboundTransportHTTP:
		if strings.TrimSpace(node.EventResponse) != "" {
			return fmt.Errorf("event_response is only valid for event transport")
		}
		node.EventResponse = ""
	case config.OutboundTransportEvent:
		if node.EventResponse != strings.TrimSpace(node.EventResponse) {
			return fmt.Errorf("event_response must not have surrounding whitespace")
		}
		if node.EventResponse != config.EventResponseDirect && node.EventResponse != config.EventResponseResultField {
			return fmt.Errorf("event transport event_response must be direct or result_field")
		}
	}
	labels, err := validatedAgentLabels(node.Labels)
	if err != nil {
		return fmt.Errorf("outbound agent labels: %w", err)
	}
	node.Labels = labels
	return nil
}

// ValidateOutboundAgent checks the persisted outbound-agent contract without
// mutating the caller's value. Stores validate again while writing so callers
// can safely distinguish request validation from storage failures.
func ValidateOutboundAgent(node config.OutboundAgent) error {
	return normalizeOutboundAgent(&node)
}

func validateOutboundAgentTLS(tlsConfig config.TLS, scheme string) error {
	if tlsConfig.ServerName != "" && len(tlsConfig.CAFiles) == 0 {
		return fmt.Errorf("outbound agent tls server_name is optional SNI and requires explicit ca_files")
	}
	hasOptions := len(tlsConfig.CAFiles) > 0 || tlsConfig.CertFile != "" || tlsConfig.KeyFile != "" || tlsConfig.ServerName != ""
	if hasOptions && !tlsConfig.Enabled {
		return fmt.Errorf("outbound agent tls options require tls.enabled")
	}
	if tlsConfig.Enabled && scheme != "https" {
		return fmt.Errorf("outbound agent tls requires https")
	}
	if (tlsConfig.CertFile == "") != (tlsConfig.KeyFile == "") {
		return fmt.Errorf("outbound agent tls cert_file and key_file must be configured together")
	}
	for _, path := range tlsConfig.CAFiles {
		if path == "" || path != strings.TrimSpace(path) {
			return fmt.Errorf("outbound agent tls ca_files must contain non-empty paths without surrounding whitespace")
		}
	}
	for name, value := range map[string]string{
		"cert_file":   tlsConfig.CertFile,
		"key_file":    tlsConfig.KeyFile,
		"server_name": tlsConfig.ServerName,
	} {
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("outbound agent tls %s must not contain surrounding whitespace", name)
		}
	}
	return nil
}

func validateOutboundAgentURL(transport string, raw string) error {
	if raw == "" {
		return fmt.Errorf("outbound agent url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("outbound agent url must be an absolute HTTP URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("outbound agent url scheme must be http or https")
	}
	if u.User != nil {
		return fmt.Errorf("outbound agent url must not contain user info")
	}
	if u.Fragment != "" {
		return fmt.Errorf("outbound agent url must not contain a fragment")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("outbound agent url must not contain a query")
	}
	return nil
}

func normalizeOutboundAgentAuth(auth *config.OutboundAgentAuth) error {
	if auth.Secret != strings.TrimSpace(auth.Secret) {
		return fmt.Errorf("outbound agent auth secret must not have surrounding whitespace")
	}
	switch auth.Type {
	case config.OutboundAuthNone:
		if auth.Header != "" || auth.AccessKey != "" || auth.ProjectID != "" || auth.Secret != "" {
			return fmt.Errorf("none auth must not include credential fields")
		}
	case config.OutboundAuthBearer:
		if auth.Header != "" || auth.AccessKey != "" || auth.ProjectID != "" {
			return fmt.Errorf("bearer auth must not include header, access_key, or project_id")
		}
		if auth.Secret == "" {
			return fmt.Errorf("bearer auth secret is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return fmt.Errorf("outbound agent auth secret is not a valid header value")
		}
	case config.OutboundAuthHeader:
		if auth.AccessKey != "" || auth.ProjectID != "" {
			return fmt.Errorf("header auth must not include access_key or project_id")
		}
		if auth.Header == "" {
			return fmt.Errorf("header auth header is required")
		}
		if !httpguts.ValidHeaderFieldName(auth.Header) {
			return fmt.Errorf("outbound agent auth header is invalid")
		}
		canonical := http.CanonicalHeaderKey(auth.Header)
		if config.ReservedHTTPAuthHeader(canonical) {
			return fmt.Errorf("outbound agent auth header %q is reserved", canonical)
		}
		if auth.Secret == "" {
			return fmt.Errorf("header auth secret is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return fmt.Errorf("outbound agent auth secret is not a valid header value")
		}
		auth.Header = canonical
	case config.OutboundAuthHuaweiCloudAKSK:
		if auth.Header != "" {
			return fmt.Errorf("huaweicloud_aksk auth must not include header")
		}
		if auth.AccessKey == "" {
			return fmt.Errorf("huaweicloud_aksk auth access_key is required")
		}
		if !config.ValidHuaweiCloudAccessKey(auth.AccessKey) {
			return fmt.Errorf("huaweicloud_aksk auth access_key is invalid")
		}
		if auth.ProjectID == "" {
			return fmt.Errorf("huaweicloud_aksk auth project_id is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.ProjectID) {
			return fmt.Errorf("outbound agent auth project_id is not a valid header value")
		}
		if auth.Secret == "" {
			return fmt.Errorf("huaweicloud_aksk auth secret is required")
		}
		if !httpguts.ValidHeaderFieldValue(auth.Secret) {
			return fmt.Errorf("outbound agent auth secret is not a valid header value")
		}
	default:
		return fmt.Errorf("outbound agent auth type must be none, bearer, header, or huaweicloud_aksk")
	}
	return nil
}

func normalizeOutboundAgentHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	normalized := make(map[string]string, len(headers))
	for rawName, value := range headers {
		if rawName != strings.TrimSpace(rawName) {
			return nil, fmt.Errorf("outbound agent header names must not have surrounding whitespace")
		}
		name := http.CanonicalHeaderKey(rawName)
		if !httpguts.ValidHeaderFieldName(name) {
			return nil, fmt.Errorf("outbound agent header name is invalid")
		}
		if config.ReservedHTTPAuthHeader(name) || outboundSensitiveHeaderName(name) {
			return nil, fmt.Errorf("outbound agent header %q is reserved", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("outbound agent header %q has an invalid value", name)
		}
		if _, duplicate := normalized[name]; duplicate {
			return nil, fmt.Errorf("duplicate outbound agent header %q", name)
		}
		normalized[name] = value
	}
	return normalized, nil
}

func outboundSensitiveHeaderName(header string) bool {
	header = strings.ToLower(header)
	for _, marker := range []string{"auth", "token", "key", "secret", "signature", "credential", "cookie"} {
		if strings.Contains(header, marker) {
			return true
		}
	}
	return false
}

func normalizeAgentConfig(cfg *config.AgentConfig) error {
	if err := ValidateAgentID(cfg.ID); err != nil {
		return err
	}
	labels, err := validatedAgentLabels(cfg.Labels)
	if err != nil {
		return fmt.Errorf("agent labels: %w", err)
	}
	cfg.Labels = labels
	return nil
}

// ValidateAgentID enforces the canonical identifier shared by gRPC and
// outbound agents. IDs are protocol keys, so silently trimming them can turn a
// preflight check for one key into a write to another.
func ValidateAgentID(id string) error {
	return config.ValidateAgentID(id)
}

// ValidateAgentLabels validates user-managed labels without changing them.
func ValidateAgentLabels(labels []string) error {
	_, err := validatedAgentLabels(labels)
	return err
}

func validatedAgentLabels(in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	for i, value := range in {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, fmt.Errorf("labels[%d] must not be empty", i)
		}
		if value != trimmed {
			return nil, fmt.Errorf("labels[%d] must not have surrounding whitespace", i)
		}
		if model.IsReservedAgentLabel(value) {
			return nil, fmt.Errorf("labels[%d] %q is reserved", i, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("labels contains duplicate value %q", value)
		}
		seen[value] = struct{}{}
	}
	if in == nil {
		return nil, nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out, nil
}

// ValidateAndNormalizeManagedSettings rejects semantically invalid managed
// settings before applying the defaults and generated identifiers provided by
// config.NormalizeManagedSettings. Keeping validation on the store boundary
// prevents direct store callers from persisting settings that the runtime
// would otherwise ignore, clamp, or interpret as unlimited.
func ValidateAndNormalizeManagedSettings(settings *config.ManagedSettings) error {
	if settings == nil {
		return fmt.Errorf("managed settings are required")
	}
	if err := validateManagedRateLimit(settings.RateLimit); err != nil {
		return err
	}
	if err := validateManagedLabelConfigs(settings.LabelConfigs); err != nil {
		return err
	}
	if err := validateManagedAPITokenScopes(settings.APITokens); err != nil {
		return err
	}
	return config.NormalizeManagedSettings(settings)
}

func validateManagedRateLimit(rateLimit config.RateLimit) error {
	if err := validateManagedLimit("rate_limit.global", rateLimit.Global); err != nil {
		return err
	}
	if err := validateManagedLimit("rate_limit.ip", rateLimit.IP); err != nil {
		return err
	}
	if err := validateManagedLimit("rate_limit.cidr", config.LimitSpec{
		RequestsPerMinute: rateLimit.CIDR.RequestsPerMinute,
		Burst:             rateLimit.CIDR.Burst,
	}); err != nil {
		return err
	}
	if rateLimit.CIDR.IPv4Prefix < 0 || rateLimit.CIDR.IPv4Prefix > 32 {
		return fmt.Errorf("rate_limit.cidr.ipv4_prefix must be 0 or between 1 and 32")
	}
	if rateLimit.CIDR.IPv6Prefix < 0 || rateLimit.CIDR.IPv6Prefix > 128 {
		return fmt.Errorf("rate_limit.cidr.ipv6_prefix must be 0 or between 1 and 128")
	}
	if err := validateManagedLimit("rate_limit.geoip", rateLimit.GeoIP); err != nil {
		return err
	}
	for _, name := range sortedMapKeys(rateLimit.Tools) {
		tool, err := validateManagedTool("rate_limit.tools", name)
		if err != nil {
			return err
		}
		limits := rateLimit.Tools[name]
		path := fmt.Sprintf("rate_limit.tools[%q]", tool)
		if err := validateManagedLimit(path+".global", limits.Global); err != nil {
			return err
		}
		if err := validateManagedLimit(path+".cidr", limits.CIDR); err != nil {
			return err
		}
		if err := validateManagedLimit(path+".ip", limits.IP); err != nil {
			return err
		}
	}
	return nil
}

func validateManagedLimit(path string, limit config.LimitSpec) error {
	if limit.RequestsPerMinute < 0 {
		return fmt.Errorf("%s.requests_per_minute must be non-negative", path)
	}
	if limit.Burst < 0 {
		return fmt.Errorf("%s.burst must be non-negative", path)
	}
	return nil
}

func validateManagedLabelConfigs(configs map[string]config.LabelConfig) error {
	labels := sortedMapKeys(configs)
	seen := make(map[string]string, len(labels))
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			return fmt.Errorf("label_configs contains an empty label")
		}
		if previous, ok := seen[trimmed]; ok {
			return fmt.Errorf("label_configs labels %q and %q are duplicates after trimming", previous, label)
		}
		seen[trimmed] = label
	}
	for _, label := range labels {
		if label != strings.TrimSpace(label) {
			return fmt.Errorf("label_configs label %q must not have surrounding whitespace", label)
		}
		cfg := configs[label]
		for _, name := range sortedMapKeys(cfg.ToolPolicies) {
			tool, err := validateManagedTool(fmt.Sprintf("label_configs[%q].tool_policies", label), name)
			if err != nil {
				return err
			}
			if err := validateManagedAllowedArgs(
				fmt.Sprintf("label_configs[%q].tool_policies[%q].allowed_args", label, tool),
				tool,
				cfg.ToolPolicies[name].AllowedArgs,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateManagedAPITokenScopes(tokens []config.APITokenPermission) error {
	for i, token := range tokens {
		if err := validateManagedAgentIDList(fmt.Sprintf("api_tokens[%d].agents", i), token.Agents, true); err != nil {
			return err
		}
		if err := validateManagedAgentIDList(fmt.Sprintf("api_tokens[%d].denied_agents", i), token.DeniedAgents, false); err != nil {
			return err
		}
		if err := validateManagedLabelList(fmt.Sprintf("api_tokens[%d].agent_tags", i), token.AgentTags); err != nil {
			return err
		}
		if err := validateManagedLabelList(fmt.Sprintf("api_tokens[%d].denied_tags", i), token.DeniedTags); err != nil {
			return err
		}
		for _, name := range sortedMapKeys(token.Tools) {
			tool, err := validateManagedTool(fmt.Sprintf("api_tokens[%d].tools", i), name)
			if err != nil {
				return err
			}
			scope := token.Tools[name]
			if err := validateManagedAllowedArgs(
				fmt.Sprintf("api_tokens[%d].tools[%q].allowed_args", i, tool),
				tool,
				scope.AllowedArgs,
			); err != nil {
				return err
			}
			seenVersions := map[int]struct{}{}
			for _, version := range scope.IPVersions {
				if version != int(model.IPAny) && version != int(model.IPv4) && version != int(model.IPv6) {
					return fmt.Errorf("api_tokens[%d].tools[%q].ip_versions contains unsupported value %d", i, tool, version)
				}
				if _, duplicate := seenVersions[version]; duplicate {
					return fmt.Errorf("api_tokens[%d].tools[%q].ip_versions contains duplicate value %d", i, tool, version)
				}
				seenVersions[version] = struct{}{}
			}
		}
	}
	return nil
}

func validateManagedAgentIDList(path string, agentIDs []string, allowWildcard bool) error {
	seen := make(map[string]struct{}, len(agentIDs))
	for i, agentID := range agentIDs {
		if allowWildcard && agentID == "*" {
			// "*" is the only non-ID value accepted by an allowlist.
		} else if err := ValidateAgentID(agentID); err != nil {
			return fmt.Errorf("%s[%d]: %w", path, i, err)
		}
		if _, duplicate := seen[agentID]; duplicate {
			return fmt.Errorf("%s contains duplicate agent id %q", path, agentID)
		}
		seen[agentID] = struct{}{}
	}
	return nil
}

func validateManagedLabelList(path string, labels []string) error {
	seen := make(map[string]struct{}, len(labels))
	for i, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			return fmt.Errorf("%s[%d] must not be empty", path, i)
		}
		if label != trimmed {
			return fmt.Errorf("%s[%d] must not have surrounding whitespace", path, i)
		}
		if _, duplicate := seen[label]; duplicate {
			return fmt.Errorf("%s contains duplicate label %q", path, label)
		}
		seen[label] = struct{}{}
	}
	return nil
}

func validateManagedTool(path string, name string) (model.Tool, error) {
	tool := model.Tool(name)
	switch tool {
	case model.ToolPing, model.ToolTraceroute, model.ToolMTR, model.ToolHTTP, model.ToolDNS, model.ToolPort:
		return tool, nil
	default:
		return "", fmt.Errorf("%s contains unknown tool %q", path, name)
	}
}

func validateManagedAllowedArgs(path string, tool model.Tool, allowedArgs map[string]string) error {
	for _, name := range sortedMapKeys(allowedArgs) {
		rule := allowedArgs[name]
		values, known := managedAllowedArgValues(tool, name)
		if !known {
			return fmt.Errorf("%s contains unknown argument %q for %s", path, name, tool)
		}
		if rule == "" || rule != strings.TrimSpace(rule) {
			return fmt.Errorf("%s[%q] must be a non-empty canonical rule", path, name)
		}
		if name == "port" {
			if strings.IndexFunc(rule, unicode.IsSpace) >= 0 || policy.ValidatePortRangeRule(rule) != nil {
				return fmt.Errorf("%s[%q] is not a valid port range rule", path, name)
			}
			continue
		}
		if err := validateManagedEnumRule(rule, values); err != nil {
			return fmt.Errorf("%s[%q] %w", path, name, err)
		}
	}
	return nil
}

func managedAllowedArgValues(tool model.Tool, name string) (map[string]struct{}, bool) {
	var values []string
	switch {
	case (tool == model.ToolPing || tool == model.ToolTraceroute || tool == model.ToolMTR) && name == "protocol":
		values = []string{"auto", "icmp", "udp", "tcp"}
	case (tool == model.ToolPing || tool == model.ToolTraceroute || tool == model.ToolMTR || tool == model.ToolPort) && name == "port":
		return nil, true
	case tool == model.ToolHTTP && name == "method":
		values = []string{"GET", "HEAD"}
	case tool == model.ToolHTTP && name == "http_version":
		values = []string{"auto", "3", "2", "1.1"}
	case tool == model.ToolDNS && name == "type":
		values = []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS"}
	default:
		return nil, false
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out, true
}

func validateManagedEnumRule(rule string, allowed map[string]struct{}) error {
	seen := map[string]struct{}{}
	for _, value := range strings.Split(rule, ",") {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("must use non-empty canonical comma-separated values")
		}
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("contains unsupported value %q", value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("contains duplicate value %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func bootstrapManagedSettings(adminToken string) (config.ManagedSettings, error) {
	adminToken = strings.TrimSpace(adminToken)
	if adminToken == "" {
		return config.ManagedSettings{}, ErrBootstrapAdminTokenRequired
	}
	if len(adminToken) > 4096 {
		return config.ManagedSettings{}, fmt.Errorf("bootstrap admin token exceeds 4096 bytes")
	}
	return config.ManagedSettingsWithAdminToken(adminToken), nil
}
