package cmd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	gettokenshooks "github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenshooks"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestInstallGetTokensHooksInstallsRoutePolicy(t *testing.T) {
	if err := installGetTokensHooks(&config.Config{}, t.TempDir()); err != nil {
		t.Fatalf("install hooks: %v", err)
	}

	mgr := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := &captureExecutor{identifier: "codex"}
	mgr.RegisterExecutor(exec)

	ctx := context.Background()
	if _, err := mgr.Register(ctx, &coreauth.Auth{ID: "auth-a", Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register auth-a: %v", err)
	}
	if _, err := mgr.Register(ctx, &coreauth.Auth{ID: "auth-b", Provider: "codex", Status: coreauth.StatusActive}); err != nil {
		t.Fatalf("register auth-b: %v", err)
	}

	_, err := mgr.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{
		Metadata: gettokenshooks.RouteMetadata(nil, []string{"auth-a"}, nil, nil),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := exec.Selected(); got != "auth-b" {
		t.Fatalf("selected auth = %q, want auth-b", got)
	}
}

func TestInstallGetTokensHooksCreatesUsageLedgerWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{UsageStatisticsEnabled: true}

	if err := installGetTokensHooks(cfg, configPath); err != nil {
		t.Fatalf("install hooks: %v", err)
	}

	ledgerPath := filepath.Join(dir, "usage-attribution-v1.sqlite")
	if _, err := os.Stat(ledgerPath); err != nil {
		t.Fatalf("usage attribution ledger missing at %s: %v", ledgerPath, err)
	}
}

type captureExecutor struct {
	identifier string
	mu         sync.Mutex
	selected   []string
}

func (e *captureExecutor) Identifier() string {
	return e.identifier
}

func (e *captureExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_ = ctx
	_ = req
	_ = opts
	e.mu.Lock()
	e.selected = append(e.selected, auth.ID)
	e.mu.Unlock()
	return cliproxyexecutor.Response{}, nil
}

func (e *captureExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	_, _ = ctx, auth
	_, _ = req, opts
	return nil, nil
}

func (e *captureExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	_, _ = ctx, auth
	return auth, nil
}

func (e *captureExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	_, _ = ctx, auth
	_, _ = req, opts
	return cliproxyexecutor.Response{}, nil
}

func (e *captureExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	_, _ = ctx, auth
	_ = req
	return nil, nil
}

func (e *captureExecutor) Selected() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.selected) == 0 {
		return ""
	}
	return e.selected[len(e.selected)-1]
}
