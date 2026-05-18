package auth

import (
	"context"
	"strconv"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// WebsocketsAllowedForRequest reports whether the current request may use an upstream websocket.
func WebsocketsAllowedForRequest(ctx context.Context, auth *Auth) bool {
	return cliproxyexecutor.DownstreamWebsocket(ctx) && AuthAllowsWebsockets(auth)
}

// AuthAllowsWebsockets reports the static websocket capability for an auth entry.
func AuthAllowsWebsockets(auth *Auth) bool {
	if auth == nil {
		return false
	}
	return AuthDataAllowsWebsockets(auth.Provider, auth.FileName, auth.Attributes, auth.Metadata)
}

// AuthDataAllowsWebsockets evaluates websocket capability for auth data without mutating it.
func AuthDataAllowsWebsockets(provider string, fileName string, attributes map[string]string, metadata map[string]any) bool {
	if enabled, ok := explicitWebsocketsSetting(attributes, metadata); ok {
		return enabled
	}
	return isCodexAuthFileData(provider, fileName, attributes, metadata)
}

func authWebsocketsEnabled(auth *Auth) bool {
	return AuthAllowsWebsockets(auth)
}

func explicitWebsocketsSetting(attributes map[string]string, metadata map[string]any) (bool, bool) {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed, true
			}
		}
	}
	if len(metadata) == 0 {
		return false, false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false, false
	}
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed, true
		}
	default:
	}
	return false, false
}

func isCodexAuthFileData(provider string, fileName string, attributes map[string]string, metadata map[string]any) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return false
	}
	if metadata != nil {
		if t, _ := metadata["type"].(string); strings.EqualFold(strings.TrimSpace(t), "codex") {
			return true
		}
	}
	if len(attributes) > 0 {
		if strings.TrimSpace(attributes["api_key"]) != "" {
			return false
		}
		for _, key := range []string{"path", "source"} {
			value := strings.TrimSpace(attributes[key])
			if value == "" || strings.HasPrefix(strings.ToLower(value), "config:") {
				continue
			}
			if strings.HasSuffix(strings.ToLower(value), ".json") {
				return true
			}
		}
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(fileName)), ".json")
}
