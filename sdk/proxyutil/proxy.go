package proxyutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// Mode describes how a proxy setting should be interpreted.
type Mode int

const (
	// ModeInherit means no explicit proxy behavior was configured.
	ModeInherit Mode = iota
	// ModeDirect means outbound requests must bypass proxies explicitly.
	ModeDirect
	// ModeProxy means a concrete proxy URL was configured.
	ModeProxy
	// ModeInvalid means the proxy setting is present but malformed or unsupported.
	ModeInvalid
)

// Setting is the normalized interpretation of a proxy configuration value.
type Setting struct {
	Raw  string
	Mode Mode
	URL  *url.URL
}

type dialerFunc func(network, addr string) (net.Conn, error)

func (f dialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(network, addr)
}

// Parse normalizes a proxy configuration value into inherit, direct, or proxy modes.
func Parse(raw string) (Setting, error) {
	trimmed := strings.TrimSpace(raw)
	setting := Setting{Raw: trimmed}

	if trimmed == "" {
		setting.Mode = ModeInherit
		return setting, nil
	}

	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		setting.Mode = ModeDirect
		return setting, nil
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("parse proxy URL failed: %w", errParse)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("proxy URL missing scheme/host")
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h", "http", "https":
		setting.Mode = ModeProxy
		setting.URL = parsedURL
		return setting, nil
	default:
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

func cloneDefaultTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		return transport.Clone()
	}
	return &http.Transport{}
}

// NewDirectTransport returns a transport that bypasses environment proxies.
func NewDirectTransport() *http.Transport {
	clone := cloneDefaultTransport()
	clone.Proxy = nil
	return clone
}

// NewSystemTransport returns a transport that follows environment proxies and,
// on supported platforms, the operating system proxy configuration.
func NewSystemTransport() *http.Transport {
	clone := cloneDefaultTransport()
	clone.Proxy = ProxyFromSystem
	return clone
}

// ProxyFromSystem resolves the proxy for req from standard environment
// variables first, then from the operating system proxy settings when available.
func ProxyFromSystem(req *http.Request) (*url.URL, error) {
	if req == nil {
		return nil, nil
	}
	if proxyURL, errProxy := http.ProxyFromEnvironment(req); errProxy != nil || proxyURL != nil {
		return proxyURL, errProxy
	}
	return proxyFromOperatingSystem(req.URL)
}

// BuildSystemDialer constructs a connection-layer dialer that resolves the
// current environment or operating-system proxy at dial time.
func BuildSystemDialer(targetScheme string) proxy.Dialer {
	scheme := strings.TrimSpace(targetScheme)
	if scheme == "" {
		scheme = "https"
	}
	return dialerFunc(func(network, addr string) (net.Conn, error) {
		targetURL := &url.URL{Scheme: scheme, Host: addr}
		proxyURL, errProxy := ProxyFromSystem(&http.Request{URL: targetURL})
		if errProxy != nil {
			return nil, errProxy
		}
		if proxyURL == nil {
			return proxy.Direct.Dial(network, addr)
		}
		proxyDialer, mode, errBuild := BuildDialer(proxyURL.String())
		if errBuild != nil {
			return nil, errBuild
		}
		if mode == ModeDirect || mode == ModeInherit || proxyDialer == nil {
			return proxy.Direct.Dial(network, addr)
		}
		return proxyDialer.Dial(network, addr)
	})
}

// BuildHTTPTransport constructs an HTTP transport for the provided proxy setting.
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return NewDirectTransport(), setting.Mode, nil
	case ModeProxy:
		if setting.URL.Scheme == "socks5" || setting.URL.Scheme == "socks5h" {
			var proxyAuth *proxy.Auth
			if setting.URL.User != nil {
				username := setting.URL.User.Username()
				password, _ := setting.URL.User.Password()
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
			if errSOCKS5 != nil {
				return nil, setting.Mode, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
			}
			transport := cloneDefaultTransport()
			transport.Proxy = nil
			transport.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
			return transport, setting.Mode, nil
		}
		transport := cloneDefaultTransport()
		transport.Proxy = http.ProxyURL(setting.URL)
		return transport, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

// BuildDialer constructs a proxy dialer for settings that operate at the connection layer.
func BuildDialer(raw string) (proxy.Dialer, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, setting.Mode, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return proxy.Direct, setting.Mode, nil
	case ModeProxy:
		switch setting.URL.Scheme {
		case "http", "https":
			return httpConnectDialer{proxyURL: setting.URL}, setting.Mode, nil
		default:
			dialer, errDialer := proxy.FromURL(setting.URL, proxy.Direct)
			if errDialer != nil {
				return nil, setting.Mode, fmt.Errorf("create proxy dialer failed: %w", errDialer)
			}
			return dialer, setting.Mode, nil
		}
	default:
		return nil, setting.Mode, nil
	}
}

type httpConnectDialer struct {
	proxyURL *url.URL
}

func (d httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	if d.proxyURL == nil {
		return proxy.Direct.Dial(network, addr)
	}
	conn, errDial := proxy.Direct.Dial(network, httpProxyAddress(d.proxyURL))
	if errDial != nil {
		return nil, errDial
	}

	if d.proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxyURL.Hostname()})
		if errHandshake := tlsConn.Handshake(); errHandshake != nil {
			_ = conn.Close()
			return nil, errHandshake
		}
		conn = tlsConn
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	request.Header.Set("Proxy-Connection", "Keep-Alive")
	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	}
	if errWrite := request.Write(conn); errWrite != nil {
		_ = conn.Close()
		return nil, errWrite
	}

	response, errRead := http.ReadResponse(bufio.NewReader(conn), request)
	if errRead != nil {
		_ = conn.Close()
		return nil, errRead
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", response.Status)
	}

	return conn, nil
}

func httpProxyAddress(proxyURL *url.URL) string {
	if proxyURL == nil {
		return ""
	}
	if proxyURL.Port() != "" {
		return proxyURL.Host
	}
	switch proxyURL.Scheme {
	case "https":
		return net.JoinHostPort(proxyURL.Hostname(), "443")
	default:
		return net.JoinHostPort(proxyURL.Hostname(), "80")
	}
}
