package store

import (
	"fmt"
	"log"
	"strings"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func normalizeHTTPAgent(node *config.HTTPAgent) error {
	node.ID = strings.TrimSpace(node.ID)
	node.BaseURL = strings.TrimSpace(node.BaseURL)
	if node.ID == "" {
		return fmt.Errorf("http agent id is required")
	}
	if node.BaseURL == "" {
		return fmt.Errorf("http agent base_url is required")
	}
	node.HTTPToken = strings.TrimSpace(node.HTTPToken)
	if node.HTTPToken == "" {
		return fmt.Errorf("http agent http_token is required")
	}
	node.Labels = normalizedAgentLabels(node.Labels)
	return nil
}

func normalizeAgentConfig(cfg *config.AgentConfig) error {
	cfg.ID = strings.TrimSpace(cfg.ID)
	if cfg.ID == "" {
		return fmt.Errorf("agent id is required")
	}
	cfg.Labels = normalizedAgentLabels(cfg.Labels)
	return nil
}

func normalizedAgentLabels(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || model.IsReservedAgentLabel(value) {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func printInitialAdminToken(settings config.ManagedSettings) {
	for _, token := range settings.APITokens {
		if token.ManageAccess == "write" && token.Secret != "" {
			log.Printf("initialized admin api token: %s", token.Secret)
			return
		}
	}
}
