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
	t.Setenv("MTR_HTTP_AUTH_TYPE", "header")
	t.Setenv("MTR_HTTP_AUTH_HEADER", "X-Auth-Token")
	t.Setenv("MTR_HTTP_AUTH_SECRET", "env-http-token")
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
	if cfg.Mode != "http" || cfg.HTTPAddr != ":9100" || cfg.HTTPPathPrefix != "/env-api" || cfg.RegisterToken != "env-register-token" || cfg.HTTPAuth.Type != HTTPAuthHeader || cfg.HTTPAuth.Header != "X-Auth-Token" || cfg.HTTPAuth.Secret != "env-http-token" {
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

func TestLoadAgentUsesEnvWithoutConfigFile(t *testing.T) {
	t.Setenv("MTR_ID", "env-agent")
	t.Setenv("MTR_MODE", "http")
	t.Setenv("MTR_HTTP_ADDR", ":9100")
	t.Setenv("MTR_HTTP_AUTH_TYPE", "bearer")
	t.Setenv("MTR_HTTP_AUTH_SECRET", "env-http-token")
	t.Setenv("MTR_HTTP_PATH_PREFIX", "env-api/")
	t.Setenv("MTR_CAPABILITIES", "ping,dns")
	t.Setenv("MTR_PROTOCOLS", "2")
	t.Setenv("MTR_SPEEDTEST_MAX_BYTES", "2048")

	cfg, err := LoadAgent(filepath.Join(t.TempDir(), "missing-agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "env-agent" || cfg.Mode != "http" || cfg.HTTPAddr != ":9100" || cfg.HTTPAuth.Type != HTTPAuthBearer || cfg.HTTPAuth.Secret != "env-http-token" || cfg.HTTPPathPrefix != "/env-api" {
		t.Fatalf("env-only agent config not loaded: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Capabilities, []string{"ping", "dns"}) || cfg.Protocols != 2 || cfg.Speedtest.MaxBytes != 2048 {
		t.Fatalf("env-only agent config fields not loaded: %#v", cfg)
	}
}

func TestLoadAgentNormalizesHTTPPathPrefix(t *testing.T) {
	cfg, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 3\ncapabilities: [ping]\nhttp_path_prefix: \"api/v1/\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPPathPrefix != "/api/v1" {
		t.Fatalf("http path prefix = %q", cfg.HTTPPathPrefix)
	}
}

func TestLoadAgentDefaultsEventModeToPort8000(t *testing.T) {
	cfg, err := LoadAgent(writeConfig(t, "id: test-agent\nmode: event\nprotocols: 3\ncapabilities: [ping]\nhttp_auth:\n  type: none\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8000" {
		t.Fatalf("event http addr = %q", cfg.HTTPAddr)
	}
}

func TestNormalizeAgentHTTPAuthByMode(t *testing.T) {
	valid := []struct {
		name string
		mode string
		auth HTTPAuth
	}{
		{name: "grpc omitted", mode: "grpc"},
		{name: "http bearer", mode: "grpc,http", auth: HTTPAuth{Type: HTTPAuthBearer, Secret: "secret"}},
		{name: "http custom header", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "x-auth-token", Secret: "secret"}},
		{name: "event none", mode: "event", auth: HTTPAuth{Type: HTTPAuthNone}},
		{name: "event bearer", mode: "grpc,event", auth: HTTPAuth{Type: HTTPAuthBearer, Secret: "secret"}},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := NormalizeAgentHTTPAuth(tt.mode, tt.auth)
			if err != nil {
				t.Fatal(err)
			}
			if tt.auth.Type == HTTPAuthHeader && auth.Header != "X-Auth-Token" {
				t.Fatalf("canonical header = %q", auth.Header)
			}
		})
	}

	invalid := []struct {
		name string
		mode string
		auth HTTPAuth
	}{
		{name: "http omitted", mode: "http"},
		{name: "http none", mode: "http", auth: HTTPAuth{Type: HTTPAuthNone}},
		{name: "event omitted", mode: "event"},
		{name: "grpc explicit none", mode: "grpc", auth: HTTPAuth{Type: HTTPAuthNone}},
		{name: "grpc bearer", mode: "grpc", auth: HTTPAuth{Type: HTTPAuthBearer, Secret: "secret"}},
		{name: "none secret", mode: "event", auth: HTTPAuth{Type: HTTPAuthNone, Secret: "secret"}},
		{name: "bearer header", mode: "http", auth: HTTPAuth{Type: HTTPAuthBearer, Header: "X-Auth-Token", Secret: "secret"}},
		{name: "bearer missing secret", mode: "http", auth: HTTPAuth{Type: HTTPAuthBearer}},
		{name: "header missing name", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Secret: "secret"}},
		{name: "header reserved", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "Authorization", Secret: "secret"}},
		{name: "header forwarded", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "Forwarded", Secret: "secret"}},
		{name: "header x-forwarded", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Forwarded-Auth", Secret: "secret"}},
		{name: "header real ip", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Real-IP", Secret: "secret"}},
		{name: "header via", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "Via", Secret: "secret"}},
		{name: "header true client ip", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "True-Client-IP", Secret: "secret"}},
		{name: "header cloudflare client ip", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "CF-Connecting-IP", Secret: "secret"}},
		{name: "header client ip", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Client-IP", Secret: "secret"}},
		{name: "header envoy address", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Envoy-External-Address", Secret: "secret"}},
		{name: "header sdk date", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Sdk-Date", Secret: "secret"}},
		{name: "header sdk content hash", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Sdk-Content-Sha256", Secret: "secret"}},
		{name: "header project id", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Project-ID", Secret: "secret"}},
		{name: "header security token", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X-Security-Token", Secret: "secret"}},
		{name: "header invalid", mode: "http", auth: HTTPAuth{Type: HTTPAuthHeader, Header: "X Auth", Secret: "secret"}},
		{name: "secret whitespace", mode: "event", auth: HTTPAuth{Type: HTTPAuthBearer, Secret: " secret"}},
		{name: "unknown", mode: "event", auth: HTTPAuth{Type: "token", Secret: "secret"}},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NormalizeAgentHTTPAuth(tt.mode, tt.auth); err == nil {
				t.Fatal("expected invalid HTTP auth to fail")
			}
		})
	}
}

func TestValidHuaweiCloudAccessKey(t *testing.T) {
	for _, value := range []string{"AKIDEXAMPLE123456789", "access-key"} {
		if !ValidHuaweiCloudAccessKey(value) {
			t.Fatalf("valid access key %q was rejected", value)
		}
	}
	for _, value := range []string{"", "access key", "access\tkey", "access\u00a0key", "access,key", "access\nkey"} {
		if ValidHuaweiCloudAccessKey(value) {
			t.Fatalf("invalid access key %q was accepted", value)
		}
	}
}

func TestLoadAgentRejectsLegacyHTTPToken(t *testing.T) {
	base := "id: test-agent\nmode: event\nprotocols: 3\ncapabilities: [ping]\nhttp_auth:\n  type: none\n"
	if _, err := LoadAgent(writeConfig(t, base+"http_token: legacy\n")); err == nil {
		t.Fatal("legacy http_token field was accepted")
	}
	t.Setenv("MTR_HTTP_TOKEN", "legacy")
	if _, err := LoadAgent(writeConfig(t, base)); err == nil {
		t.Fatal("legacy MTR_HTTP_TOKEN was accepted")
	}
}

func TestLoadAgentRejectsInvalidHTTPPathPrefix(t *testing.T) {
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 3\ncapabilities: [ping]\nhttp_path_prefix: \"/api?token=x\"\n")); err == nil {
		t.Fatal("expected invalid http path prefix to be rejected")
	}
}

func TestLoadAgentRejectsNonCanonicalOrDuplicateCapabilities(t *testing.T) {
	for _, capabilities := range []string{"[PING]", `[" ping "]`, "[ping, ping]"} {
		if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 3\ncapabilities: "+capabilities+"\n")); err == nil {
			t.Fatalf("capabilities %s were accepted", capabilities)
		}
	}
}

func TestLoadAgentRejectsUnknownCapabilitiesAndProtocolBits(t *testing.T) {
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 3\ncapabilities: [ping, shell]\n")); err == nil {
		t.Fatal("expected unknown agent capability to be rejected")
	}
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 4\ncapabilities: [ping]\n")); err == nil {
		t.Fatal("expected unknown agent protocol bits to be rejected")
	}
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 0\ncapabilities: [ping]\n")); err == nil {
		t.Fatal("expected empty agent protocols to be rejected")
	}
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 3\ncapabilities: []\n")); err == nil {
		t.Fatal("expected empty agent capabilities to be rejected")
	}
}

func TestLoadAgentRequiresCanonicalExplicitID(t *testing.T) {
	for _, idLine := range []string{"", "id: .\n", "id: ..\n", "id: edge/one\n", "id: _edge\n"} {
		if _, err := LoadAgent(writeConfig(t, idLine+"protocols: 3\ncapabilities: [ping]\n")); err == nil {
			t.Fatalf("agent id config %q was accepted", idLine)
		}
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

func TestLoadServerUsesEnvWithoutConfigFile(t *testing.T) {
	t.Setenv("MTR_HTTP_ADDR", ":9999")
	t.Setenv("MTR_GRPC_ADDR", ":9998")
	t.Setenv("MTR_DATABASE_URL", "sqlite:/tmp/mtr.db")
	t.Setenv("MTR_TLS_ENABLED", "true")
	t.Setenv("MTR_TLS_CA_FILES", "/env/ca.pem,/env/cloudflare.pem")
	t.Setenv("MTR_TRUSTED_PROXIES", "127.0.0.1,10.0.0.0/8")
	t.Setenv("MTR_CLIENT_IP_HEADERS", "Eo-Connecting-Ip,X-Client-IP-Secret")

	cfg, err := LoadServer(filepath.Join(t.TempDir(), "missing-server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":9999" || cfg.GRPCAddr != ":9998" || cfg.DatabaseURL != "sqlite:/tmp/mtr.db" {
		t.Fatalf("env-only server addresses not loaded: %#v", cfg)
	}
	if !cfg.TLS.Enabled || !reflect.DeepEqual(cfg.TLS.CAFiles, []string{"/env/ca.pem", "/env/cloudflare.pem"}) {
		t.Fatalf("env-only server tls not loaded: %#v", cfg.TLS)
	}
	if !reflect.DeepEqual(cfg.TrustedProxies, []string{"127.0.0.1", "10.0.0.0/8"}) || !reflect.DeepEqual(cfg.ClientIPHeaders, []string{"Eo-Connecting-Ip", "X-Client-IP-Secret"}) {
		t.Fatalf("env-only server lists not loaded: %#v", cfg)
	}
}

func TestConfigRejectsUnknownYAMLFields(t *testing.T) {
	if _, err := LoadServer(writeConfig(t, "tls:\n  enabledd: true\n")); err == nil {
		t.Fatal("expected unknown nested server field to be rejected")
	}
	if _, err := LoadAgent(writeConfig(t, "id: test-agent\nprotocols: 1\ncapabilities: [ping]\nlegacy_mode: true\n")); err == nil {
		t.Fatal("expected unknown agent field to be rejected")
	}
}

func TestConfigRejectsUnknownStructuredEnvironmentFields(t *testing.T) {
	t.Setenv("MTR_TLS", "{enabledd: true}")
	if _, err := LoadServer(filepath.Join(t.TempDir(), "missing-server.yaml")); err == nil {
		t.Fatal("expected unknown structured environment field to be rejected")
	}
}

func TestConfigRejectsUnknownEnvironmentVariables(t *testing.T) {
	t.Setenv("MTR_TLS_ENABLEDD", "true")
	if _, err := LoadServer(filepath.Join(t.TempDir(), "missing-server.yaml")); err == nil {
		t.Fatal("expected unknown server environment variable to be rejected")
	}
}

func TestLoadAgentAllowsDisablingSpeedtest(t *testing.T) {
	t.Setenv("MTR_SPEEDTEST_MAX_BYTES", "2048")

	cfg, err := LoadAgent(writeConfig(t, `
id: test-agent
protocols: 3
capabilities: [ping]
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
	settings.RegisterTokens = []RegisterToken{{Name: "edge", AllowedAgentIDs: []string{"edge-1", "edge-2"}}}
	if err := NormalizeManagedSettings(&settings); err != nil {
		t.Fatalf("expected missing secrets to be generated: %v", err)
	}
	if settings.APITokens[0].ID == "" || settings.APITokens[0].Secret == "" || settings.RegisterTokens[0].ID == "" || settings.RegisterTokens[0].Token == "" || settings.RegisterTokens[0].Name != "edge" {
		t.Fatalf("expected generated tokens: %#v", settings)
	}
	if got := settings.RegisterTokens[0].AllowedAgentIDs; len(got) != 2 || got[0] != "edge-1" || got[1] != "edge-2" {
		t.Fatalf("expected canonical allowed agent ids: %#v", got)
	}
	for _, allowed := range [][]string{{" "}, {" edge-1"}, {"edge one"}, {"."}, {"edge/one"}, {"edge-1", "edge-1"}} {
		settings = DefaultManagedSettings()
		settings.RegisterTokens = []RegisterToken{{Token: "one", RemainingUses: 1, AllowedAgentIDs: allowed}}
		if err := NormalizeManagedSettings(&settings); err == nil {
			t.Fatalf("expected invalid register token allowed agent ids %#v to be rejected", allowed)
		}
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
	settings = DefaultManagedSettings()
	settings.RegisterTokens = []RegisterToken{{Token: "one", RemainingUses: -1}}
	if err := NormalizeManagedSettings(&settings); err == nil {
		t.Fatal("expected negative register token remaining uses to be rejected")
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

func TestNormalizeManagedSettingsRejectsRuntimeOutsideEffectiveBounds(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*LabelConfig)
	}{
		{name: "count", configure: func(cfg *LabelConfig) { cfg.Runtime.Count = 11 }},
		{name: "max hops", configure: func(cfg *LabelConfig) { cfg.Runtime.MaxHops = 31 }},
		{name: "probe timeout", configure: func(cfg *LabelConfig) { cfg.Runtime.ProbeStepTimeoutSec = 2 }},
		{name: "tool timeout", configure: func(cfg *LabelConfig) { cfg.Runtime.MaxToolTimeoutSec = 301 }},
		{name: "http timeout", configure: func(cfg *LabelConfig) { cfg.Runtime.HTTPTimeoutSec = 6 }},
		{name: "dns timeout", configure: func(cfg *LabelConfig) { cfg.Runtime.DNSTimeoutSec = 6 }},
		{name: "resolve timeout", configure: func(cfg *LabelConfig) { cfg.Runtime.ResolveTimeoutSec = 4 }},
		{name: "invoke attempts", configure: func(cfg *LabelConfig) { cfg.Runtime.OutboundInvokeAttempts = 4 }},
		{name: "health interval", configure: func(cfg *LabelConfig) { cfg.Runtime.OutboundMaxHealthIntervalSec = 301 }},
		{name: "offline interval", configure: func(cfg *LabelConfig) { cfg.Scheduler.AgentOfflineAfterSec = 91 }},
		{name: "max inflight", configure: func(cfg *LabelConfig) { cfg.Scheduler.MaxInflightPerAgent = 5 }},
		{name: "poll interval", configure: func(cfg *LabelConfig) { cfg.Scheduler.PollIntervalSec = 3 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := DefaultRuntime()
			var scheduler Scheduler
			DefaultScheduler(&scheduler)
			cfg := LabelConfig{Runtime: &runtime, Scheduler: &scheduler}
			tt.configure(&cfg)
			settings := ManagedSettings{LabelConfigs: map[string]LabelConfig{"edge": cfg}}
			if err := NormalizeManagedSettings(&settings); err == nil {
				t.Fatal("expected out-of-range managed setting to be rejected")
			}
		})
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

func TestLoadAgentEmptyOrNullYAMLDoesNotClearEnvironment(t *testing.T) {
	for name, body := range map[string]string{"empty": "", "null": "null\n"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("MTR_MODE", "event")
			t.Setenv("MTR_ID", "env-agent")
			t.Setenv("MTR_PROTOCOLS", "3")
			t.Setenv("MTR_CAPABILITIES", "ping")
			t.Setenv("MTR_HTTP_AUTH_TYPE", "none")
			cfg, err := LoadAgent(writeConfig(t, body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Mode != "event" || cfg.ID != "env-agent" {
				t.Fatalf("environment was cleared by %s YAML: %#v", name, cfg)
			}
		})
	}
}

func TestLoadServerBootstrapAdminTokenFileFromEnvironment(t *testing.T) {
	t.Setenv("MTR_BOOTSTRAP_ADMIN_TOKEN_FILE", "/run/secrets/mtr-admin-token")
	cfg, err := LoadServer(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BootstrapAdminTokenFile != "/run/secrets/mtr-admin-token" {
		t.Fatalf("bootstrap token file = %q", cfg.BootstrapAdminTokenFile)
	}
}
