package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const websocketCircuitOpenUntilMetadataKey = "websocket_circuit_open_until"

// WebsocketsAllowedForRequest reports whether the current request may use an upstream websocket.
func WebsocketsAllowedForRequest(ctx context.Context, auth *Auth) bool {
	return cliproxyexecutor.DownstreamWebsocket(ctx) && AuthAllowsWebsockets(auth)
}

// AuthAllowsWebsockets reports the static websocket capability for an auth entry.
func AuthAllowsWebsockets(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if AuthWebsocketCircuitOpen(auth, time.Now()) {
		return false
	}
	return AuthDataAllowsWebsockets(auth.Provider, auth.FileName, auth.Attributes, auth.Metadata)
}

// MarkAuthWebsocketCircuitOpen temporarily disables websocket transport for an auth in memory.
func MarkAuthWebsocketCircuitOpen(auth *Auth, until time.Time) {
	if auth == nil || until.IsZero() {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	auth.Metadata[websocketCircuitOpenUntilMetadataKey] = until.UTC().Format(time.RFC3339Nano)
}

// AuthWebsocketCircuitOpen reports whether an auth is temporarily barred from websocket transport.
func AuthWebsocketCircuitOpen(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.Metadata) == 0 {
		return false
	}
	raw, ok := auth.Metadata[websocketCircuitOpenUntilMetadataKey]
	if !ok || raw == nil {
		return false
	}
	var until time.Time
	switch value := raw.(type) {
	case time.Time:
		until = value
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err == nil {
			until = parsed
		}
	}
	return !until.IsZero() && until.After(now)
}

// AuthDataAllowsWebsockets evaluates websocket capability for auth data without mutating it.
func AuthDataAllowsWebsockets(provider string, fileName string, attributes map[string]string, metadata map[string]any) bool {
	if enabled, ok := explicitWebsocketsSetting(attributes, metadata); ok {
		return enabled
	}
	return strings.EqualFold(strings.TrimSpace(provider), "codex")
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
