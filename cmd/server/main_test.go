package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ztelliot/mtr/internal/config"
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

func TestToSchedulerOutboundAgentsKeepsConnectionConfig(t *testing.T) {
	agents := toSchedulerOutboundAgents([]config.OutboundAgent{{ID: "edge-1", BaseURL: "http://edge", HTTPToken: "secret"}})
	if len(agents) != 1 || agents[0].ID != "edge-1" || agents[0].BaseURL != "http://edge" || agents[0].Token != "secret" {
		t.Fatalf("unexpected agents: %#v", agents)
	}
}
