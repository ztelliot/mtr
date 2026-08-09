package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstExistingConfigPrefersSystemPath(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "system.yaml")
	localPath := filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(systemPath, []byte("http_addr: :8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := firstExistingConfig(systemPath, localPath); got != systemPath {
		t.Fatalf("config path = %q, want %q", got, systemPath)
	}
}

func TestFirstExistingConfigFallsBackToLocalPath(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "missing.yaml")
	localPath := filepath.Join(dir, "local.yaml")
	if got := firstExistingConfig(systemPath, localPath); got != localPath {
		t.Fatalf("config path = %q, want %q", got, localPath)
	}
}

func TestReadBootstrapAdminToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admin-token")
	const secret = "bootstrap-secret-0123456789"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	got, err := readBootstrapAdminToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Fatalf("token mismatch")
	}
}

func TestReadBootstrapAdminTokenRejectsUnsafeInputWithoutEchoingSecret(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{name: "world readable", body: "world-readable-secret-012345", mode: 0o644},
		{name: "group writable", body: "group-writable-secret-012345", mode: 0o660},
		{name: "too short", body: "short", mode: 0o600},
		{name: "whitespace", body: "bootstrap secret 0123456789", mode: 0o600},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(test.name, " ", "-"))
			if err := os.WriteFile(path, []byte(test.body), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, test.mode); err != nil {
				t.Fatal(err)
			}
			_, err := readBootstrapAdminToken(path)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if strings.Contains(err.Error(), test.body) {
				t.Fatal("validation error exposed token contents")
			}
		})
	}
}

func TestReadBootstrapAdminTokenAllowsMissingConfiguration(t *testing.T) {
	got, err := readBootstrapAdminToken("  ")
	if err != nil || got != "" {
		t.Fatalf("got token %q, error %v", got, err)
	}
	got, err = readBootstrapAdminToken(filepath.Join(t.TempDir(), "removed-after-bootstrap"))
	if err != nil || got != "" {
		t.Fatalf("missing bootstrap file returned token %q, error %v", got, err)
	}
}
