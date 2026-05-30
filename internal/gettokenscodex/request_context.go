package gettokenscodex

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type RequestKind string

const (
	RequestKindMain RequestKind = "main"
)

const (
	MetadataKey                    = "codex_request_context"
	RequestKindMetadataKey         = "codex_request_kind"
	SessionIDMetadataKey           = "codex_session_id"
	ClientRequestIDMetadataKey     = "codex_client_request_id"
	ThreadIDMetadataKey            = "codex_thread_id"
	ThreadSourceMetadataKey        = "codex_thread_source"
	TurnIDMetadataKey              = "codex_turn_id"
	TurnStartedAtUnixMsMetadataKey = "codex_turn_started_at_unix_ms"
)

const (
	maxSessionIDLength       = 256
	maxClientRequestIDLength = 256
	maxThreadIDLength        = 256
	maxTurnIDLength          = 256
	maxSmallFieldLength      = 64
)

type RequestContext struct {
	RequestKind         RequestKind
	RequestedModel      string
	SessionID           string
	ClientRequestID     string
	ThreadID            string
	ThreadSource        string
	TurnID              string
	Sandbox             string
	TurnStartedAtUnixMs int64
	CodexWindowID       string
	ParentThreadID      string
	InstallationID      string
	PromptCacheKey      string
	MetadataParseError  string
}

type HeaderSet map[string]struct{}

func (s HeaderSet) Contains(key string) bool {
	_, ok := s[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func CodexResponsesClientContextHeaders() HeaderSet {
	headers := HeaderSet{}
	for _, key := range CodexResponsesClientContextHeaderKeys() {
		headers[strings.ToLower(key)] = struct{}{}
	}
	return headers
}

func CodexResponsesClientContextHeaderKeys() []string {
	return []string{
		"Version",
		"X-Codex-Installation-Id",
		"X-Codex-Turn-State",
		"X-Codex-Turn-Metadata",
		"X-Client-Request-Id",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Window-Id",
		"X-OpenAI-Subagent",
		"X-OpenAI-Memgen-Request",
		"X-OAI-Attestation",
		"Session-Id",
		"Thread-Id",
	}
}

func ExtractRequestContext(headers http.Header, body []byte, fallbackModel string) RequestContext {
	ctx := RequestContext{
		RequestKind:    RequestKindMain,
		RequestedModel: boundedString(fallbackModel, maxSessionIDLength),
	}
	if model := strings.TrimSpace(gjson.GetBytes(body, "model").String()); model != "" {
		ctx.RequestedModel = boundedString(model, maxSessionIDLength)
	}
	if headers != nil {
		ctx.SessionID = firstNonEmpty(
			boundedString(headerValue(headers, "Session_id"), maxSessionIDLength),
			boundedString(headerValue(headers, "Session-Id"), maxSessionIDLength),
		)
		ctx.ClientRequestID = boundedString(headerValue(headers, "X-Client-Request-Id"), maxClientRequestIDLength)
		ctx.ThreadID = boundedString(headerValue(headers, "Thread-Id"), maxThreadIDLength)
		ctx.CodexWindowID = boundedString(headerValue(headers, "X-Codex-Window-Id"), maxSessionIDLength)
		ctx.ParentThreadID = boundedString(headerValue(headers, "X-Codex-Parent-Thread-Id"), maxThreadIDLength)
		ctx.InstallationID = boundedString(headerValue(headers, "X-Codex-Installation-Id"), maxSessionIDLength)
		parseTurnMetadata(headerValue(headers, "X-Codex-Turn-Metadata"), &ctx)
	}
	if len(body) > 0 {
		ctx.PromptCacheKey = boundedString(gjson.GetBytes(body, "prompt_cache_key").String(), maxSessionIDLength)
		if ctx.CodexWindowID == "" {
			ctx.CodexWindowID = boundedString(gjson.GetBytes(body, "client_metadata.x-codex-window-id").String(), maxSessionIDLength)
		}
		if ctx.ClientRequestID == "" {
			ctx.ClientRequestID = boundedString(gjson.GetBytes(body, "client_metadata.x-client-request-id").String(), maxClientRequestIDLength)
		}
	}
	return ctx
}

func RequestContextFromMetadata(metadata map[string]any) *RequestContext {
	if metadata == nil {
		return nil
	}
	raw, ok := metadata[MetadataKey]
	if !ok {
		return nil
	}
	switch value := raw.(type) {
	case RequestContext:
		copy := value
		return &copy
	case *RequestContext:
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	default:
		return nil
	}
}

type turnMetadata struct {
	SessionID           string          `json:"session_id"`
	ThreadID            string          `json:"thread_id"`
	ThreadSource        string          `json:"thread_source"`
	TurnID              string          `json:"turn_id"`
	Sandbox             string          `json:"sandbox"`
	TurnStartedAtUnixMs json.RawMessage `json:"turn_started_at_unix_ms"`
}

func parseTurnMetadata(raw string, ctx *RequestContext) {
	raw = strings.TrimSpace(raw)
	if raw == "" || ctx == nil {
		return
	}
	var metadata turnMetadata
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil {
		ctx.MetadataParseError = "invalid_json"
		return
	}
	if ctx.SessionID == "" {
		ctx.SessionID = boundedString(metadata.SessionID, maxSessionIDLength)
	}
	if ctx.ThreadID == "" {
		ctx.ThreadID = boundedString(metadata.ThreadID, maxThreadIDLength)
	}
	ctx.ThreadSource = boundedString(metadata.ThreadSource, maxSmallFieldLength)
	ctx.TurnID = boundedString(metadata.TurnID, maxTurnIDLength)
	ctx.Sandbox = boundedString(metadata.Sandbox, maxSmallFieldLength)
	if len(metadata.TurnStartedAtUnixMs) > 0 {
		var number json.Number
		if err := json.Unmarshal(metadata.TurnStartedAtUnixMs, &number); err == nil {
			if value, err := number.Int64(); err == nil {
				ctx.TurnStartedAtUnixMs = value
			}
		}
	}
}

func headerValue(headers http.Header, key string) string {
	if headers == nil {
		return ""
	}
	if val := strings.TrimSpace(headers.Get(key)); val != "" {
		return val
	}
	for existingKey, values := range headers {
		if !strings.EqualFold(existingKey, key) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func boundedString(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
