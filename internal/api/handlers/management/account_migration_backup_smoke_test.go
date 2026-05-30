package management

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAccountMigrationBackupFixtureEndToEnd(t *testing.T) {
	backupDir := strings.TrimSpace(os.Getenv("GETTOKENS_ACCOUNT_MIGRATION_BACKUP_DIR"))
	if backupDir == "" {
		t.Skip("set GETTOKENS_ACCOUNT_MIGRATION_BACKUP_DIR to run local backup fixture smoke")
	}
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}

	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	codexKeyDir := filepath.Join(root, "codex-api-keys")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.MkdirAll(codexKeyDir, 0700); err != nil {
		t.Fatalf("mkdir codex key dir: %v", err)
	}

	authFileCount := 0
	codexAPIKeyCount := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(backupDir, entry.Name()))
		if err != nil {
			t.Fatalf("read backup file %s: %v", entry.Name(), err)
		}
		targetDir := authDir
		if strings.HasPrefix(strings.ToLower(entry.Name()), "codex-api-key-") {
			targetDir = codexKeyDir
			codexAPIKeyCount++
		} else if strings.HasPrefix(strings.ToLower(entry.Name()), "codex-") {
			authFileCount++
		} else {
			continue
		}
		if err := os.WriteFile(filepath.Join(targetDir, entry.Name()), data, 0600); err != nil {
			t.Fatalf("copy backup file %s: %v", entry.Name(), err)
		}
	}
	if authFileCount == 0 || codexAPIKeyCount == 0 {
		t.Fatalf("backup fixture needs auth-file and codex-api-key samples, got auth=%d api-key=%d", authFileCount, codexAPIKeyCount)
	}

	ctx := context.Background()
	report, err := accountstore.DryRunLegacyImport(ctx, accountstore.LegacySources{
		AuthDir:              authDir,
		CodexAPIKeyStoreDirs: []string{codexKeyDir},
	})
	if err != nil {
		t.Fatalf("DryRunLegacyImport: %v", err)
	}
	if got, want := countMigrationCandidates(report.Candidates, accountstore.KindAuthFile), authFileCount; got != want {
		t.Fatalf("auth-file candidates = %d, want %d", got, want)
	}
	if got, want := countMigrationCandidates(report.Candidates, accountstore.KindCodexAPIKey), codexAPIKeyCount; got != want {
		t.Fatalf("codex-api-key candidates = %d, want %d", got, want)
	}

	store, err := accountstore.Open(filepath.Join(root, "accounts-v1.sqlite"))
	if err != nil {
		t.Fatalf("open account store: %v", err)
	}
	defer store.Close()
	commit, err := store.CommitImport(ctx, report)
	if err != nil {
		t.Fatalf("CommitImport: %v", err)
	}
	if commit.Imported != authFileCount+codexAPIKeyCount || commit.Skipped != 0 || len(commit.Errors) != 0 {
		t.Fatalf("unexpected commit result: imported=%d skipped=%d errors=%d", commit.Imported, commit.Skipped, len(commit.Errors))
	}
	accounts, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != authFileCount+codexAPIKeyCount {
		t.Fatalf("accounts = %d, want %d", len(accounts), authFileCount+codexAPIKeyCount)
	}

	cfg := &config.Config{AccountStoreDB: filepath.Join(root, "accounts-v1.sqlite")}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{
		Config:      cfg,
		Now:         time.Now(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(auths) != authFileCount+codexAPIKeyCount {
		t.Fatalf("runtime auths = %d, want %d", len(auths), authFileCount+codexAPIKeyCount)
	}

	manager := coreauth.NewManager(nil, &coreauth.FillFirstSelector{}, nil)
	exec := &backupRouteExecutor{}
	manager.RegisterExecutor(exec)
	var firstOAuthAuth *coreauth.Auth
	for _, auth := range auths {
		if auth.AccountKey == "" {
			t.Fatalf("runtime auth %s missing account key", auth.ID)
		}
		if _, err := manager.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("register runtime auth: %v", err)
		}
		if firstOAuthAuth == nil && auth.Provider == "codex" && tokenValueFromMetadata(auth.Metadata) != "" {
			firstOAuthAuth = auth
		}
	}
	if firstOAuthAuth == nil {
		t.Fatal("no routable codex auth-file with token synthesized")
	}

	opts := cliproxyexecutor.Options{Metadata: map[string]any{}}
	if _, err := manager.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: ""}, opts); err != nil {
		t.Fatalf("Execute route selection: %v", err)
	}
	if exec.authID == "" {
		t.Fatal("route executor did not receive selected auth")
	}

	var gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &Handler{authManager: manager, cfg: cfg}
	router.POST("/v0/management/api-call", handler.APICall)
	body, _ := json.Marshal(map[string]any{
		"auth_index": firstOAuthAuth.AccountKey,
		"method":     http.MethodGet,
		"url":        upstream.URL,
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
		},
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/management/api-call", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("api-call status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(gotAuthorization, "Bearer ") || gotAuthorization == "Bearer $TOKEN$" {
		t.Fatalf("api-call did not substitute token")
	}
}

func countMigrationCandidates(candidates []accountstore.ImportCandidate, kind accountstore.AccountKind) int {
	count := 0
	for _, candidate := range candidates {
		if candidate.Kind == kind {
			count++
		}
	}
	return count
}

type backupRouteExecutor struct {
	authID string
}

func (e *backupRouteExecutor) Identifier() string { return "codex" }

func (e *backupRouteExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if auth != nil {
		e.authID = auth.ID
	}
	return cliproxyexecutor.Response{}, nil
}

func (e *backupRouteExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *backupRouteExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *backupRouteExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *backupRouteExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, nil
}
