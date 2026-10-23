package auth

import (
	"context"
	"os"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// A config-derived openai-compatibility block without api-key entries but with
// request-scoped-errors metadata must not be written into the auth dir.
func TestManagerPersist_SkipsConfigOpenAICompatBlockWithRequestScopedErrors(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	mgr := cliproxyauth.NewManager(store, nil, nil)

	compat := &cliproxyauth.Auth{
		ID:       "openai-compatibility:chain-oc:abc123",
		Provider: "chain-oc",
		Label:    "chain-oc",
		Status:   cliproxyauth.StatusActive,
		Attributes: map[string]string{
			"source":       "config:chain-oc[abc123]",
			"base_url":     "http://127.0.0.1:1/v1",
			"compat_name":  "chain-oc",
			"provider_key": "chain-oc",
			"config_index": "0",
		},
		Metadata: map[string]any{
			"request_scoped_errors": []any{map[string]any{"status": 404}},
			"disable_cooling":       true,
		},
	}
	ctx := context.Background()
	if _, err := mgr.Register(ctx, compat); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := mgr.Update(ctx, compat); err != nil {
		t.Fatalf("Update: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty auth dir, got %d entries (first: %s)", len(entries), entries[0].Name())
	}

	// Control: a regular file-backed auth still persists into the same dir.
	file := &cliproxyauth.Auth{
		ID:       "control.json",
		Provider: "codex",
		Status:   cliproxyauth.StatusActive,
		Metadata: map[string]any{"type": "codex"},
	}
	if _, err := mgr.Register(ctx, file); err != nil {
		t.Fatalf("Register control: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected control auth to be persisted, got %d entries", len(entries))
	}
}
