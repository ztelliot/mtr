package store

import (
	"context"
	"strings"
	"testing"

	"github.com/ztelliot/mtr/internal/config"
)

func TestValidateAndNormalizeManagedSettingsAcceptsCanonicalSettings(t *testing.T) {
	settings := config.DefaultManagedSettings()
	settings.LabelConfigs["edge"] = config.LabelConfig{ToolPolicies: map[string]config.Policy{
		"ping":       {Enabled: true, AllowedArgs: map[string]string{"protocol": "auto,icmp,udp,tcp", "port": "53,80-443"}},
		"traceroute": {Enabled: true, AllowedArgs: map[string]string{"protocol": "icmp,udp", "port": "33434-33534"}},
		"mtr":        {Enabled: true, AllowedArgs: map[string]string{"protocol": "tcp", "port": "443"}},
		"http":       {Enabled: true, AllowedArgs: map[string]string{"method": "GET,HEAD", "http_version": "auto,3,2,1.1"}},
		"dns":        {Enabled: true, AllowedArgs: map[string]string{"type": "A,AAAA,CNAME,MX,TXT,NS"}},
		"port":       {Enabled: true, AllowedArgs: map[string]string{"port": "1-65535"}},
	}}
	settings.RateLimit.Tools = map[string]config.ToolLimitSpec{
		"ping": {Global: config.LimitSpec{RequestsPerMinute: 10, Burst: 2}},
	}
	settings.APITokens = []config.APITokenPermission{{
		Secret:       "canonical-token",
		Agents:       []string{"*"},
		DeniedAgents: []string{"edge-blocked"},
		AgentTags:    []string{"edge", "blue"},
		DeniedTags:   []string{"blocked"},
		Tools: map[string]config.APIToolScope{
			"http": {
				AllowedArgs: map[string]string{"method": "HEAD", "http_version": "2"},
				IPVersions:  []int{0, 4, 6},
			},
		},
	}}

	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		t.Fatalf("canonical settings rejected: %v", err)
	}
}

func TestValidateAndNormalizeManagedSettingsRejectsInvalidToolsAndAllowedArgs(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*config.ManagedSettings)
	}{
		{
			name: "unknown label tool",
			want: "unknown tool",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = config.LabelConfig{ToolPolicies: map[string]config.Policy{"speedtest": {Enabled: true}}}
			},
		},
		{
			name: "unknown token tool",
			want: "unknown tool",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", Tools: map[string]config.APIToolScope{"tcping": {}}}}
			},
		},
		{
			name: "unknown rate limit tool",
			want: "unknown tool",
			mutate: func(settings *config.ManagedSettings) {
				settings.RateLimit.Tools = map[string]config.ToolLimitSpec{"speedtest": {}}
			},
		},
		{
			name: "unknown argument",
			want: "unknown argument",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("ping", map[string]string{"timeout": "1"})
			},
		},
		{
			name: "invalid protocol",
			want: "unsupported value",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("mtr", map[string]string{"protocol": "raw"})
			},
		},
		{
			name: "invalid HTTP version alias",
			want: "unsupported value",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("http", map[string]string{"http_version": "h2"})
			},
		},
		{
			name: "invalid HTTP method case",
			want: "unsupported value",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("http", map[string]string{"method": "get"})
			},
		},
		{
			name: "invalid DNS type",
			want: "unsupported value",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("dns", map[string]string{"type": "SOA"})
			},
		},
		{
			name: "invalid port range",
			want: "valid port range",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("port", map[string]string{"port": "0-65536"})
			},
		},
		{
			name: "noncanonical port range",
			want: "valid port range",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("port", map[string]string{"port": "80, 443"})
			},
		},
		{
			name: "empty port range segment",
			want: "valid port range",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("port", map[string]string{"port": "80,,443"})
			},
		},
		{
			name: "port range with leading zero",
			want: "valid port range",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("port", map[string]string{"port": "080"})
			},
		},
		{
			name: "empty rule",
			want: "non-empty canonical rule",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("http", map[string]string{"method": ""})
			},
		},
		{
			name: "duplicate enum value",
			want: "duplicate value",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = labelPolicy("http", map[string]string{"method": "GET,GET"})
			},
		},
		{
			name: "unknown token argument",
			want: "unknown argument",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", Tools: map[string]config.APIToolScope{
					"dns": {AllowedArgs: map[string]string{"protocol": "udp"}},
				}}}
			},
		},
		{
			name: "invalid IP version",
			want: "unsupported value",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", Tools: map[string]config.APIToolScope{
					"ping": {IPVersions: []int{5}},
				}}}
			},
		},
		{
			name: "duplicate IP version",
			want: "duplicate value",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", Tools: map[string]config.APIToolScope{
					"ping": {IPVersions: []int{4, 4}},
				}}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := config.DefaultManagedSettings()
			tt.mutate(&settings)
			if err := ValidateAndNormalizeManagedSettings(&settings); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateAndNormalizeManagedSettingsRejectsNegativeLimitsAndInvalidPrefixes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.ManagedSettings)
	}{
		{name: "global rpm", mutate: func(s *config.ManagedSettings) { s.RateLimit.Global.RequestsPerMinute = -1 }},
		{name: "global burst", mutate: func(s *config.ManagedSettings) { s.RateLimit.Global.Burst = -1 }},
		{name: "IP rpm", mutate: func(s *config.ManagedSettings) { s.RateLimit.IP.RequestsPerMinute = -1 }},
		{name: "IP burst", mutate: func(s *config.ManagedSettings) { s.RateLimit.IP.Burst = -1 }},
		{name: "CIDR rpm", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.RequestsPerMinute = -1 }},
		{name: "CIDR burst", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.Burst = -1 }},
		{name: "GeoIP rpm", mutate: func(s *config.ManagedSettings) { s.RateLimit.GeoIP.RequestsPerMinute = -1 }},
		{name: "GeoIP burst", mutate: func(s *config.ManagedSettings) { s.RateLimit.GeoIP.Burst = -1 }},
		{name: "tool global rpm", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {Global: config.LimitSpec{RequestsPerMinute: -1}}}
		}},
		{name: "tool global burst", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {Global: config.LimitSpec{Burst: -1}}}
		}},
		{name: "tool CIDR rpm", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {CIDR: config.LimitSpec{RequestsPerMinute: -1}}}
		}},
		{name: "tool CIDR burst", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {CIDR: config.LimitSpec{Burst: -1}}}
		}},
		{name: "tool IP rpm", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {IP: config.LimitSpec{RequestsPerMinute: -1}}}
		}},
		{name: "tool IP burst", mutate: func(s *config.ManagedSettings) {
			s.RateLimit.Tools = map[string]config.ToolLimitSpec{"ping": {IP: config.LimitSpec{Burst: -1}}}
		}},
		{name: "negative IPv4 prefix", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.IPv4Prefix = -1 }},
		{name: "large IPv4 prefix", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.IPv4Prefix = 33 }},
		{name: "negative IPv6 prefix", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.IPv6Prefix = -1 }},
		{name: "large IPv6 prefix", mutate: func(s *config.ManagedSettings) { s.RateLimit.CIDR.IPv6Prefix = 129 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := config.DefaultManagedSettings()
			tt.mutate(&settings)
			if err := ValidateAndNormalizeManagedSettings(&settings); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	settings := config.ManagedSettings{RateLimit: config.RateLimit{
		Tools: map[string]config.ToolLimitSpec{"ping": {}},
	}}
	if err := ValidateAndNormalizeManagedSettings(&settings); err != nil {
		t.Fatalf("documented zero defaults rejected: %v", err)
	}
	if settings.RateLimit.CIDR.IPv4Prefix != 32 || settings.RateLimit.CIDR.IPv6Prefix != 128 {
		t.Fatalf("zero CIDR defaults = %d/%d", settings.RateLimit.CIDR.IPv4Prefix, settings.RateLimit.CIDR.IPv6Prefix)
	}
	if got := settings.RateLimit.Tools["ping"]; got != (config.ToolLimitSpec{}) {
		t.Fatalf("zero tool limit should remain disabled, got %#v", got)
	}
}

func TestValidateAndNormalizeManagedSettingsRejectsInvalidLabels(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(*config.ManagedSettings)
	}{
		{
			name: "empty label",
			want: "empty label",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs[" \t"] = config.LabelConfig{}
			},
		},
		{
			name: "surrounding whitespace",
			want: "surrounding whitespace",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs[" edge "] = config.LabelConfig{}
			},
		},
		{
			name: "duplicate after trim",
			want: "duplicates after trimming",
			mutate: func(settings *config.ManagedSettings) {
				settings.LabelConfigs["edge"] = config.LabelConfig{}
				settings.LabelConfigs[" edge "] = config.LabelConfig{}
			},
		},
		{
			name: "empty token label",
			want: "must not be empty",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", AgentTags: []string{" "}}}
			},
		},
		{
			name: "duplicate token label",
			want: "duplicate label",
			mutate: func(settings *config.ManagedSettings) {
				settings.APITokens = []config.APITokenPermission{{Secret: "token", DeniedTags: []string{"edge", "edge"}}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := config.DefaultManagedSettings()
			tt.mutate(&settings)
			if err := ValidateAndNormalizeManagedSettings(&settings); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestValidateAndNormalizeManagedSettingsRejectsInvalidTokenAgentIDs(t *testing.T) {
	tests := []struct {
		name string
		set  func(*config.APITokenPermission)
	}{
		{name: "padded allowed id", set: func(token *config.APITokenPermission) { token.Agents = []string{" edge-1"} }},
		{name: "path allowed id", set: func(token *config.APITokenPermission) { token.Agents = []string{"edge/one"} }},
		{name: "empty denied id", set: func(token *config.APITokenPermission) { token.DeniedAgents = []string{""} }},
		{name: "reserved denied id", set: func(token *config.APITokenPermission) { token.DeniedAgents = []string{".."} }},
		{name: "duplicate allowed id", set: func(token *config.APITokenPermission) { token.Agents = []string{"edge-1", "edge-1"} }},
		{name: "duplicate denied id", set: func(token *config.APITokenPermission) { token.DeniedAgents = []string{"edge-1", "edge-1"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := config.DefaultManagedSettings()
			token := config.APITokenPermission{Secret: "token"}
			tt.set(&token)
			settings.APITokens = []config.APITokenPermission{token}
			if err := ValidateAndNormalizeManagedSettings(&settings); err == nil {
				t.Fatal("expected invalid token Agent ID list to be rejected")
			}
		})
	}
}

func TestValidateAndNormalizeManagedSettingsRejectsInvalidRegisterTokenAgentIDs(t *testing.T) {
	for _, allowed := range [][]string{
		{" "},
		{" edge-1"},
		{"edge one"},
		{"."},
		{"edge/one"},
		{"edge-1", "edge-1"},
	} {
		settings := config.DefaultManagedSettings()
		settings.RegisterTokens = []config.RegisterToken{{
			Token:           "register-token",
			RemainingUses:   1,
			AllowedAgentIDs: allowed,
		}}
		if err := ValidateAndNormalizeManagedSettings(&settings); err == nil {
			t.Fatalf("expected invalid register token Agent IDs %#v to be rejected", allowed)
		}
	}
}

func TestMemoryUpdateManagedSettingsRejectsSemanticFailureWithoutMutation(t *testing.T) {
	st := NewTestMemory()
	before, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	invalid := before
	invalid.RateLimit.Global.RequestsPerMinute = -1
	if _, err := st.UpdateManagedSettings(context.Background(), invalid); err == nil {
		t.Fatal("expected update validation error")
	}
	after, err := st.GetManagedSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.RateLimit.Global != before.RateLimit.Global {
		t.Fatalf("invalid update mutated settings: before=%#v after=%#v", before.RateLimit.Global, after.RateLimit.Global)
	}
}

func TestManagedAgentLabelsRequireCanonicalNonReservedValues(t *testing.T) {
	valid := []string{"edge", "blue"}
	if err := ValidateAgentLabels(valid); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}

	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{name: "empty", labels: []string{""}, want: "must not be empty"},
		{name: "whitespace only", labels: []string{" \t"}, want: "must not be empty"},
		{name: "surrounding whitespace", labels: []string{" edge "}, want: "surrounding whitespace"},
		{name: "duplicate", labels: []string{"edge", "edge"}, want: "duplicate value"},
		{name: "all agents reserved", labels: []string{"agent"}, want: "reserved"},
		{name: "transport reserved", labels: []string{"agent:event"}, want: "reserved"},
		{name: "id reserved", labels: []string{"id:edge"}, want: "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateAgentLabels(tt.labels); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}

			cfg := config.AgentConfig{ID: "edge", Labels: tt.labels}
			if err := normalizeAgentConfig(&cfg); err == nil {
				t.Fatal("invalid gRPC agent labels were accepted")
			}

			node := config.OutboundAgent{
				ID:        "edge",
				Transport: config.OutboundTransportHTTP,
				URL:       "https://agent.example",
				Auth:      config.OutboundAgentAuth{Type: config.OutboundAuthNone},
				Labels:    tt.labels,
			}
			if err := normalizeOutboundAgent(&node); err == nil {
				t.Fatal("invalid outbound agent labels were accepted")
			}
		})
	}
}

func labelPolicy(tool string, allowedArgs map[string]string) config.LabelConfig {
	return config.LabelConfig{ToolPolicies: map[string]config.Policy{
		tool: {Enabled: true, AllowedArgs: allowedArgs},
	}}
}
