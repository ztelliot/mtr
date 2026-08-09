package store

import (
	"errors"
	"testing"
)

func TestMemoryBootstrapAdminToken(t *testing.T) {
	if store, initialization, err := NewMemory(""); !errors.Is(err, ErrBootstrapAdminTokenRequired) {
		t.Fatalf("empty bootstrap token: store=%#v initialization=%#v err=%v", store, initialization, err)
	}

	store, initialization, err := NewMemory("  explicit-admin-token  ")
	if err != nil {
		t.Fatal(err)
	}
	if !initialization.Seeded {
		t.Fatal("memory store was not reported as seeded")
	}
	settings, err := store.GetManagedSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.APITokens) != 1 || settings.APITokens[0].Secret != "explicit-admin-token" {
		t.Fatalf("unexpected seeded settings: %#v", settings.APITokens)
	}
}

func TestBootstrapAdminTokenSizeLimit(t *testing.T) {
	oversized := make([]byte, 4097)
	for i := range oversized {
		oversized[i] = 'x'
	}
	if _, _, err := NewMemory(string(oversized)); err == nil {
		t.Fatal("expected oversized bootstrap token to be rejected")
	}
}
