package logging

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

func TestGinLogrusRecoveryRepanicsErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/abort", func(c *gin.Context) {
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected panic, got nil")
		}
		err, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T", recovered)
		}
		if !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("expected ErrAbortHandler, got %v", err)
		}
		if err != http.ErrAbortHandler {
			t.Fatalf("expected exact ErrAbortHandler sentinel, got %v", err)
		}
	}()

	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestIsAIAPIPathIncludesImages(t *testing.T) {
	if !isAIAPIPath("/v1/images/generations") {
		t.Fatalf("expected /v1/images/generations to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/images/edits") {
		t.Fatalf("expected /v1/images/edits to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos") {
		t.Fatalf("expected /v1/videos to be treated as AI API path")
	}
	if !isAIAPIPath("/v1/videos/video_123") {
		t.Fatalf("expected /v1/videos/video_123 to be treated as AI API path")
	}
}

func TestGinLogrusLoggerRedactsAccountKeyQueryValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logs bytes.Buffer
	logger := log.StandardLogger()
	previousOut := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(previousOut)

	engine := gin.New()
	engine.Use(GinLogrusLogger())
	engine.GET("/v0/management/gettokens/quota-status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/v0/management/gettokens/quota-status?account_keys=acct_a,acct_b,acct_c&window=24h", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	output := logs.String()
	if strings.Contains(output, "acct_a") || strings.Contains(output, "acct_b") || strings.Contains(output, "acct_c") {
		t.Fatalf("access log leaked account keys: %s", output)
	}
	if !strings.Contains(output, "account_keys=%5Bredacted%3A3%5D") {
		t.Fatalf("access log should include bounded account key summary, got %s", output)
	}
	if !strings.Contains(output, "window=24h") {
		t.Fatalf("access log should preserve unrelated query values, got %s", output)
	}
}
