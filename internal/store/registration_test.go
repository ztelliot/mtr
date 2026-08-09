package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ztelliot/mtr/internal/config"
	"github.com/ztelliot/mtr/internal/model"
)

func TestMemoryAgentRegistrationRules(t *testing.T) {
	testAgentRegistrationRules(t, NewTestMemory())
}

func TestSQLiteAgentRegistrationRules(t *testing.T) {
	st, err := newTestSQLite(context.Background(), "sqlite://:memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	testAgentRegistrationRules(t, st)
}

func testAgentRegistrationRules(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	unconfigured := model.Agent{ID: "unconfigured", Status: model.AgentOnline, LastSeenAt: now, CreatedAt: now}
	if _, _, err := st.RegisterAgent(ctx, unconfigured, "not-configured"); !errors.Is(err, ErrInvalidRegisterToken) {
		t.Fatalf("unconfigured register token error = %v", err)
	}
	settings, err := st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.RegisterTokens = []config.RegisterToken{
		{ID: "key-a", Name: "edge", Token: "secret-a", RemainingUses: 1, AllowedAgentIDs: []string{"edge-1", "edge-2"}},
		{ID: "key-b", Name: "other", Token: "secret-b", RemainingUses: 1, AllowedAgentIDs: []string{"edge-2"}},
		{ID: "key-open", Name: "open", Token: "secret-open", RemainingUses: 1},
	}
	if _, err := st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}

	first := model.Agent{
		ID:           "edge-1",
		Country:      "CN",
		Region:       "Shanghai",
		Provider:     "provider-a",
		ISP:          "isp-a",
		Version:      "v1",
		Capabilities: []model.Tool{model.ToolPing},
		Protocols:    model.ProtocolIPv4,
		Status:       model.AgentOnline,
		LastSeenAt:   now,
		CreatedAt:    now,
	}
	blocked := first
	blocked.ID = "blocked"
	if _, _, err := st.RegisterAgent(ctx, blocked, "secret-a"); !errors.Is(err, ErrAgentIDNotAllowed) {
		t.Fatalf("disallowed agent id error = %v", err)
	}
	settings, err = st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.RegisterTokens[0].RemainingUses != 1 {
		t.Fatalf("disallowed agent consumed a use: %d", settings.RegisterTokens[0].RemainingUses)
	}
	registered, created, err := st.RegisterAgent(ctx, first, "secret-a")
	if err != nil {
		t.Fatal(err)
	}
	if !created || registered.TokenHash != registrationTokenHash("key-a") {
		t.Fatalf("first registration = %#v, created = %t", registered, created)
	}
	settings, err = st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.RegisterTokens[0].RemainingUses != 0 {
		t.Fatalf("remaining uses = %d, want 0", settings.RegisterTokens[0].RemainingUses)
	}

	reconnect := first
	reconnect.Country = "US"
	reconnect.Region = "California"
	reconnect.Provider = "provider-b"
	reconnect.ISP = "isp-b"
	reconnect.Version = "v2"
	reconnect.Capabilities = []model.Tool{model.ToolDNS}
	reconnect.Protocols = model.ProtocolIPv6
	reconnect.LastSeenAt = now.Add(time.Minute)
	reconnect.CreatedAt = reconnect.LastSeenAt
	registered, created, err = st.RegisterAgent(ctx, reconnect, "secret-a")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("reconnect must not create a new agent")
	}
	if registered.Country != first.Country || registered.Region != first.Region || registered.Provider != first.Provider || registered.ISP != first.ISP {
		t.Fatalf("registration metadata changed on reconnect: %#v", registered)
	}
	if registered.Version != "v2" || registered.Protocols != model.ProtocolIPv6 || len(registered.Capabilities) != 1 || registered.Capabilities[0] != model.ToolDNS {
		t.Fatalf("runtime metadata was not refreshed: %#v", registered)
	}

	second := first
	second.ID = "edge-2"
	if _, _, err := st.RegisterAgent(ctx, second, "secret-a"); !errors.Is(err, ErrRegisterTokenExhausted) {
		t.Fatalf("exhausted token error = %v", err)
	}
	if _, _, err := st.RegisterAgent(ctx, reconnect, "secret-b"); !errors.Is(err, ErrInvalidRegisterToken) {
		t.Fatalf("different token should not take over existing agent: %v", err)
	}
	openAgent := first
	openAgent.ID = "arbitrary-id"
	if _, created, err := st.RegisterAgent(ctx, openAgent, "secret-open"); err != nil || !created {
		t.Fatalf("empty allowlist should allow any new agent id: created=%t err=%v", created, err)
	}

	settings, err = st.GetManagedSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.RegisterTokens[0].Token = "rotated-a"
	settings.RegisterTokens[0].AllowedAgentIDs = []string{"different-new-agent"}
	if _, err := st.UpdateManagedSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if _, created, err := st.RegisterAgent(ctx, reconnect, "rotated-a"); err != nil || created {
		t.Fatalf("reconnect after key rotation: created=%t err=%v", created, err)
	}
	if _, _, err := st.RegisterAgent(ctx, reconnect, "secret-a"); !errors.Is(err, ErrInvalidRegisterToken) {
		t.Fatalf("old rotated token should be rejected: %v", err)
	}
}

func TestUpsertAgentPreservesRegistrationMetadata(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testUpsertAgentPreservesRegistrationMetadata(t, NewTestMemory())
	})
	t.Run("sqlite", func(t *testing.T) {
		st, err := newTestSQLite(context.Background(), "sqlite://:memory:")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		testUpsertAgentPreservesRegistrationMetadata(t, st)
	})
}

func testUpsertAgentPreservesRegistrationMetadata(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	first := model.Agent{ID: "edge-1", Country: "CN", Region: "Shanghai", Provider: "provider-a", ISP: "isp-a", TokenHash: "registration-binding", CreatedAt: now}
	if err := st.UpsertAgent(ctx, first); err != nil {
		t.Fatal(err)
	}
	next := model.Agent{ID: first.ID, Country: "US", Region: "California", Provider: "provider-b", ISP: "isp-b", Version: "v2", Protocols: model.ProtocolIPv6, CreatedAt: now.Add(time.Hour)}
	if err := st.UpsertAgent(ctx, next); err != nil {
		t.Fatal(err)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].Country != first.Country || agents[0].Region != first.Region || agents[0].Provider != first.Provider || agents[0].ISP != first.ISP || agents[0].TokenHash != first.TokenHash || agents[0].Protocols != model.ProtocolIPv6 {
		t.Fatalf("unexpected agent after upsert: %#v", agents)
	}
}
