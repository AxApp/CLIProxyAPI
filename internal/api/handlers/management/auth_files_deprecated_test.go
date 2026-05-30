package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestDeprecatedAuthFilesEndpointReturnsGone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)

	router := gin.New()
	router.GET("/v0/management/auth-files", h.DeprecatedAuthFilesEndpoint)
	router.POST("/v0/management/auth-files", h.DeprecatedAuthFilesEndpoint)
	router.GET("/v0/management/auth-files/download", h.DeprecatedAuthFilesEndpoint)
	router.GET("/v0/management/auth-files/models", h.DeprecatedAuthFilesEndpoint)
	router.PATCH("/v0/management/auth-files/status", h.DeprecatedAuthFilesEndpoint)
	router.PATCH("/v0/management/auth-files/fields", h.DeprecatedAuthFilesEndpoint)
	router.DELETE("/v0/management/auth-files", h.DeprecatedAuthFilesEndpoint)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v0/management/auth-files"},
		{http.MethodPost, "/v0/management/auth-files"},
		{http.MethodGet, "/v0/management/auth-files/download"},
		{http.MethodGet, "/v0/management/auth-files/models"},
		{http.MethodPatch, "/v0/management/auth-files/status"},
		{http.MethodPatch, "/v0/management/auth-files/fields"},
		{http.MethodDelete, "/v0/management/auth-files"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != http.StatusGone {
			t.Fatalf("%s %s status = %d body=%s", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "/v0/management/accounts") {
			t.Fatalf("%s %s body missing replacement: %s", tc.method, tc.path, recorder.Body.String())
		}
	}
}
