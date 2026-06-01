package synthesizer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_SynthesizesAccountStoreAuthFilesWhenStoreOwnsCodex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	created, err := store.CreateAccount(context.Background(), accountstore.AccountWrite{
		Kind:             accountstore.KindAuthFile,
		Title:            "Plus",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		AuthFile: &accountstore.AuthFileCredential{
			SourceFileName: "codex-plus.json",
			AuthJSON:       `{"type":"codex","access_token":"token","account_id":"acct_chatgpt","plan_type":"plus","email":"plus@example.com"}`,
			AuthType:       "codex",
			Email:          "plus@example.com",
			PlanType:       "plus",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
		Config:      &config.Config{AccountStoreDB: dbPath},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", auth.Provider)
	}
	if auth.AccountKey != created.AccountKey {
		t.Fatalf("account key = %q, want %q", auth.AccountKey, created.AccountKey)
	}
	if got := auth.Metadata["access_token"]; got != "token" {
		t.Fatalf("access token metadata = %#v, want token", got)
	}
}

func TestAccountStoreAccountsCachesWithinSynthesisPass(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if _, err := store.CreateAccount(context.Background(), accountstore.AccountWrite{
		Kind:             accountstore.KindAuthFile,
		Title:            "Plus",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		AuthFile: &accountstore.AuthFileCredential{
			SourceFileName: "codex-plus.json",
			AuthJSON:       `{"type":"codex","access_token":"token","account_id":"acct_chatgpt"}`,
			AuthType:       "codex",
		},
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	ctx := &SynthesisContext{Config: &config.Config{AccountStoreDB: dbPath}}
	accounts, active := accountStoreAccounts(ctx)
	if !active || len(accounts) != 1 {
		t.Fatalf("accountStoreAccounts active=%t len=%d, want one active account", active, len(accounts))
	}
	ctx.Config.AccountStoreDB = filepath.Join(t.TempDir(), "missing.sqlite")
	if !accountStoreHasKind(ctx, accountstore.KindAuthFile) {
		t.Fatal("accountStoreHasKind should use the synthesis-pass account store cache")
	}
}

func TestConfigSynthesizer_GeminiKeys(t *testing.T) {
	tests := []struct {
		name       string
		geminiKeys []config.GeminiKey
		wantLen    int
		validate   func(*testing.T, []*coreauth.Auth)
	}{
		{
			name: "single gemini key",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a"},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Provider != "gemini" {
					t.Errorf("expected provider gemini, got %s", auths[0].Provider)
				}
				if auths[0].Prefix != "team-a" {
					t.Errorf("expected prefix team-a, got %s", auths[0].Prefix)
				}
				if auths[0].Label != "gemini-apikey" {
					t.Errorf("expected label gemini-apikey, got %s", auths[0].Label)
				}
				if auths[0].Attributes["api_key"] != "test-key-123" {
					t.Errorf("expected api_key test-key-123, got %s", auths[0].Attributes["api_key"])
				}
				if auths[0].Metadata != nil {
					t.Errorf("expected metadata to be nil when disable_cooling not set, got %v", auths[0].Metadata)
				}
				if auths[0].Status != coreauth.StatusActive {
					t.Errorf("expected status active, got %s", auths[0].Status)
				}
			},
		},
		{
			name: "gemini key disable cooling",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a", DisableCooling: true},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
					t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
				}
			},
		},
		{
			name: "gemini key with base url and proxy",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:   "api-key",
					BaseURL:  "https://custom.api.com",
					ProxyURL: "http://proxy.local:8080",
					Prefix:   "custom",
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["base_url"] != "https://custom.api.com" {
					t.Errorf("expected base_url https://custom.api.com, got %s", auths[0].Attributes["base_url"])
				}
				if auths[0].ProxyURL != "http://proxy.local:8080" {
					t.Errorf("expected proxy_url http://proxy.local:8080, got %s", auths[0].ProxyURL)
				}
			},
		},
		{
			name: "gemini key with headers",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:  "api-key",
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["header:X-Custom"] != "value" {
					t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
				}
			},
		},
		{
			name: "empty api key skipped",
			geminiKeys: []config.GeminiKey{
				{APIKey: ""},
				{APIKey: "  "},
				{APIKey: "valid-key"},
			},
			wantLen: 1,
		},
		{
			name: "multiple gemini keys",
			geminiKeys: []config.GeminiKey{
				{APIKey: "key-1", Prefix: "a"},
				{APIKey: "key-2", Prefix: "b"},
				{APIKey: "key-3", Prefix: "c"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					GeminiKey: tt.geminiKeys,
				},
				Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}

			if tt.validate != nil && len(auths) > 0 {
				tt.validate(t, auths)
			}
		})
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:         "sk-ant-api-xxx",
					Prefix:         "main",
					BaseURL:        "https://api.anthropic.com",
					DisableCooling: true,
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "claude-apikey" {
		t.Errorf("expected label claude-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_CodexKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					LocalID:        "codex-api-key:stable-001",
					APIKey:         "codex-key-123",
					Prefix:         "dev",
					BaseURL:        "https://api.openai.com",
					ProxyURL:       "http://proxy.local",
					Websockets:     true,
					DisableCooling: true,
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "codex" {
		t.Errorf("expected provider codex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "codex-apikey" {
		t.Errorf("expected label codex-apikey, got %s", auths[0].Label)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if auths[0].Attributes["websockets"] != "true" {
		t.Errorf("expected websockets=true, got %s", auths[0].Attributes["websockets"])
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
	if auths[0].AccountKey != "codex-api-key:stable-001" {
		t.Errorf("expected account_key codex-api-key:stable-001, got %s", auths[0].AccountKey)
	}
}

func TestConfigSynthesizer_CodexKeys_RequiresStableAccountKey(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					LocalID: "codex-api-key:first",
					APIKey:  "shared-codex-key",
					BaseURL: "https://api.openai.com",
				},
				{
					LocalID: "codex-api-key:second",
					APIKey:  "shared-codex-key",
					BaseURL: "https://api.openai.com",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("expected 2 auths, got %d", len(auths))
	}
	if auths[0].AccountKey != "codex-api-key:first" || auths[1].AccountKey != "codex-api-key:second" {
		t.Fatalf("account keys = %q / %q, want stable local ids", auths[0].AccountKey, auths[1].AccountKey)
	}
	if auths[0].ID == auths[1].ID {
		t.Fatalf("runtime ids must remain distinct for duplicated credential cards, got %q", auths[0].ID)
	}
}

func TestConfigSynthesizer_CodexKeys_DisabledKeyStaysInRuntimeAsDisabled(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:   "codex-disabled-key",
					BaseURL:  "https://api.openai.com",
					Disabled: true,
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected disabled codex key to remain visible as disabled auth, got %d auths", len(auths))
	}
	if !auths[0].Disabled {
		t.Fatal("expected disabled codex key to synthesize disabled auth")
	}
	if auths[0].Status != coreauth.StatusDisabled {
		t.Fatalf("expected disabled status, got %s", auths[0].Status)
	}
}

func TestConfigSynthesizer_CodexKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: ""},   // empty, should be skipped
				{APIKey: "  "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"Authorization": "Bearer xyz"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:Authorization"] != "Bearer xyz" {
		t.Errorf("expected header:Authorization=Bearer xyz, got %s", auths[0].Attributes["header:Authorization"])
	}
}

func TestConfigSynthesizer_UsesAccountStoreForCodexAndOpenAICompatible(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts-v1.sqlite")
	store, err := accountstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open account store: %v", err)
	}
	defer store.Close()
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	codexAccount, err := store.CreateAccount(context.Background(), accountstore.AccountWrite{
		Kind:             accountstore.KindCodexAPIKey,
		Title:            "DB Codex",
		Provider:         "codex",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		Priority:         5,
		CodexAPIKey: &accountstore.CodexAPIKeyCredential{
			APIKey:      "sk-db-codex",
			BaseURL:     "https://db.example.com/v1",
			Prefix:      "db/",
			Websockets:  true,
			HeadersJSON: `{"X-DB":"1"}`,
			ModelsJSON:  `[{"name":"gpt-5","alias":"gpt-5"}]`,
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount codex: %v", err)
	}
	compatAccount, err := store.CreateAccount(context.Background(), accountstore.AccountWrite{
		Kind:             accountstore.KindOpenAICompatible,
		Title:            "DB DeepSeek",
		Provider:         "deepseek",
		CredentialSource: accountstore.SourceSidecarManagementAPI,
		OpenAICompatible: &accountstore.OpenAICompatibleCredential{
			ProviderName:       "deepseek",
			RuntimeProviderKey: "openai-compatible:db-deepseek",
			BaseURL:            "https://deepseek.example.com",
			Prefix:             "ds/",
			APIKeyEntriesJSON:  `[{"api-key":"sk-db-ds","proxy-url":"http://proxy.local:9000"}]`,
			HeadersJSON:        `{"X-Provider":"DeepSeek"}`,
			ModelsJSON:         `[{"name":"deepseek-chat","alias":"deepseek-chat"}]`,
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount compat: %v", err)
	}

	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(&SynthesisContext{
		Config: &config.Config{
			AccountStoreDB: dbPath,
			CodexKey: []config.CodexKey{{
				LocalID: "codex-api-key:legacy",
				APIKey:  "sk-legacy",
				BaseURL: "https://legacy.example.com",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "legacy",
				BaseURL: "https://legacy.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey: "sk-legacy-openai-compatible",
				}},
			}},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("auths len = %d, want 2", len(auths))
	}
	if auths[0].AccountKey != codexAccount.AccountKey {
		t.Fatalf("codex account_key = %q, want %q", auths[0].AccountKey, codexAccount.AccountKey)
	}
	if auths[0].Attributes["api_key"] != "sk-db-codex" || auths[0].Attributes["base_url"] != "https://db.example.com/v1" {
		t.Fatalf("codex auth not synthesized from db: %+v", auths[0].Attributes)
	}
	if auths[0].Attributes["header:X-DB"] != "1" {
		t.Fatalf("codex headers not synthesized from db: %+v", auths[0].Attributes)
	}
	if auths[1].AccountKey != compatAccount.AccountKey {
		t.Fatalf("compat account_key = %q, want %q", auths[1].AccountKey, compatAccount.AccountKey)
	}
	if auths[1].Provider != "deepseek" || auths[1].Attributes["api_key"] != "sk-db-ds" {
		t.Fatalf("compat auth not synthesized from db: provider=%s attrs=%+v", auths[1].Provider, auths[1].Attributes)
	}
}

func TestConfigSynthesizer_OpenAICompat(t *testing.T) {
	tests := []struct {
		name    string
		compat  []config.OpenAICompatibility
		wantLen int
	}{
		{
			name: "with APIKeyEntries",
			compat: []config.OpenAICompatibility{
				{
					Name:           "CustomProvider",
					BaseURL:        "https://custom.api.com",
					DisableCooling: true,
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-1"},
						{APIKey: "key-2"},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "empty APIKeyEntries included (legacy)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "EmptyKeys",
					BaseURL: "https://empty.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: ""},
						{APIKey: "   "},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "without APIKeyEntries (fallback)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "NoKeyProvider",
					BaseURL: "https://no-key.api.com",
				},
			},
			wantLen: 1,
		},
		{
			name: "empty name defaults",
			compat: []config.OpenAICompatibility{
				{
					Name:    "",
					BaseURL: "https://default.api.com",
				},
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					OpenAICompatibility: tt.compat,
				},
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}
			if tt.name == "with APIKeyEntries" {
				for i := range auths {
					if v, ok := auths[i].Metadata["disable_cooling"].(bool); !ok || !v {
						t.Fatalf("expected auth[%d].disable_cooling=true, got %v", i, auths[i].Metadata["disable_cooling"])
					}
					if auths[i].AccountKey != "openai-compatible:CustomProvider" {
						t.Fatalf("expected auth[%d].account_key openai-compatible:CustomProvider, got %q", i, auths[i].AccountKey)
					}
				}
			}
		})
	}
}

func TestConfigSynthesizer_VertexCompat(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key-123",
					BaseURL: "https://vertex.googleapis.com",
					Prefix:  "vertex-prod",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "vertex" {
		t.Errorf("expected provider vertex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "vertex-apikey" {
		t.Errorf("expected label vertex-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "vertex-prod" {
		t.Errorf("expected prefix vertex-prod, got %s", auths[0].Prefix)
	}
}

func TestConfigSynthesizer_VertexCompat_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "", BaseURL: "https://vertex.api"},   // empty key creates auth without api_key attr
				{APIKey: "  ", BaseURL: "https://vertex.api"}, // whitespace key creates auth without api_key attr
				{APIKey: "valid-key", BaseURL: "https://vertex.api", Headers: map[string]string{"X-Vertex": "test"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Vertex compat doesn't skip empty keys - it creates auths without api_key attribute
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	// First two should not have api_key attribute
	if _, ok := auths[0].Attributes["api_key"]; ok {
		t.Error("expected first auth to not have api_key attribute")
	}
	if _, ok := auths[1].Attributes["api_key"]; ok {
		t.Error("expected second auth to not have api_key attribute")
	}
	// Third should have headers
	if auths[2].Attributes["header:X-Vertex"] != "test" {
		t.Errorf("expected header:X-Vertex=test, got %s", auths[2].Attributes["header:X-Vertex"])
	}
}

func TestConfigSynthesizer_OpenAICompat_WithModelsHash(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "TestProvider",
					BaseURL: "https://test.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-with-models"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if auths[0].Attributes["api_key"] != "key-with-models" {
		t.Errorf("expected api_key key-with-models, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_OpenAICompat_FallbackWithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "NoKeyWithModels",
					BaseURL: "https://nokey.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-x"},
					},
					Headers: map[string]string{"X-API": "header-value"},
					// No APIKeyEntries - should use fallback path
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in fallback path")
	}
	if auths[0].Attributes["header:X-API"] != "header-value" {
		t.Errorf("expected header:X-API=header-value, got %s", auths[0].Attributes["header:X-API"])
	}
}

func TestConfigSynthesizer_VertexCompat_WithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key",
					BaseURL: "https://vertex.api",
					Models: []config.VertexCompatModel{
						{Name: "gemini-pro", Alias: "pro"},
						{Name: "gemini-ultra", Alias: "ultra"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in vertex auth with models")
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_AllProviders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key"},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key"},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key"},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "compat", BaseURL: "https://compat.api"},
			},
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "vertex-key", BaseURL: "https://vertex.api"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 5 {
		t.Fatalf("expected 5 auths, got %d", len(auths))
	}

	providers := make(map[string]bool)
	for _, a := range auths {
		providers[a.Provider] = true
	}

	expected := []string{"gemini", "claude", "codex", "compat", "vertex"}
	for _, p := range expected {
		if !providers[p] {
			t.Errorf("expected provider %s not found", p)
		}
	}
}
