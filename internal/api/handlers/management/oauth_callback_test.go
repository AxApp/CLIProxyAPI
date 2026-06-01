package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestPostOAuthCallbackCreatesMissingAuthDir(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := filepath.Join(t.TempDir(), "missing-auth")
	state := "test-antigravity-state"
	RegisterOAuthSession(state, "antigravity")
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)

	body := `{"provider":"antigravity","redirect_url":"http://localhost:59788/oauth-callback?state=test-antigravity-state&code=test-code"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	callbackPath := filepath.Join(authDir, ".oauth-antigravity-"+state+".oauth")
	payload := readOAuthCallbackPayload(t, callbackPath)
	if payload.State != state || payload.Code != "test-code" || payload.Error != "" {
		t.Fatalf("unexpected callback payload: %+v", payload)
	}
}

func TestWriteOAuthCallbackFileForPendingSessionCreatesMissingAuthDirForCallbackProviders(t *testing.T) {
	providers := []string{"anthropic", "codex", "gemini", "antigravity", "xai"}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			authDir := filepath.Join(t.TempDir(), "missing-auth")
			state := provider + "-state"
			RegisterOAuthSession(state, provider)
			defer CompleteOAuthSession(state)

			path, err := WriteOAuthCallbackFileForPendingSession(authDir, provider, state, "code-"+provider, "")
			if err != nil {
				t.Fatalf("WriteOAuthCallbackFileForPendingSession() error = %v", err)
			}

			payload := readOAuthCallbackPayload(t, path)
			if payload.State != state || payload.Code != "code-"+provider || payload.Error != "" {
				t.Fatalf("unexpected callback payload: %+v", payload)
			}
		})
	}
}

func TestPostOAuthCallbackNonPendingSessionKeepsConflictSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	state := "completed-state"
	RegisterOAuthSession(state, "codex")
	SetOAuthSessionError(state, "Authentication failed")
	defer CompleteOAuthSession(state)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)

	body := `{"provider":"codex","state":"completed-state","code":"test-code"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Authentication failed") {
		t.Fatalf("conflict body did not preserve session status: %s", w.Body.String())
	}
}

func TestPostOAuthCallbackLogsPersistFailureWithoutLeakingRedirectSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)

	authDirParent := t.TempDir()
	authDir := filepath.Join(authDirParent, "auth-file")
	if err := os.WriteFile(authDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("failed to create authDir blocker: %v", err)
	}

	state := "persist-failure-state"
	RegisterOAuthSession(state, "gemini")
	defer CompleteOAuthSession(state)

	var logs bytes.Buffer
	previousOut := log.StandardLogger().Out
	log.SetOutput(&logs)
	defer log.SetOutput(previousOut)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	router := gin.New()
	router.POST("/v0/management/oauth-callback", h.PostOAuthCallback)

	redirectURL := "http://localhost:59788/oauth2callback?state=persist-failure-state&code=secret-code-never-log"
	body := `{"provider":"gemini","redirect_url":"` + redirectURL + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "failed to persist oauth callback") {
		t.Fatalf("response body changed API error semantics: %s", w.Body.String())
	}

	logText := logs.String()
	if !strings.Contains(logText, "failed to persist oauth callback") {
		t.Fatalf("expected persist failure to be logged, got logs %q", logText)
	}
	if strings.Contains(logText, "secret-code-never-log") || strings.Contains(logText, redirectURL) {
		t.Fatalf("persist failure log leaked redirect secret: %q", logText)
	}
}

func readOAuthCallbackPayload(t *testing.T, path string) oauthCallbackFilePayload {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected callback file at %s: %v", path, err)
	}
	var payload oauthCallbackFilePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("failed to decode callback payload: %v", err)
	}
	return payload
}
