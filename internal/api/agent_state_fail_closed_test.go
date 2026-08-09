package api

import (
	"context"
	"errors"
	"testing"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

var errTestAgentStateUnavailable = errors.New("agent state unavailable")

type failingAgentStateStore struct {
	store.Store
	failListConfigs bool
	failOutbound    bool
	failGRPCConfig  bool
}

func (s *failingAgentStateStore) ListAgentConfigs(ctx context.Context) ([]config.AgentConfig, error) {
	if s.failListConfigs {
		return nil, errTestAgentStateUnavailable
	}
	return s.Store.ListAgentConfigs(ctx)
}

func (s *failingAgentStateStore) GetOutboundAgent(ctx context.Context, id string) (config.OutboundAgent, error) {
	if s.failOutbound {
		return config.OutboundAgent{}, errTestAgentStateUnavailable
	}
	return s.Store.GetOutboundAgent(ctx, id)
}

func (s *failingAgentStateStore) GetAgentConfig(ctx context.Context, id string) (config.AgentConfig, error) {
	if s.failGRPCConfig {
		return config.AgentConfig{}, errTestAgentStateUnavailable
	}
	return s.Store.GetAgentConfig(ctx, id)
}

func TestAgentPolicyAndDisabledStateReadsFailClosed(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()

	policyServer := &Server{
		store:    &failingAgentStateStore{Store: base, failListConfigs: true},
		policies: policy.DefaultPolicies(),
	}
	if _, err := policyServer.policiesForAgent(ctx, "edge-1"); !errors.Is(err, errTestAgentStateUnavailable) {
		t.Fatalf("policy error = %v", err)
	}

	for name, failingStore := range map[string]*failingAgentStateStore{
		"outbound lookup": {Store: base, failOutbound: true},
		"grpc config":     {Store: base, failGRPCConfig: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := &Server{store: failingStore, policies: policy.DefaultPolicies()}
			disabled, err := server.agentDisabled(ctx, "edge-1")
			if !disabled || !errors.Is(err, errTestAgentStateUnavailable) {
				t.Fatalf("disabled = %t, error = %v", disabled, err)
			}
		})
	}
}
