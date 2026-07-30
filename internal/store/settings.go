package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func prepareAgentRegistration(settings *config.ManagedSettings, current *model.Agent, next model.Agent, suppliedToken string) (model.Agent, bool, error) {
	suppliedToken = strings.TrimSpace(suppliedToken)
	var matched *config.RegisterToken
	for i := range settings.RegisterTokens {
		if suppliedToken != "" && settings.RegisterTokens[i].Token == suppliedToken {
			matched = &settings.RegisterTokens[i]
			break
		}
	}
	if matched == nil {
		return model.Agent{}, false, ErrInvalidRegisterToken
	}

	keyHash := registrationTokenHash(matched.ID)
	if current != nil {
		if current.TokenHash != keyHash {
			return model.Agent{}, false, ErrInvalidRegisterToken
		}
		next.Country = current.Country
		next.Region = current.Region
		next.Provider = current.Provider
		next.ISP = current.ISP
		next.CreatedAt = current.CreatedAt
		next.TokenHash = keyHash
		return next, false, nil
	}

	if len(matched.AllowedAgentIDs) > 0 && !containsExactString(matched.AllowedAgentIDs, next.ID) {
		return model.Agent{}, false, ErrAgentIDNotAllowed
	}
	if matched.RemainingUses <= 0 {
		return model.Agent{}, false, ErrRegisterTokenExhausted
	}
	matched.RemainingUses--
	next.TokenHash = keyHash
	return next, true, nil
}

func containsExactString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func registrationTokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func preserveAgentRegistrationMetadata(current, next model.Agent) model.Agent {
	// Managed HTTP agents may be inserted before their first successful health
	// probe. Allow each still-empty field to be initialized once, then lock it.
	if current.Country != "" {
		next.Country = current.Country
	}
	if current.Region != "" {
		next.Region = current.Region
	}
	if current.Provider != "" {
		next.Provider = current.Provider
	}
	if current.ISP != "" {
		next.ISP = current.ISP
	}
	if current.TokenHash != "" {
		next.TokenHash = current.TokenHash
	}
	next.CreatedAt = current.CreatedAt
	return next
}

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
