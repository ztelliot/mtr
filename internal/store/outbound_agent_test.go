package store

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ztelliot/mtr/internal/config"
)

func TestNormalizeOutboundAgentRequiresExplicitCanonicalFields(t *testing.T) {
	tests := []struct {
		name string
		node config.OutboundAgent
	}{
		{name: "missing transport", node: config.OutboundAgent{ID: "edge", URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}}},
		{name: "missing auth", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example"}},
		{name: "event response on http", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect}},
		{name: "missing event response", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}}},
		{name: "url query", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example?key=secret", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect}},
		{name: "url userinfo", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://user:pass@agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect}},
		{name: "url fragment", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example/#fragment", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: config.EventResponseDirect}},
		{name: "padded id", node: config.OutboundAgent{ID: " edge ", Transport: config.OutboundTransportHTTP, URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}}},
		{name: "uppercase transport", node: config.OutboundAgent{ID: "edge", Transport: "HTTP", URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}}},
		{name: "padded url", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportHTTP, URL: " https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}}},
		{name: "uppercase auth", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: "NONE"}}},
		{name: "padded event response", node: config.OutboundAgent{ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example", Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, EventResponse: " direct"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := normalizeOutboundAgent(&test.node); err == nil {
				t.Fatal("expected invalid outbound agent to fail")
			}
		})
	}
}

func TestValidateAgentIDRejectsNonCanonicalKeys(t *testing.T) {
	for _, id := range []string{
		"", ".", "..", "/", "edge/one", "_edge", "-edge", "edge:grpc",
		" edge", "edge ", "edge one", "edge\nother", "é", strings.Repeat("a", 129),
	} {
		if err := ValidateAgentID(id); err == nil {
			t.Fatalf("ValidateAgentID(%q) succeeded", id)
		}
	}
	for _, id := range []string{"edge-1.example_grpc", "A", strings.Repeat("a", 128)} {
		if err := ValidateAgentID(id); err != nil {
			t.Fatalf("canonical id %q rejected: %v", id, err)
		}
	}
}

func TestNormalizeOutboundAgentAllowsPreconfiguredCredentialsAndHeadersOverHTTP(t *testing.T) {
	for name, node := range map[string]config.OutboundAgent{
		"bearer": {
			ID: "edge", Transport: config.OutboundTransportHTTP, URL: "http://agent.example",
			Auth: config.OutboundAgentAuth{Type: config.OutboundAuthBearer, Secret: "secret"},
		},
		"custom auth": {
			ID: "edge", Transport: config.OutboundTransportEvent, URL: "http://agent.example",
			Auth: config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "X-Auth-Token", Secret: "secret"}, EventResponse: config.EventResponseDirect,
		},
		"headers": {
			ID: "edge", Transport: config.OutboundTransportHTTP, URL: "http://agent.example",
			Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, Headers: map[string]string{"X-CFF-Request-Version": "v1"},
		},
		"huaweicloud aksk": {
			ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://functiongraph.example/invoke",
			Auth: config.OutboundAgentAuth{
				Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "test-access-key",
				ProjectID: "test-project-id", Secret: "test-secret-key",
			},
			EventResponse: config.EventResponseResultField,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := normalizeOutboundAgent(&node); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizeOutboundAgentValidatesHuaweiCloudAKSKCredentials(t *testing.T) {
	valid := config.OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, URL: "https://functiongraph.example/invoke",
		Auth: config.OutboundAgentAuth{
			Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "test-access-key",
			ProjectID: "test-project-id", Secret: "test-secret-key",
		},
		EventResponse: config.EventResponseResultField,
	}
	if err := normalizeOutboundAgent(&valid); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*config.OutboundAgent){
		"header":             func(node *config.OutboundAgent) { node.Auth.Header = "X-Auth-Token" },
		"missing access key": func(node *config.OutboundAgent) { node.Auth.AccessKey = "" },
		"padded access key":  func(node *config.OutboundAgent) { node.Auth.AccessKey = " access-key" },
		"invalid access key": func(node *config.OutboundAgent) { node.Auth.AccessKey = "access\nkey" },
		"spaced access key":  func(node *config.OutboundAgent) { node.Auth.AccessKey = "access key" },
		"tabbed access key":  func(node *config.OutboundAgent) { node.Auth.AccessKey = "access\tkey" },
		"comma access key":   func(node *config.OutboundAgent) { node.Auth.AccessKey = "access,key" },
		"missing project id": func(node *config.OutboundAgent) { node.Auth.ProjectID = "" },
		"padded project id":  func(node *config.OutboundAgent) { node.Auth.ProjectID = "project-id " },
		"invalid project id": func(node *config.OutboundAgent) { node.Auth.ProjectID = "project\nid" },
		"missing secret key": func(node *config.OutboundAgent) { node.Auth.Secret = "" },
		"padded secret key":  func(node *config.OutboundAgent) { node.Auth.Secret = " secret-key" },
		"invalid secret key": func(node *config.OutboundAgent) { node.Auth.Secret = "secret\nkey" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			node := valid
			mutate(&node)
			if err := normalizeOutboundAgent(&node); err == nil {
				t.Fatal("expected invalid Huawei Cloud AK/SK credentials to fail")
			}
		})
	}
}

func TestNormalizeOutboundAgentRejectsHuaweiCloudFieldsForOtherAuthTypes(t *testing.T) {
	tests := map[string]config.OutboundAgentAuth{
		"none access key":   {Type: config.OutboundAuthNone, AccessKey: "access-key"},
		"none project id":   {Type: config.OutboundAuthNone, ProjectID: "project-id"},
		"bearer access key": {Type: config.OutboundAuthBearer, AccessKey: "access-key", Secret: "token"},
		"bearer project id": {Type: config.OutboundAuthBearer, ProjectID: "project-id", Secret: "token"},
		"header access key": {Type: config.OutboundAuthHeader, Header: "X-Auth-Token", AccessKey: "access-key", Secret: "token"},
		"header project id": {Type: config.OutboundAuthHeader, Header: "X-Auth-Token", ProjectID: "project-id", Secret: "token"},
	}
	for name, auth := range tests {
		t.Run(name, func(t *testing.T) {
			node := config.OutboundAgent{
				ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example", Auth: auth,
			}
			if err := normalizeOutboundAgent(&node); err == nil {
				t.Fatal("expected auth-specific fields to be rejected")
			}
		})
	}
}

func TestNormalizeOutboundAgentStillRequiresHTTPSWhenTLSIsEnabled(t *testing.T) {
	node := config.OutboundAgent{
		ID: "edge", Transport: config.OutboundTransportHTTP, URL: "http://agent.example",
		Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, TLS: config.TLS{Enabled: true},
	}
	if err := normalizeOutboundAgent(&node); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeOutboundAgentRejectsProxyIdentityAuthHeaders(t *testing.T) {
	for _, header := range []string{
		"Forwarded", "X-Forwarded-For", "X-Real-IP", "Via", "True-Client-IP",
		"CF-Connecting-IP", "X-Client-IP", "X-Envoy-External-Address",
	} {
		node := config.OutboundAgent{
			ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example",
			Auth: config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: header, Secret: "secret"},
		}
		if err := normalizeOutboundAgent(&node); err == nil {
			t.Fatalf("proxy identity auth header %q was accepted", header)
		}
	}
}

func TestNormalizeOutboundAgentRejectsInertOrIncompleteTLSOptions(t *testing.T) {
	for name, tlsConfig := range map[string]config.TLS{
		"disabled with server name": {ServerName: "agent.example"},
		"disabled with ca":          {CAFiles: []string{"/ca.pem"}},
		"cert without key":          {Enabled: true, CertFile: "/client.crt"},
		"key without cert":          {Enabled: true, KeyFile: "/client.key"},
		"empty ca path":             {Enabled: true, CAFiles: []string{""}},
		"SNI without explicit CA":   {Enabled: true, ServerName: "agent.example"},
		"padded server name":        {Enabled: true, ServerName: " agent.example"},
	} {
		t.Run(name, func(t *testing.T) {
			node := config.OutboundAgent{
				ID: "edge", Transport: config.OutboundTransportHTTP, URL: "https://agent.example",
				Auth: config.OutboundAgentAuth{Type: config.OutboundAuthNone}, TLS: tlsConfig,
			}
			if err := normalizeOutboundAgent(&node); err == nil {
				t.Fatal("expected invalid TLS configuration to fail")
			}
		})
	}
}

func TestValidateOutboundAgentTLSRequiresExplicitCAForSNIRegardlessOfEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		err := validateOutboundAgentTLS(config.TLS{Enabled: enabled, ServerName: "agent.internal"}, "https")
		if err == nil || !strings.Contains(err.Error(), "requires explicit ca_files") {
			t.Fatalf("enabled=%t: error = %v", enabled, err)
		}
	}
}

func TestNormalizeOutboundAgentValidatesNonSecretHeaders(t *testing.T) {
	valid := config.OutboundAgent{
		ID: "edge", Transport: config.OutboundTransportEvent, URL: "https://agent.example/invoke",
		Headers:       map[string]string{"x-cff-request-version": "v1"},
		Auth:          config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "X-Auth-Token", Secret: "secret"},
		EventResponse: config.EventResponseResultField,
	}
	if err := normalizeOutboundAgent(&valid); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(valid.Headers, map[string]string{"X-Cff-Request-Version": "v1"}) || valid.Auth.Header != "X-Auth-Token" {
		t.Fatalf("normalized = %#v", valid)
	}
	for name, headers := range map[string]map[string]string{
		"reserved":               {"Content-Type": "text/plain"},
		"authorization":          {"Authorization": "Bearer secret"},
		"sdk date":               {"X-Sdk-Date": "20260809T000000Z"},
		"sdk content hash":       {"X-Sdk-Content-Sha256": "hash"},
		"project id":             {"X-Project-ID": "project"},
		"security token":         {"X-Security-Token": "token"},
		"proxy identity":         {"X-Forwarded-For": "203.0.113.10"},
		"credential":             {"X-API-Key": "secret"},
		"newline":                {"X-Version": "v1\r\nX-Evil: yes"},
		"surrounding whitespace": {" X-Version": "v1"},
	} {
		t.Run(name, func(t *testing.T) {
			node := valid
			node.Headers = headers
			if err := normalizeOutboundAgent(&node); err == nil {
				t.Fatal("expected invalid header to fail")
			}
		})
	}
}

func TestMemoryOutboundAgentRoundTripKeepsNewFields(t *testing.T) {
	store := NewTestMemory()
	node := config.OutboundAgent{
		ID: "event-edge", Transport: config.OutboundTransportEvent, Enabled: true,
		URL:           "https://agent.example/invoke",
		Headers:       map[string]string{"X-CFF-Request-Version": "v1"},
		Auth:          config.OutboundAgentAuth{Type: config.OutboundAuthHeader, Header: "X-Auth-Token", Secret: "secret"},
		EventResponse: config.EventResponseResultField,
	}
	if err := store.UpsertOutboundAgent(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetOutboundAgent(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Transport != node.Transport || got.URL != node.URL || got.Auth.Secret != "secret" || got.EventResponse != node.EventResponse || got.Headers["X-Cff-Request-Version"] != "v1" {
		t.Fatalf("round trip = %#v", got)
	}
}

func TestMemoryOutboundAgentRoundTripKeepsHuaweiCloudAKSKFields(t *testing.T) {
	st := NewTestMemory()
	node := config.OutboundAgent{
		ID: "functiongraph-edge", Transport: config.OutboundTransportEvent, Enabled: true,
		URL: "https://functiongraph.example/invoke",
		Auth: config.OutboundAgentAuth{
			Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "test-access-key",
			ProjectID: "test-project-id", Secret: "test-secret-key",
		},
		EventResponse: config.EventResponseResultField,
	}
	if err := st.UpsertOutboundAgent(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetOutboundAgent(context.Background(), node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Auth, node.Auth) {
		t.Fatalf("auth round trip = %#v, want %#v", got.Auth, node.Auth)
	}
}
