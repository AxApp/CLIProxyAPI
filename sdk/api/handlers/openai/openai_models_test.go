package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestOpenAIModelsReturnsOpenAIListByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelRegistry := registry.GetGlobalRegistry()
	authID := "test-openai-models-default"
	modelRegistry.RegisterClient(authID, "openai", []*registry.ModelInfo{{
		ID:      "gpt-5.5",
		Object:  "model",
		Created: 1776902400,
		OwnedBy: "openai",
		Type:    "openai",
	}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(authID)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", h.OpenAIModels)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["object"] != "list" {
		t.Fatalf("object = %#v, want %q", payload["object"], "list")
	}
	if _, ok := payload["data"]; !ok {
		t.Fatalf("expected OpenAI payload to include data field")
	}
	if _, ok := payload["models"]; ok {
		t.Fatalf("did not expect OpenAI payload to include models field")
	}
}

func TestOpenAIModelsReturnsCodexCatalogForClientVersionRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)

	modelRegistry := registry.GetGlobalRegistry()
	authID := "test-openai-models-codex"
	modelRegistry.RegisterClient(authID, "openai", []*registry.ModelInfo{{
		ID:            "gpt-5.5",
		Object:        "model",
		Created:       1776902400,
		OwnedBy:       "openai",
		Type:          "openai",
		DisplayName:   "gpt-5.5",
		Description:   "Latest coding model",
		ContextLength: 400000,
	}})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(authID)
	})

	manager := coreauth.NewManager(nil, nil, nil)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIAPIHandler(base)
	router := gin.New()
	router.GET("/v1/models", h.OpenAIModels)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.124.0", nil)
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}

	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Models) != 1 {
		t.Fatalf("models length = %d, want 1", len(payload.Models))
	}
	model := payload.Models[0]
	if model["slug"] != "gpt-5.5" {
		t.Fatalf("slug = %#v, want %q", model["slug"], "gpt-5.5")
	}
	if model["shell_type"] != "shell_command" {
		t.Fatalf("shell_type = %#v, want %q", model["shell_type"], "shell_command")
	}
	if model["visibility"] != "list" {
		t.Fatalf("visibility = %#v, want %q", model["visibility"], "list")
	}
	if model["base_instructions"] == "" {
		t.Fatalf("expected base_instructions to be non-empty")
	}
	if _, ok := model["supported_reasoning_levels"]; !ok {
		t.Fatalf("expected supported_reasoning_levels in Codex payload")
	}
	if _, ok := model["context_window"]; !ok {
		t.Fatalf("expected context_window in Codex payload")
	}
}
