package tlsutil

import "testing"

func TestClientCredentialsEnabledUsesTLSWithoutCustomFiles(t *testing.T) {
	creds, err := ClientCredentials(nil, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("security protocol = %q, want tls", got)
	}
}

func TestClientCredentialsDefaultWithoutFilesUsesInsecure(t *testing.T) {
	creds, err := ClientCredentials(nil, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Fatalf("security protocol = %q, want insecure", got)
	}
}

func TestServerCredentialsEnabledRequiresCertificate(t *testing.T) {
	if _, err := ServerCredentials(nil, "", "", true); err == nil {
		t.Fatal("expected missing cert_file and key_file to fail when TLS is enabled")
	}
}

func TestClientTLSConfigDefaultWithoutFilesIsNil(t *testing.T) {
	cfg, err := ClientTLSConfig(nil, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatalf("expected nil TLS config, got %#v", cfg)
	}
}

func TestClientTLSConfigEnabledVerifiesCAWithoutServerName(t *testing.T) {
	cfg, err := ClientTLSConfig(nil, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.ServerName != "" || !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil {
		t.Fatalf("expected CA-only verification config, got %#v", cfg)
	}
}

func TestServerTLSConfigDisabledIsNil(t *testing.T) {
	cfg, err := ServerTLSConfig(nil, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatalf("expected nil TLS config, got %#v", cfg)
	}
}

func TestServerTLSConfigEnabledRequiresCertificate(t *testing.T) {
	if _, err := ServerTLSConfig(nil, "", "", true); err == nil {
		t.Fatal("expected missing cert_file and key_file to fail when HTTP TLS is enabled")
	}
}
