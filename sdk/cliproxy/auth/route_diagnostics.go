package auth

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokenscodex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
)

const routeDecisionHistoryLimit = 200

type RouteDecisionCandidateSnapshot struct {
	AuthID     string
	AccountKey string
	Provider   string
}

type RouteDecisionSnapshot struct {
	ID                   string
	RecordedAt           time.Time
	Provider             string
	Providers            []string
	Model                string
	ProjectKey           string
	ProjectName          string
	ProjectKeySource     string
	ProjectKeyConfidence string
	ProjectMatchKeys     []string
	Source               string
	CandidateCount       int
	Candidates           []RouteDecisionCandidateSnapshot
	SelectedAuthID       string
	SelectedAccountKey   string
	SelectedProvider     string
	UnavailableCode      string
	UnavailableMessage   string
	Trace                []gettokensrouting.DecisionStep
}

var routeDecisionHistory = struct {
	sync.Mutex
	nextID int
	items  []RouteDecisionSnapshot
}{}

func recordRouteDecision(req routeRequest, source string, result gettokensrouting.RouteResult, selected *Auth, err error) {
	routeDecisionHistory.Lock()
	defer routeDecisionHistory.Unlock()

	routeDecisionHistory.nextID++
	snapshot := RouteDecisionSnapshot{
		ID:                 "route-decision-" + intString(routeDecisionHistory.nextID),
		RecordedAt:         time.Now().UTC(),
		Provider:           strings.TrimSpace(strings.ToLower(req.Provider)),
		Providers:          append([]string(nil), req.Providers...),
		Model:              strings.TrimSpace(req.Model),
		Source:             strings.TrimSpace(source),
		CandidateCount:     len(result.Candidates),
		Candidates:         routeDecisionCandidatesFromRouteResult(result),
		Trace:              cloneRouteDecisionTrace(result.Trace),
		SelectedAuthID:     routeDecisionSelectedAuthID(selected),
		SelectedAccountKey: routeDecisionSelectedAccountKey(selected),
		SelectedProvider:   routeDecisionSelectedProvider(selected),
		UnavailableCode:    routeDecisionErrorCode(err),
		UnavailableMessage: routeDecisionErrorMessage(err),
		ProjectMatchKeys:   []string{},
	}
	if codexCtx := gettokenscodex.RequestContextFromMetadata(req.Options.Metadata); codexCtx != nil {
		snapshot.ProjectKey = strings.TrimSpace(codexCtx.ProjectKey)
		snapshot.ProjectName = strings.TrimSpace(codexCtx.ProjectName)
		snapshot.ProjectKeySource = strings.TrimSpace(codexCtx.ProjectKeySource)
		snapshot.ProjectKeyConfidence = strings.TrimSpace(codexCtx.ProjectKeyConfidence)
		snapshot.ProjectMatchKeys = append([]string(nil), codexCtx.ProjectMatchKeys...)
	}
	routeDecisionHistory.items = append(routeDecisionHistory.items, snapshot)
	if len(routeDecisionHistory.items) > routeDecisionHistoryLimit {
		routeDecisionHistory.items = append([]RouteDecisionSnapshot(nil), routeDecisionHistory.items[len(routeDecisionHistory.items)-routeDecisionHistoryLimit:]...)
	}
}

func RecentRouteDecisionSnapshots(provider string, limit int) []RouteDecisionSnapshot {
	routeDecisionHistory.Lock()
	defer routeDecisionHistory.Unlock()

	provider = strings.TrimSpace(strings.ToLower(provider))
	if limit <= 0 {
		limit = 20
	}
	out := make([]RouteDecisionSnapshot, 0, limit)
	for index := len(routeDecisionHistory.items) - 1; index >= 0 && len(out) < limit; index-- {
		item := routeDecisionHistory.items[index]
		if provider != "" && item.Provider != provider {
			continue
		}
		out = append(out, cloneRouteDecisionSnapshot(item))
	}
	return out
}

func resetRouteDecisionSnapshotsForTest() {
	routeDecisionHistory.Lock()
	defer routeDecisionHistory.Unlock()
	routeDecisionHistory.nextID = 0
	routeDecisionHistory.items = nil
}

func ResetRouteDecisionSnapshotsForTests() {
	resetRouteDecisionSnapshotsForTest()
}

func SeedRouteDecisionSnapshotForTests(snapshot RouteDecisionSnapshot) {
	routeDecisionHistory.Lock()
	defer routeDecisionHistory.Unlock()
	routeDecisionHistory.nextID++
	if strings.TrimSpace(snapshot.ID) == "" {
		snapshot.ID = "route-decision-" + intString(routeDecisionHistory.nextID)
	}
	if snapshot.RecordedAt.IsZero() {
		snapshot.RecordedAt = time.Now().UTC()
	}
	routeDecisionHistory.items = append(routeDecisionHistory.items, cloneRouteDecisionSnapshot(snapshot))
	if len(routeDecisionHistory.items) > routeDecisionHistoryLimit {
		routeDecisionHistory.items = append([]RouteDecisionSnapshot(nil), routeDecisionHistory.items[len(routeDecisionHistory.items)-routeDecisionHistoryLimit:]...)
	}
}

func cloneRouteDecisionSnapshot(item RouteDecisionSnapshot) RouteDecisionSnapshot {
	cloned := item
	cloned.Providers = append([]string(nil), item.Providers...)
	cloned.ProjectMatchKeys = append([]string(nil), item.ProjectMatchKeys...)
	cloned.Candidates = append([]RouteDecisionCandidateSnapshot(nil), item.Candidates...)
	cloned.Trace = cloneRouteDecisionTrace(item.Trace)
	return cloned
}

func cloneRouteDecisionTrace(trace []gettokensrouting.DecisionStep) []gettokensrouting.DecisionStep {
	if len(trace) == 0 {
		return []gettokensrouting.DecisionStep{}
	}
	out := make([]gettokensrouting.DecisionStep, 0, len(trace))
	for _, step := range trace {
		cloned := step
		cloned.AllowIDs = append([]string(nil), step.AllowIDs...)
		cloned.DenyIDs = append([]string(nil), step.DenyIDs...)
		cloned.OrderIDs = append([]string(nil), step.OrderIDs...)
		if step.Fallback != nil {
			value := *step.Fallback
			cloned.Fallback = &value
		}
		out = append(out, cloned)
	}
	return out
}

func routeDecisionCandidatesFromRouteResult(result gettokensrouting.RouteResult) []RouteDecisionCandidateSnapshot {
	if len(result.Candidates) == 0 {
		return []RouteDecisionCandidateSnapshot{}
	}
	out := make([]RouteDecisionCandidateSnapshot, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		item := RouteDecisionCandidateSnapshot{AuthID: strings.TrimSpace(candidate.ID)}
		if auth, ok := candidate.Value.(*Auth); ok && auth != nil {
			item.AccountKey = strings.TrimSpace(auth.AccountKey)
			item.Provider = strings.TrimSpace(strings.ToLower(auth.Provider))
			if item.AuthID == "" {
				item.AuthID = strings.TrimSpace(auth.ID)
			}
		}
		out = append(out, item)
	}
	return out
}

func routeDecisionSelectedAuthID(selected *Auth) string {
	if selected == nil {
		return ""
	}
	return strings.TrimSpace(selected.ID)
}

func routeDecisionSelectedAccountKey(selected *Auth) string {
	if selected == nil {
		return ""
	}
	return strings.TrimSpace(selected.AccountKey)
}

func routeDecisionSelectedProvider(selected *Auth) string {
	if selected == nil {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(selected.Provider))
}

func routeDecisionErrorCode(err error) string {
	var authErr *Error
	if err == nil || !errors.As(err, &authErr) || authErr == nil {
		return ""
	}
	return strings.TrimSpace(authErr.Code)
}

func routeDecisionErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimSpace(err.Error())
}

func intString(value int) string {
	return strconv.Itoa(value)
}
