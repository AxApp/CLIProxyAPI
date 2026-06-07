package executor

import (
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func authFormatBaseURL(auth *cliproxyauth.Auth, format string) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	if value := strings.TrimSpace(auth.Attributes["format_base_url:"+format]); value != "" {
		return value
	}
	return strings.TrimSpace(auth.Attributes["base_url"])
}
