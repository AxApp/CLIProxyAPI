//go:build darwin

package proxyutil

import (
	"net/http"
	"testing"
	"time"
)

func TestParseDarwinSystemProxyConfig(t *testing.T) {
	t.Parallel()

	cfg := parseDarwinSystemProxyConfig([]byte(`<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : ocsp.digicert.com
  }
  HTTPEnable : 1
  HTTPPort : 9090
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 9090
  HTTPSProxy : 127.0.0.1
}`))

	if !cfg.http.enabled || cfg.http.host != "127.0.0.1" || cfg.http.port != 9090 {
		t.Fatalf("http proxy = %#v, want enabled 127.0.0.1:9090", cfg.http)
	}
	if !cfg.https.enabled || cfg.https.host != "127.0.0.1" || cfg.https.port != 9090 {
		t.Fatalf("https proxy = %#v, want enabled 127.0.0.1:9090", cfg.https)
	}
	if !darwinProxyBypass(cfg.exceptions, "api.local") {
		t.Fatal("expected *.local to bypass proxy")
	}
	if !darwinProxyBypass(cfg.exceptions, "ocsp.digicert.com") {
		t.Fatal("expected exact exception to bypass proxy")
	}
}

func TestProxyFromSystemUsesDarwinSystemProxyWhenEnvironmentIsEmpty(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")

	darwinSystemProxyCache.Lock()
	darwinSystemProxyCache.config = darwinSystemProxyConfig{
		https: darwinProxyEndpoint{enabled: true, host: "127.0.0.1", port: 9090},
	}
	darwinSystemProxyCache.expiresAt = time.Now().Add(darwinSystemProxyCacheTTL)
	darwinSystemProxyCache.Unlock()

	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	proxyURL, err := ProxyFromSystem(req)
	if err != nil {
		t.Fatalf("ProxyFromSystem() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:9090" {
		t.Fatalf("proxy URL = %v, want http://127.0.0.1:9090", proxyURL)
	}
}
