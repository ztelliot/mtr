package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/policy"
	"github.com/ztelliot/mtr/internal/store"
)

var errTestHubAgentStateUnavailable = errors.New("agent state unavailable")

type failingHubAgentStateStore struct {
	store.Store
	failListConfigs bool
	failOutbound    bool
	failGRPCConfig  bool
}

func (s *failingHubAgentStateStore) ListAgentConfigs(ctx context.Context) ([]config.AgentConfig, error) {
	if s.failListConfigs {
		return nil, errTestHubAgentStateUnavailable
	}
	return s.Store.ListAgentConfigs(ctx)
}

func (s *failingHubAgentStateStore) GetOutboundAgent(ctx context.Context, id string) (config.OutboundAgent, error) {
	if s.failOutbound {
		return config.OutboundAgent{}, errTestHubAgentStateUnavailable
	}
	return s.Store.GetOutboundAgent(ctx, id)
}

func (s *failingHubAgentStateStore) GetAgentConfig(ctx context.Context, id string) (config.AgentConfig, error) {
	if s.failGRPCConfig {
		return config.AgentConfig{}, errTestHubAgentStateUnavailable
	}
	return s.Store.GetAgentConfig(ctx, id)
}

func TestHubAgentPolicyAndDisabledStateReadsFailClosed(t *testing.T) {
	ctx := context.Background()
	base := store.NewTestMemory()
	newHub := func(st store.Store) *Hub {
		return NewHub(st, policy.DefaultPolicies(), time.Second, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	hub := newHub(&failingHubAgentStateStore{Store: base, failListConfigs: true})
	if _, err := hub.policiesForAgent(ctx, "edge-1"); !errors.Is(err, errTestHubAgentStateUnavailable) {
		t.Fatalf("policy error = %v", err)
	}

	for name, failingStore := range map[string]*failingHubAgentStateStore{
		"outbound lookup": {Store: base, failOutbound: true},
		"grpc config":     {Store: base, failGRPCConfig: true},
	} {
		t.Run(name, func(t *testing.T) {
			disabled, err := newHub(failingStore).agentConfigDisabled(ctx, "edge-1")
			if !disabled || !errors.Is(err, errTestHubAgentStateUnavailable) {
				t.Fatalf("disabled = %t, error = %v", disabled, err)
			}
		})
	}
}
