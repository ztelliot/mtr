package tlsutil

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const maxTLSMaterialBytes = 4 << 20

func ServerCredentials(caFiles []string, certFile, keyFile string, enabled bool) (credentials.TransportCredentials, error) {
	if certFile == "" || keyFile == "" {
		if enabled {
			return nil, errors.New("tls cert_file and key_file are required when tls.enabled is true")
		}
		return insecure.NewCredentials(), nil
	}
	cfg, err := serverTLSConfig(caFiles, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func ServerTLSConfig(caFiles []string, certFile, keyFile string, enabled bool) (*tls.Config, error) {
	if !enabled {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("tls cert_file and key_file are required when tls.enabled is true")
	}
	return serverTLSConfig(caFiles, certFile, keyFile)
}

func serverTLSConfig(caFiles []string, certFile, keyFile string) (*tls.Config, error) {
	cert, err := loadKeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if len(caFiles) > 0 {
		ca, err := loadPool(caFiles)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = ca
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

func ClientCredentials(caFiles []string, certFile, keyFile string, enabled bool, serverName string) (credentials.TransportCredentials, error) {
	if !enabled && len(caFiles) == 0 && certFile == "" && keyFile == "" {
		return insecure.NewCredentials(), nil
	}
	cfg, err := ClientTLSConfig(caFiles, certFile, keyFile, enabled, serverName)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func ClientTLSConfig(caFiles []string, certFile, keyFile string, enabled bool, serverName string) (*tls.Config, error) {
	if !enabled && len(caFiles) == 0 && certFile == "" && keyFile == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if len(caFiles) > 0 {
		pool, err := loadPool(caFiles)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if certFile != "" || keyFile != "" {
		cert, err := loadKeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// OutboundClientTLSConfig builds TLS for a server-managed HTTP/Event Agent.
// With explicit CA files, managed Agents may share one CA-issued certificate
// across several endpoint names: the certificate chain, validity period, and
// ServerAuth usage are verified without binding trust to the request hostname.
// In that mode sni is only a TLS routing hint. Without explicit CA files this
// keeps the standard system-root hostname/SAN verification.
func OutboundClientTLSConfig(caFiles []string, certFile, keyFile string, sni string) (*tls.Config, error) {
	if len(caFiles) == 0 {
		if sni != "" {
			return nil, errors.New("outbound TLS server_name requires explicit ca_files")
		}
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if certFile != "" || keyFile != "" {
			cert, err := loadKeyPair(certFile, keyFile)
			if err != nil {
				return nil, err
			}
			cfg.Certificates = []tls.Certificate{cert}
		}
		return cfg, nil
	}
	roots, err := loadPool(caFiles)
	if err != nil {
		return nil, err
	}
	cfg := outboundTLSConfig(roots, sni)
	if certFile != "" || keyFile != "" {
		cert, err := loadKeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func outboundTLSConfig(roots *x509.CertPool, sni string) *tls.Config {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         sni,
		InsecureSkipVerify: true, // Verification is performed below without a DNSName.
	}
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("outbound TLS peer provided no certificate")
		}
		intermediates := x509.NewCertPool()
		for _, certificate := range state.PeerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return fmt.Errorf("verify outbound TLS certificate chain: %w", err)
		}
		return nil
	}
	return cfg
}

func loadPool(paths []string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, path := range paths {
		b, err := readTLSMaterial(path)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, fmt.Errorf("tls CA file %q contains no certificates", path)
		}
	}
	return pool, nil
}

func loadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	certPEM, err := readTLSMaterial(certFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err := readTLSMaterial(keyFile)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func readTLSMaterial(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("tls material %q must be a regular file", path)
	}
	if info.Size() > maxTLSMaterialBytes {
		return nil, fmt.Errorf("tls material %q exceeds %d bytes", path, maxTLSMaterialBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxTLSMaterialBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxTLSMaterialBytes {
		return nil, fmt.Errorf("tls material %q exceeds %d bytes", path, maxTLSMaterialBytes)
	}
	return content, nil
}

// MaterialFingerprint hashes the exact TLS files used to construct a client.
// Callers can use it to detect in-place CA and client-certificate rotation.
func MaterialFingerprint(caFiles []string, certFile, keyFile string) ([sha256.Size]byte, error) {
	hash := sha256.New()
	paths := make([]struct {
		role string
		path string
	}, 0, len(caFiles)+2)
	for _, path := range caFiles {
		paths = append(paths, struct {
			role string
			path string
		}{role: "ca", path: path})
	}
	if certFile != "" {
		paths = append(paths, struct {
			role string
			path string
		}{role: "cert", path: certFile})
	}
	if keyFile != "" {
		paths = append(paths, struct {
			role string
			path string
		}{role: "key", path: keyFile})
	}
	var size [8]byte
	for _, item := range paths {
		content, err := readTLSMaterial(item.path)
		if err != nil {
			return [sha256.Size]byte{}, err
		}
		for _, value := range []string{item.role, item.path} {
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = hash.Write(size[:])
			_, _ = io.WriteString(hash, value)
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(content)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(content)
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint, nil
}
