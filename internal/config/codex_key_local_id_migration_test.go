package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigPersistsMissingCodexKeyLocalIDs(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initial := `codex-api-key:
  - api-key: sk-one
    base-url: https://api.openai.com/v1
  - api-key: sk-one
    base-url: https://api.openai.com/v1
`
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}
	if len(cfg.CodexKey) != 2 {
		t.Fatalf("CodexKey len = %d, want 2", len(cfg.CodexKey))
	}
	if cfg.CodexKey[0].LocalID == "" || cfg.CodexKey[1].LocalID == "" {
		t.Fatalf("expected generated local ids, got %q / %q", cfg.CodexKey[0].LocalID, cfg.CodexKey[1].LocalID)
	}
	if cfg.CodexKey[0].LocalID == cfg.CodexKey[1].LocalID {
		t.Fatalf("duplicate config entries must receive distinct local ids, got %q", cfg.CodexKey[0].LocalID)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if got := strings.Count(string(content), "local-id: codex-api-key:legacy-"); got != 2 {
		t.Fatalf("persisted local-id count = %d, want 2\n%s", got, string(content))
	}
}
