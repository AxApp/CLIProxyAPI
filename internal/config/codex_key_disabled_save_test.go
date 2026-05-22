package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveConfigPreserveCommentsSyncsCodexKeyDisabled(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	initial := strings.TrimSpace(`
codex-api-key:
  - api-key: enabled-key
    disabled: true
    base-url: https://example.test/enabled
  - api-key: disabled-key
    base-url: https://example.test/disabled
`) + "\n"
	if err := os.WriteFile(configFile, []byte(initial), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{
		CodexKey: []CodexKey{
			{
				APIKey:  "enabled-key",
				BaseURL: "https://example.test/enabled",
			},
			{
				APIKey:   "disabled-key",
				BaseURL:  "https://example.test/disabled",
				Disabled: true,
			},
		},
	}
	if err := SaveConfigPreserveComments(configFile, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	output := string(data)
	if strings.Contains(output, "api-key: enabled-key\n    disabled: true") {
		t.Fatalf("enabled key kept disabled flag:\n%s", output)
	}
	if !strings.Contains(output, "api-key: disabled-key\n    base-url: https://example.test/disabled\n    disabled: true") {
		t.Fatalf("disabled key missing disabled flag:\n%s", output)
	}
}
