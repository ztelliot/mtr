package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

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
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
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

func ClientCredentials(caFiles []string, certFile, keyFile string, enabled bool) (credentials.TransportCredentials, error) {
	if !enabled && len(caFiles) == 0 && certFile == "" && keyFile == "" {
		return insecure.NewCredentials(), nil
	}
	cfg, err := ClientTLSConfig(caFiles, certFile, keyFile, enabled)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(cfg), nil
}

func ClientTLSConfig(caFiles []string, certFile, keyFile string, enabled bool) (*tls.Config, error) {
	if !enabled && len(caFiles) == 0 && certFile == "" && keyFile == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caFiles) > 0 {
		pool, err := loadPool(caFiles)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("tls peer certificate is required")
		}
		opts := x509.VerifyOptions{
			Roots:         cfg.RootCAs,
			CurrentTime:   time.Now(),
			Intermediates: x509.NewCertPool(),
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		for _, cert := range state.PeerCertificates[1:] {
			opts.Intermediates.AddCert(cert)
		}
		_, err := state.PeerCertificates[0].Verify(opts)
		return err
	}
	return cfg, nil
}

func loadPool(paths []string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		pool.AppendCertsFromPEM(b)
	}
	return pool, nil
}
