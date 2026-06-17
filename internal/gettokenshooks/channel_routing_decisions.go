package gettokenshooks

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type ChannelRoutingDecisionListResponse struct {
	Items []ChannelRoutingDecisionSnapshot `json:"items"`
}

type ChannelRoutingDecisionSnapshot struct {
	ID                   string                            `json:"id"`
	RecordedAt           string                            `json:"recordedAt"`
	Channel              string                            `json:"channel"`
	Providers            []string                          `json:"providers,omitempty"`
	Model                string                            `json:"model,omitempty"`
	ProjectKey           string                            `json:"projectKey,omitempty"`
	ProjectName          string                            `json:"projectName,omitempty"`
	ProjectKeySource     string                            `json:"projectKeySource,omitempty"`
	ProjectKeyConfidence string                            `json:"projectKeyConfidence,omitempty"`
	ProjectMatchKeys     []string                          `json:"projectMatchKeys,omitempty"`
	Source               string                            `json:"source,omitempty"`
	CandidateCount       int                               `json:"candidateCount"`
	Candidates           []ChannelRoutingDecisionCandidate `json:"candidates"`
	SelectedAuthID       string                            `json:"selectedAuthID,omitempty"`
	SelectedAccountID    string                            `json:"selectedAccountID,omitempty"`
	SelectedProvider     string                            `json:"selectedProvider,omitempty"`
	UnavailableCode      string                            `json:"unavailableCode,omitempty"`
	UnavailableMessage   string                            `json:"unavailableMessage,omitempty"`
	Trace                []ChannelRoutingDecisionStep      `json:"trace"`
	DroppedReasons       []ChannelRoutingDroppedReason     `json:"droppedReasons,omitempty"`
}

type ChannelRoutingDecisionCandidate struct {
	AuthID    string `json:"authID,omitempty"`
	AccountID string `json:"accountID,omitempty"`
	Provider  string `json:"provider,omitempty"`
}

type ChannelRoutingDecisionStep struct {
	Stage     gettokensrouting.PolicyStage `json:"stage"`
	Policy    string                       `json:"policy,omitempty"`
	Reason    string                       `json:"reason,omitempty"`
	Before    int                          `json:"before"`
	After     int                          `json:"after"`
	AllowIDs  []string                     `json:"allowIDs,omitempty"`
	DenyIDs   []string                     `json:"denyIDs,omitempty"`
	OrderIDs  []string                     `json:"orderIDs,omitempty"`
	Fallback  *bool                        `json:"fallback,omitempty"`
	Activated bool                         `json:"activated"`
}

func ConfigureChannelRoutingDecisionRoutes(group *gin.RouterGroup, _ *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.GET("/gettokens/channel-routing/decisions", func(c *gin.Context) {
		channel := strings.TrimSpace(strings.ToLower(c.Query("channel")))
		if channel != "" && channel != "codex" && channel != "claude" && channel != "mixed" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "channel must be codex, claude, or mixed"})
			return
		}
		limit := 20
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
				return
			}
			limit = parsed
		}
		items := coreauth.RecentRouteDecisionSnapshots(channel, limit)
		response := ChannelRoutingDecisionListResponse{Items: make([]ChannelRoutingDecisionSnapshot, 0, len(items))}
		for _, item := range items {
			response.Items = append(response.Items, mapChannelRoutingDecisionSnapshot(item))
		}
		c.JSON(http.StatusOK, response)
	})
}

func mapChannelRoutingDecisionSnapshot(item coreauth.RouteDecisionSnapshot) ChannelRoutingDecisionSnapshot {
	out := ChannelRoutingDecisionSnapshot{
		ID:                   item.ID,
		RecordedAt:           item.RecordedAt.UTC().Format(time.RFC3339Nano),
		Channel:              item.Provider,
		Providers:            append([]string(nil), item.Providers...),
		Model:                item.Model,
		ProjectKey:           item.ProjectKey,
		ProjectName:          item.ProjectName,
		ProjectKeySource:     item.ProjectKeySource,
		ProjectKeyConfidence: item.ProjectKeyConfidence,
		ProjectMatchKeys:     append([]string(nil), item.ProjectMatchKeys...),
		Source:               item.Source,
		CandidateCount:       item.CandidateCount,
		Candidates:           make([]ChannelRoutingDecisionCandidate, 0, len(item.Candidates)),
		SelectedAuthID:       item.SelectedAuthID,
		SelectedAccountID:    item.SelectedAccountKey,
		SelectedProvider:     item.SelectedProvider,
		UnavailableCode:      item.UnavailableCode,
		UnavailableMessage:   item.UnavailableMessage,
		Trace:                make([]ChannelRoutingDecisionStep, 0, len(item.Trace)),
		DroppedReasons:       make([]ChannelRoutingDroppedReason, 0, len(item.DroppedReasons)),
	}
	for _, candidate := range item.Candidates {
		out.Candidates = append(out.Candidates, ChannelRoutingDecisionCandidate{
			AuthID:    candidate.AuthID,
			AccountID: candidate.AccountKey,
			Provider:  candidate.Provider,
		})
	}
	for _, step := range item.Trace {
		cloned := ChannelRoutingDecisionStep{
			Stage:     step.Stage,
			Policy:    step.Policy,
			Reason:    step.Reason,
			Before:    step.Before,
			After:     step.After,
			AllowIDs:  append([]string(nil), step.AllowIDs...),
			DenyIDs:   append([]string(nil), step.DenyIDs...),
			OrderIDs:  append([]string(nil), step.OrderIDs...),
			Activated: step.Activated,
		}
		if step.Fallback != nil {
			value := *step.Fallback
			cloned.Fallback = &value
		}
		out.Trace = append(out.Trace, cloned)
	}
	for _, reason := range item.DroppedReasons {
		out.DroppedReasons = append(out.DroppedReasons, mapChannelRoutingDecisionDroppedReason(reason))
	}
	return out
}

func mapChannelRoutingDecisionDroppedReason(item coreauth.RouteDecisionDroppedReasonSnapshot) ChannelRoutingDroppedReason {
	out := ChannelRoutingDroppedReason{
		AccountID:     strings.TrimSpace(item.AccountKey),
		AuthID:        strings.TrimSpace(item.AuthID),
		Source:        strings.TrimSpace(item.Source),
		Scope:         normalizeRouteResilienceScope(RouteResilienceScope(item.Scope), item.Source),
		Reason:        strings.TrimSpace(item.Reason),
		Model:         strings.TrimSpace(item.Model),
		RouteBlocking: item.RouteBlocking,
	}
	if !item.ExpiresAt.IsZero() {
		out.ExpiresAt = item.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if !item.UpdatedAt.IsZero() {
		out.UpdatedAt = item.UpdatedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}
