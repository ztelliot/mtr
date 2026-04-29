package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

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
	return credentials.NewTLS(cfg), nil
}

func ClientCredentials(caFiles []string, certFile, keyFile, serverName string, enabled bool) (credentials.TransportCredentials, error) {
	if !enabled && len(caFiles) == 0 && certFile == "" && keyFile == "" {
		return insecure.NewCredentials(), nil
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
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(cfg), nil
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
