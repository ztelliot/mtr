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
	t.Setenv("MTR_HTTP_PATH_PREFIX", "/env-api/")
	t.Setenv("MTR_TLS_ENABLED", "true")
	t.Setenv("MTR_TLS_CA_FILES", "/env/ca.pem,/env/cloudflare.pem")
	t.Setenv("MTR_HTTP_TLS_ENABLED", "true")
	t.Setenv("MTR_HTTP_TLS_CERT_FILE", "/env/http-cert.pem")
	t.Setenv("MTR_CAPABILITIES", "ping,dns")
	t.Setenv("MTR_PROTOCOLS", "2")
	t.Setenv("MTR_SPEEDTEST_MAX_BYTES", "2048")

	cfg, err := LoadAgent(writeConfig(t, `
id: file-agent
tls:
  cert_file: /file/cert.pem
http_tls:
  key_file: /file/http-key.pem
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "file-agent" {
		t.Fatalf("config file should override env id: %#v", cfg)
	}
	if cfg.Mode != "http" || cfg.HTTPAddr != ":9100" || cfg.HTTPPathPrefix != "/env-api" || cfg.RegisterToken != "env-register-token" || cfg.HTTPToken != "env-http-token" {
		t.Fatalf("env values not loaded: %#v", cfg)
	}
	if !cfg.TLS.Enabled || !reflect.DeepEqual(cfg.TLS.CAFiles, []string{"/env/ca.pem", "/env/cloudflare.pem"}) || cfg.TLS.CertFile != "/file/cert.pem" {
		t.Fatalf("nested env/yaml values not merged: %#v", cfg.TLS)
	}
	if !cfg.HTTPTLS.Enabled || cfg.HTTPTLS.CertFile != "/env/http-cert.pem" || cfg.HTTPTLS.KeyFile != "/file/http-key.pem" {
		t.Fatalf("http tls env/yaml values not merged: %#v", cfg.HTTPTLS)
	}
	if !reflect.DeepEqual(cfg.Capabilities, []string{"ping", "dns"}) {
		t.Fatalf("capabilities = %#v", cfg.Capabilities)
	}
	if cfg.Protocols != 2 || cfg.Speedtest.MaxBytes != 2048 {
		t.Fatalf("numeric env values not loaded: %#v", cfg)
	}
}

func TestLoadAgentNormalizesHTTPPathPrefix(t *testing.T) {
	cfg, err := LoadAgent(writeConfig(t, `http_path_prefix: "api/v1/"`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPPathPrefix != "/api/v1" {
		t.Fatalf("http path prefix = %q", cfg.HTTPPathPrefix)
	}
}

func TestLoadAgentRejectsInvalidHTTPPathPrefix(t *testing.T) {
	if _, err := LoadAgent(writeConfig(t, `http_path_prefix: "/api?token=x"`)); err == nil {
		t.Fatal("expected invalid http path prefix to be rejected")
	}
}

func TestLoadServerUsesEnvAndLetsYAMLOverride(t *testing.T) {
	t.Setenv("MTR_HTTP_ADDR", ":9999")
	t.Setenv("MTR_GRPC_ADDR", ":9998")
	t.Setenv("MTR_TLS_ENABLED", "true")
	t.Setenv("MTR_TLS_CA_FILES", "/env/ca.pem,/env/cloudflare.pem")
	t.Setenv("MTR_TRUSTED_PROXIES", "127.0.0.1,10.0.0.0/8")
	t.Setenv("MTR_CLIENT_IP_HEADERS", "Eo-Connecting-Ip,X-Client-IP-Secret")

	cfg, err := LoadServer(writeConfig(t, `
http_addr: ":file"
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
	if !reflect.DeepEqual(cfg.TrustedProxies, []string{"127.0.0.1", "10.0.0.0/8"}) {
		t.Fatalf("trusted proxies env not loaded: %#v", cfg.TrustedProxies)
	}
	if !reflect.DeepEqual(cfg.ClientIPHeaders, []string{"Eo-Connecting-Ip", "X-Client-IP-Secret"}) {
		t.Fatalf("client ip headers env not loaded: %#v", cfg.ClientIPHeaders)
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

func TestNormalizeManagedSettingsGeneratesAndRejectsDuplicateTokens(t *testing.T) {
	settings := DefaultManagedSettings()
	settings.APITokens = []APITokenPermission{{Tools: map[string]APIToolScope{"ping": {}}}}
	settings.RegisterTokens = []RegisterToken{{Name: "edge"}}
	if err := NormalizeManagedSettings(&settings); err != nil {
		t.Fatalf("expected missing secrets to be generated: %v", err)
	}
	if settings.APITokens[0].ID == "" || settings.APITokens[0].Secret == "" || settings.RegisterTokens[0].ID == "" || settings.RegisterTokens[0].Token == "" || settings.RegisterTokens[0].Name != "edge" {
		t.Fatalf("expected generated tokens: %#v", settings)
	}
	settings = DefaultManagedSettings()
	settings.APITokens = []APITokenPermission{{Secret: "same", All: true}, {Secret: "same", All: true}}
	if err := NormalizeManagedSettings(&settings); err == nil {
		t.Fatal("expected duplicate secret to be rejected")
	}
	settings = DefaultManagedSettings()
	settings.RegisterTokens = []RegisterToken{{ID: "same", Token: "one"}, {ID: "same", Token: "two"}}
	if err := NormalizeManagedSettings(&settings); err == nil {
		t.Fatal("expected duplicate register token id to be rejected")
	}
}

func TestNormalizeManagedSettingsNormalizesLabelConfigs(t *testing.T) {
	settings := DefaultManagedSettings()
	settings.LabelConfigs = map[string]LabelConfig{
		" edge ": {
			Runtime:   &Runtime{Count: 2},
			Scheduler: &Scheduler{MaxInflightPerAgent: 1},
			ToolPolicies: map[string]Policy{
				"ping": {Enabled: true, AllowedArgs: map[string]string{"protocol": "tcp"}},
			},
		},
		"": {Runtime: &Runtime{Count: 1}},
	}
	if err := NormalizeManagedSettings(&settings); err != nil {
		t.Fatal(err)
	}
	cfg, ok := settings.LabelConfigs["edge"]
	if !ok || len(settings.LabelConfigs) != 2 {
		t.Fatalf("label configs not normalized: %#v", settings.LabelConfigs)
	}
	if settings.LabelConfigs[AgentAllLabel].Runtime == nil || settings.LabelConfigs[AgentAllLabel].Scheduler == nil {
		t.Fatalf("agent label defaults not ensured: %#v", settings.LabelConfigs[AgentAllLabel])
	}
	if cfg.Runtime == nil || cfg.Runtime.Count != 2 || cfg.Runtime.MaxHops == 0 {
		t.Fatalf("label runtime not defaulted: %#v", cfg.Runtime)
	}
	if cfg.Scheduler == nil || cfg.Scheduler.MaxInflightPerAgent != 1 || cfg.Scheduler.PollIntervalSec == 0 {
		t.Fatalf("label scheduler not defaulted: %#v", cfg.Scheduler)
	}
	if cfg.ToolPolicies["ping"].AllowedArgs["protocol"] != "tcp" {
		t.Fatalf("label policy not preserved: %#v", cfg.ToolPolicies)
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
