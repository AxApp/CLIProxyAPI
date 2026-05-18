//go:build darwin

package proxyutil

import (
	"bytes"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const darwinSystemProxyCacheTTL = 5 * time.Second

type darwinProxyEndpoint struct {
	enabled bool
	host    string
	port    int
}

type darwinSystemProxyConfig struct {
	http       darwinProxyEndpoint
	https      darwinProxyEndpoint
	exceptions []string
}

var darwinSystemProxyCache = struct {
	sync.Mutex
	expiresAt time.Time
	config    darwinSystemProxyConfig
}{}

func proxyFromOperatingSystem(target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, nil
	}
	cfg := currentDarwinSystemProxyConfig()
	if darwinProxyBypass(cfg.exceptions, target.Hostname()) {
		return nil, nil
	}

	endpoint := darwinProxyEndpoint{}
	switch strings.ToLower(target.Scheme) {
	case "http", "ws":
		endpoint = cfg.http
	case "https", "wss":
		endpoint = cfg.https
	default:
		return nil, nil
	}
	if !endpoint.enabled || endpoint.host == "" || endpoint.port <= 0 {
		return nil, nil
	}
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(endpoint.host, strconv.Itoa(endpoint.port)),
	}, nil
}

func currentDarwinSystemProxyConfig() darwinSystemProxyConfig {
	now := time.Now()
	darwinSystemProxyCache.Lock()
	if now.Before(darwinSystemProxyCache.expiresAt) {
		cfg := darwinSystemProxyCache.config
		darwinSystemProxyCache.Unlock()
		return cfg
	}
	darwinSystemProxyCache.Unlock()

	cfg := readDarwinSystemProxyConfig()

	darwinSystemProxyCache.Lock()
	darwinSystemProxyCache.config = cfg
	darwinSystemProxyCache.expiresAt = now.Add(darwinSystemProxyCacheTTL)
	darwinSystemProxyCache.Unlock()
	return cfg
}

func readDarwinSystemProxyConfig() darwinSystemProxyConfig {
	output, err := exec.Command("scutil", "--proxy").Output()
	if err != nil {
		return darwinSystemProxyConfig{}
	}
	return parseDarwinSystemProxyConfig(output)
}

func parseDarwinSystemProxyConfig(output []byte) darwinSystemProxyConfig {
	cfg := darwinSystemProxyConfig{}
	lines := bytes.Split(output, []byte{'\n'})
	for _, rawLine := range lines {
		key, value, ok := parseDarwinScutilLine(string(rawLine))
		if !ok {
			continue
		}
		switch key {
		case "HTTPEnable":
			cfg.http.enabled = value == "1"
		case "HTTPProxy":
			cfg.http.host = value
		case "HTTPPort":
			cfg.http.port, _ = strconv.Atoi(value)
		case "HTTPSEnable":
			cfg.https.enabled = value == "1"
		case "HTTPSProxy":
			cfg.https.host = value
		case "HTTPSPort":
			cfg.https.port, _ = strconv.Atoi(value)
		default:
			if _, err := strconv.Atoi(key); err == nil && value != "" {
				cfg.exceptions = append(cfg.exceptions, value)
			}
		}
	}
	return cfg
}

func parseDarwinScutilLine(line string) (string, string, bool) {
	left, right, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok {
		return "", "", false
	}
	key := strings.TrimSpace(left)
	value := strings.TrimSpace(right)
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

func darwinProxyBypass(exceptions []string, hostname string) bool {
	host := strings.ToLower(strings.TrimSpace(hostname))
	if host == "" {
		return false
	}
	for _, rawPattern := range exceptions {
		pattern := strings.ToLower(strings.TrimSpace(rawPattern))
		if pattern == "" {
			continue
		}
		if pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, strings.TrimPrefix(pattern, "*")) {
			return true
		}
	}
	return false
}
