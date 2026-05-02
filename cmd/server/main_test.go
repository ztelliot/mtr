package main

import (
	"os"
	"path/filepath"
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
