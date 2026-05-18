package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileTokenStoreListCodexDoesNotInjectWebsocketsByDefault(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "codex.json"), []byte(`{"type":"codex","email":"codex@example.com"}`), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if got, ok := auths[0].Attributes["websockets"]; ok {
		t.Fatalf("websockets attribute should be absent by default, got %q", got)
	}
}

func TestFileTokenStoreListCodexKeepsExplicitWebsocketsInMetadata(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "codex.json"), []byte(`{"type":"codex","email":"codex@example.com","websockets":false}`), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if got, ok := auths[0].Attributes["websockets"]; ok {
		t.Fatalf("websockets attribute should be absent when disabled, got %q", got)
	}
	if got, ok := auths[0].Metadata["websockets"].(bool); !ok || got {
		t.Fatalf("expected websockets=false metadata to be preserved, got %#v", auths[0].Metadata["websockets"])
	}
}

func TestExtractAccessToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata map[string]any
		expected string
	}{
		{
			"antigravity top-level access_token",
			map[string]any{"access_token": "tok-abc"},
			"tok-abc",
		},
		{
			"gemini nested token.access_token",
			map[string]any{
				"token": map[string]any{"access_token": "tok-nested"},
			},
			"tok-nested",
		},
		{
			"top-level takes precedence over nested",
			map[string]any{
				"access_token": "tok-top",
				"token":        map[string]any{"access_token": "tok-nested"},
			},
			"tok-top",
		},
		{
			"empty metadata",
			map[string]any{},
			"",
		},
		{
			"whitespace-only access_token",
			map[string]any{"access_token": "   "},
			"",
		},
		{
			"wrong type access_token",
			map[string]any{"access_token": 12345},
			"",
		},
		{
			"token is not a map",
			map[string]any{"token": "not-a-map"},
			"",
		},
		{
			"nested whitespace-only",
			map[string]any{
				"token": map[string]any{"access_token": "  "},
			},
			"",
		},
		{
			"fallback to nested when top-level empty",
			map[string]any{
				"access_token": "",
				"token":        map[string]any{"access_token": "tok-fallback"},
			},
			"tok-fallback",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractAccessToken(tt.metadata)
			if got != tt.expected {
				t.Errorf("extractAccessToken() = %q, want %q", got, tt.expected)
			}
		})
	}
}
