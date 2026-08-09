package tlsutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientCredentialsEnabledUsesTLSWithoutCustomFiles(t *testing.T) {
	creds, err := ClientCredentials(nil, "", "", true, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("security protocol = %q, want tls", got)
	}
}

func TestClientCredentialsDefaultWithoutFilesUsesInsecure(t *testing.T) {
	creds, err := ClientCredentials(nil, "", "", false, "")
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
	cfg, err := ClientTLSConfig(nil, "", "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		t.Fatalf("expected nil TLS config, got %#v", cfg)
	}
}

func TestClientTLSConfigUsesStandardHostnameVerification(t *testing.T) {
	cfg, err := ClientTLSConfig(nil, "", "", true, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.ServerName != "agent.example.com" || cfg.InsecureSkipVerify || cfg.VerifyConnection != nil {
		t.Fatalf("expected standard SAN verification config, got %#v", cfg)
	}
}

func TestOutboundClientTLSConfigWithoutExplicitCAUsesStandardHostnameVerification(t *testing.T) {
	cfg, err := OutboundClientTLSConfig(nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.ServerName != "" || cfg.InsecureSkipVerify || cfg.VerifyConnection != nil {
		t.Fatalf("unexpected outbound TLS config: %#v", cfg)
	}
	if _, err := OutboundClientTLSConfig(nil, "", "", "agent-routing.internal"); err == nil {
		t.Fatal("SNI without an explicit trust bundle was accepted")
	}
}

func TestOutboundClientTLSConfigWithoutExplicitCARejectsHostnameMismatch(t *testing.T) {
	ca, caKey, _ := writeOutboundTestCA(t)
	leaf, leafKey := newOutboundTestLeaf(t, ca, caKey, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request reached server despite hostname mismatch")
	}))
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: leafKey}},
		MinVersion:   tls.VersionTLS12,
	}
	server.StartTLS()
	defer server.Close()

	cfg, err := OutboundClientTLSConfig(nil, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	cfg.RootCAs = pool // Model a system-root-trusted certificate in a hermetic test.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("standard outbound TLS accepted a certificate for a different hostname")
	}
}

func TestOutboundClientTLSConfigVerifiesChainTimeAndServerAuth(t *testing.T) {
	ca, caKey, caPath := writeOutboundTestCA(t)
	now := time.Now()
	for _, test := range []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		usage     x509.ExtKeyUsage
		wantError bool
	}{
		{name: "valid shared server certificate", notBefore: now.Add(-time.Hour), notAfter: now.Add(time.Hour), usage: x509.ExtKeyUsageServerAuth},
		{name: "expired", notBefore: now.Add(-2 * time.Hour), notAfter: now.Add(-time.Hour), usage: x509.ExtKeyUsageServerAuth, wantError: true},
		{name: "wrong extended key usage", notBefore: now.Add(-time.Hour), notAfter: now.Add(time.Hour), usage: x509.ExtKeyUsageClientAuth, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaf, _ := newOutboundTestLeaf(t, ca, caKey, test.notBefore, test.notAfter, test.usage)
			cfg, err := OutboundClientTLSConfig([]string{caPath}, "", "", "routing-name.invalid")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ServerName != "routing-name.invalid" || !cfg.InsecureSkipVerify || cfg.VerifyConnection == nil {
				t.Fatalf("unexpected explicit-CA outbound TLS config: %#v", cfg)
			}
			err = cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}})
			if (err != nil) != test.wantError {
				t.Fatalf("verification error = %v, wantError = %t", err, test.wantError)
			}
		})
	}
}

func writeOutboundTestCA(t *testing.T) (*x509.Certificate, *rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "outbound test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "outbound-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificate, key, path
}

func newOutboundTestLeaf(t *testing.T, ca *x509.Certificate, caKey *rsa.PrivateKey, notBefore time.Time, notAfter time.Time, usage x509.ExtKeyUsage) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "shared Agent certificate"},
		DNSNames:     []string{"certificate-name.invalid"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
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

func TestMaterialFingerprintChangesWhenFileContentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := MaterialFingerprint([]string{path}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := MaterialFingerprint([]string{path}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("TLS material fingerprint did not change")
	}
}

func TestTLSMaterialReaderRejectsNonRegularAndOversizedFiles(t *testing.T) {
	if _, err := MaterialFingerprint([]string{t.TempDir()}, "", ""); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "oversized.pem")
	if err := os.WriteFile(path, make([]byte, maxTLSMaterialBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterialFingerprint([]string{path}, "", ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized error = %v", err)
	}
}
