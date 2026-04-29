package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadAgentUsesEnvAndLetsYAMLOverride(t *testing.T) {
	t.Setenv("MTR_ID", "env-agent")
	t.Setenv("MTR_MODE", "http")
	t.Setenv("MTR_HTTP_ADDR", ":9100")
	t.Setenv("MTR_REGISTER_TOKEN", "env-register-token")
	t.Setenv("MTR_HTTP_TOKEN", "env-http-token")
	t.Setenv("MTR_TLS_ENABLED", "true")
	t.Setenv("MTR_TLS_CA_FILES", "/env/ca.pem,/env/cloudflare.pem")
	t.Setenv("MTR_CAPABILITIES", "ping,dns")
	t.Setenv("MTR_PROTOCOLS", "2")
	t.Setenv("MTR_SPEEDTEST_MAX_BYTES", "2048")

	cfg, err := LoadAgent(writeConfig(t, `
id: file-agent
tls:
  cert_file: /file/cert.pem
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "file-agent" {
		t.Fatalf("config file should override env id: %#v", cfg)
	}
	if cfg.Mode != "http" || cfg.HTTPAddr != ":9100" || cfg.RegisterToken != "env-register-token" || cfg.HTTPToken != "env-http-token" {
		t.Fatalf("env values not loaded: %#v", cfg)
	}
	if !cfg.TLS.Enabled || !reflect.DeepEqual(cfg.TLS.CAFiles, []string{"/env/ca.pem", "/env/cloudflare.pem"}) || cfg.TLS.CertFile != "/file/cert.pem" {
		t.Fatalf("nested env/yaml values not merged: %#v", cfg.TLS)
	}
	if !reflect.DeepEqual(cfg.Capabilities, []string{"ping", "dns"}) {
		t.Fatalf("capabilities = %#v", cfg.Capabilities)
	}
	if cfg.Protocols != 2 || cfg.Speedtest.MaxBytes != 2048 {
		t.Fatalf("numeric env values not loaded: %#v", cfg)
	}
}

func TestLoadServerUsesEnvAndLetsYAMLOverride(t *testing.T) {
	t.Setenv("MTR_HTTP_ADDR", ":9999")
	t.Setenv("MTR_GRPC_ADDR", ":9998")
	t.Setenv("MTR_TLS_ENABLED", "true")
	t.Setenv("MTR_TLS_CA_FILES", "/env/ca.pem,/env/cloudflare.pem")
	t.Setenv("MTR_RATE_LIMIT_GLOBAL_REQUESTS_PER_MINUTE", "7")
	t.Setenv("MTR_RATE_LIMIT_GLOBAL_BURST", "8")
	t.Setenv("MTR_RUNTIME_HTTP_TIMEOUT_SEC", "9")
	t.Setenv("MTR_OUTBOUND_AGENTS", `[{id: edge-env, base_url: "http://edge", http_token: secret}]`)
	t.Setenv("MTR_TOOL_POLICIES", `{ping: {enabled: true, allowed_args: {protocol: "^(icmp)$"}, hide_first_hops: 2}}`)
	t.Setenv("MTR_API_TOKENS", `[{secret: token-env, schedule_access: read, tools: {http: {allowed_args: {method: "^(HEAD)$"}}}, agents: [edge-1]}]`)

	cfg, err := LoadServer(writeConfig(t, `
http_addr: ":file"
rate_limit:
  global:
    burst: 11
runtime:
  http_timeout_sec: 5
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":file" || cfg.GRPCAddr != ":9998" {
		t.Fatalf("unexpected addresses: %#v", cfg)
	}
	if !cfg.TLS.Enabled || !reflect.DeepEqual(cfg.TLS.CAFiles, []string{"/env/ca.pem", "/env/cloudflare.pem"}) {
		t.Fatalf("nested tls env not loaded: %#v", cfg.TLS)
	}
	if cfg.RateLimit.Global.RequestsPerMinute != 7 || cfg.RateLimit.Global.Burst != 11 {
		t.Fatalf("nested rate limit merge failed: %#v", cfg.RateLimit.Global)
	}
	if cfg.Runtime.HTTPTimeoutSec != 5 {
		t.Fatalf("runtime yaml should override env: %#v", cfg.Runtime)
	}
	if cfg.Runtime.ProbeStepTimeoutSec != 1 {
		t.Fatalf("runtime probe step default = %d, want 1", cfg.Runtime.ProbeStepTimeoutSec)
	}
	if cfg.Scheduler.GRPCMaxInflightPerAgent != 4 || cfg.Scheduler.OutboundMaxInflightPerAgent != 1 {
		t.Fatalf("scheduler inflight defaults = %#v", cfg.Scheduler)
	}
	if len(cfg.OutboundAgents) != 1 || cfg.OutboundAgents[0].ID != "edge-env" || cfg.OutboundAgents[0].BaseURL != "http://edge" || cfg.OutboundAgents[0].HTTPToken != "secret" {
		t.Fatalf("outbound agents env not loaded: %#v", cfg.OutboundAgents)
	}
	pingPolicy := cfg.ToolPolicies["ping"]
	if !pingPolicy.Enabled || pingPolicy.HideFirstHops != 2 || pingPolicy.AllowedArgs["protocol"] != "^(icmp)$" {
		t.Fatalf("tool policies env not loaded: %#v", cfg.ToolPolicies)
	}
	if len(cfg.APITokenPermissions) != 1 || cfg.APITokenPermissions[0].Secret != "token-env" || cfg.APITokenPermissions[0].ScheduleAccess != "read" || cfg.APITokenPermissions[0].Tools["http"].AllowedArgs["method"] != "^(HEAD)$" || !reflect.DeepEqual(cfg.APITokenPermissions[0].Agents, []string{"edge-1"}) {
		t.Fatalf("api token permissions env not loaded: %#v", cfg.APITokenPermissions)
	}
}

func TestLoadServerSchedulerInflightLimits(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, `
scheduler:
  grpc_max_inflight_per_agent: 7
  outbound_max_inflight_per_agent: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scheduler.GRPCMaxInflightPerAgent != 7 || cfg.Scheduler.OutboundMaxInflightPerAgent != 2 {
		t.Fatalf("explicit scheduler inflight limits not loaded: %#v", cfg.Scheduler)
	}
}

func TestLoadAgentAllowsDisablingSpeedtest(t *testing.T) {
	t.Setenv("MTR_SPEEDTEST_MAX_BYTES", "2048")

	cfg, err := LoadAgent(writeConfig(t, `
speedtest:
  max_bytes: 0
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Speedtest.MaxBytes != 0 {
		t.Fatalf("speedtest max_bytes should remain disabled: %#v", cfg.Speedtest)
	}
}

func TestLoadServerYAMLEmptyMapOverridesEnvMap(t *testing.T) {
	t.Setenv("MTR_TOOL_POLICIES", `{ping: {enabled: true}}`)

	cfg, err := LoadServer(writeConfig(t, `tool_policies: {}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ToolPolicies) != 0 {
		t.Fatalf("yaml empty map should override env map: %#v", cfg.ToolPolicies)
	}
}

func TestLoadServerRejectsInvalidAPITokenPermissions(t *testing.T) {
	if _, err := LoadServer(writeConfig(t, `api_tokens: [{tools: {ping: {}}}]`)); err == nil {
		t.Fatal("expected missing secret to be rejected")
	}
	if _, err := LoadServer(writeConfig(t, `
api_tokens:
  - secret: same
    all: true
  - secret: same
    all: true
`)); err == nil {
		t.Fatal("expected duplicate secret to be rejected")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
