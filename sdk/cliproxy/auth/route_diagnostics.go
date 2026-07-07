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

const (
	routeDecisionHistoryLimit        = 200
	routeDecisionSnapshotSampleLimit = 100
)

type RouteDecisionCandidateSnapshot struct {
	AuthID     string
	AccountKey string
	Provider   string
}

type RouteDecisionDroppedReasonSnapshot struct {
	AuthID        string
	AccountKey    string
	Source        string
	Scope         string
	Reason        string
	Model         string
	ExpiresAt     time.Time
	UpdatedAt     time.Time
	RouteBlocking bool
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
	DroppedReasons       []RouteDecisionDroppedReasonSnapshot
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
	recordedAt := time.Now().UTC()
	snapshot := RouteDecisionSnapshot{
		ID:                 "route-decision-" + intString(routeDecisionHistory.nextID),
		RecordedAt:         recordedAt,
		Provider:           strings.TrimSpace(strings.ToLower(req.Provider)),
		Providers:          append([]string(nil), req.Providers...),
		Model:              strings.TrimSpace(req.Model),
		Source:             strings.TrimSpace(source),
		CandidateCount:     routeResultCandidateCount(result),
		Candidates:         routeDecisionCandidatesFromRouteResult(result, selected),
		Trace:              cloneRouteDecisionTrace(result.Trace),
		SelectedAuthID:     routeDecisionSelectedAuthID(selected),
		SelectedAccountKey: routeDecisionSelectedAccountKey(selected),
		SelectedProvider:   routeDecisionSelectedProvider(selected),
		UnavailableCode:    routeDecisionErrorCode(err),
		UnavailableMessage: routeDecisionErrorMessage(err),
		ProjectMatchKeys:   []string{},
		DroppedReasons:     routeDecisionDroppedReasonsFromTrace(result.Trace, result.Candidates, req.Model, recordedAt),
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

func routeResultCandidateCount(result gettokensrouting.RouteResult) int {
	if result.CandidateCount > 0 {
		return result.CandidateCount
	}
	return len(result.Candidates)
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
	cloned.DroppedReasons = append([]RouteDecisionDroppedReasonSnapshot(nil), item.DroppedReasons...)
	return cloned
}

func cloneRouteDecisionTrace(trace []gettokensrouting.DecisionStep) []gettokensrouting.DecisionStep {
	if len(trace) == 0 {
		return []gettokensrouting.DecisionStep{}
	}
	out := make([]gettokensrouting.DecisionStep, 0, len(trace))
	for _, step := range trace {
		cloned := step
		cloned.AllowIDs = routeDecisionSampleIDs(step.AllowIDs)
		cloned.DenyIDs = routeDecisionSampleIDs(step.DenyIDs)
		cloned.OrderIDs = routeDecisionSampleIDs(step.OrderIDs)
		if step.Fallback != nil {
			value := *step.Fallback
			cloned.Fallback = &value
		}
		out = append(out, cloned)
	}
	return out
}

func routeDecisionCandidatesFromRouteResult(result gettokensrouting.RouteResult, selected *Auth) []RouteDecisionCandidateSnapshot {
	if len(result.Candidates) == 0 {
		return []RouteDecisionCandidateSnapshot{}
	}
	limit := routeDecisionSnapshotSampleLimit
	if len(result.Candidates) < limit {
		limit = len(result.Candidates)
	}
	selectedAuthID := routeDecisionSelectedAuthID(selected)
	out := make([]RouteDecisionCandidateSnapshot, 0, limit)
	selectedIncluded := selectedAuthID == ""
	var selectedCandidate *gettokensrouting.RouteCandidate
	for index := range result.Candidates {
		candidate := result.Candidates[index]
		if selectedAuthID != "" && strings.TrimSpace(candidate.ID) == selectedAuthID {
			selectedCandidate = &candidate
			if len(out) < limit {
				selectedIncluded = true
			}
		}
		if len(out) >= limit {
			continue
		}
		out = append(out, routeDecisionCandidateSnapshot(candidate))
	}
	if !selectedIncluded && selectedCandidate != nil {
		if len(out) >= routeDecisionSnapshotSampleLimit {
			out = out[:routeDecisionSnapshotSampleLimit-1]
		}
		out = append(out, routeDecisionCandidateSnapshot(*selectedCandidate))
	}
	if !selectedIncluded && selectedCandidate == nil && selected != nil {
		if len(out) >= routeDecisionSnapshotSampleLimit {
			out = out[:routeDecisionSnapshotSampleLimit-1]
		}
		out = append(out, routeDecisionCandidateSnapshotFromAuth(selected))
	}
	return out
}

func routeDecisionCandidateSnapshot(candidate gettokensrouting.RouteCandidate) RouteDecisionCandidateSnapshot {
	item := RouteDecisionCandidateSnapshot{AuthID: strings.TrimSpace(candidate.ID)}
	if auth, ok := candidate.Value.(*Auth); ok && auth != nil {
		item.AccountKey = strings.TrimSpace(auth.AccountKey)
		item.Provider = strings.TrimSpace(strings.ToLower(auth.Provider))
		if item.AuthID == "" {
			item.AuthID = strings.TrimSpace(auth.ID)
		}
	}
	return item
}

func routeDecisionCandidateSnapshotFromAuth(auth *Auth) RouteDecisionCandidateSnapshot {
	if auth == nil {
		return RouteDecisionCandidateSnapshot{}
	}
	return RouteDecisionCandidateSnapshot{
		AuthID:     strings.TrimSpace(auth.ID),
		AccountKey: strings.TrimSpace(auth.AccountKey),
		Provider:   strings.TrimSpace(strings.ToLower(auth.Provider)),
	}
}

func routeDecisionSampleIDs(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	limit := routeDecisionSnapshotSampleLimit
	if len(values) < limit {
		limit = len(values)
	}
	return append([]string(nil), values[:limit]...)
}

func routeDecisionDroppedReasonsFromTrace(trace []gettokensrouting.DecisionStep, candidates []gettokensrouting.RouteCandidate, model string, recordedAt time.Time) []RouteDecisionDroppedReasonSnapshot {
	if len(trace) == 0 {
		return []RouteDecisionDroppedReasonSnapshot{}
	}
	accountByAuthID := routeDecisionAccountKeysByAuthID(candidates)
	model = strings.TrimSpace(model)
	out := []RouteDecisionDroppedReasonSnapshot{}
	seen := map[string]struct{}{}
	for _, step := range trace {
		if len(step.DenyIDs) == 0 {
			continue
		}
		sources := routeDecisionDroppedSourcesFromStep(step)
		if len(sources) == 0 {
			continue
		}
		for _, authID := range step.DenyIDs {
			authID = strings.TrimSpace(authID)
			if authID == "" {
				continue
			}
			for _, source := range sources {
				if strings.TrimSpace(source.source) == "" {
					continue
				}
				item := RouteDecisionDroppedReasonSnapshot{
					AuthID:        authID,
					AccountKey:    accountByAuthID[authID],
					Source:        strings.TrimSpace(source.source),
					Scope:         routeDecisionFailureScopeForSource(source.source),
					Reason:        strings.TrimSpace(source.reason),
					Model:         model,
					UpdatedAt:     recordedAt,
					RouteBlocking: true,
				}
				key := strings.Join([]string{item.AuthID, item.AccountKey, item.Source, item.Scope, item.Reason, item.Model}, "\x00")
				if _, exists := seen[key]; exists {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, item)
				if len(out) >= routeDecisionSnapshotSampleLimit {
					return out
				}
			}
		}
	}
	if len(out) == 0 {
		return []RouteDecisionDroppedReasonSnapshot{}
	}
	return out
}

func routeDecisionAccountKeysByAuthID(candidates []gettokensrouting.RouteCandidate) map[string]string {
	out := map[string]string{}
	for _, candidate := range candidates {
		authID := strings.TrimSpace(candidate.ID)
		if authID == "" {
			continue
		}
		if auth, ok := candidate.Value.(*Auth); ok && auth != nil {
			out[authID] = strings.TrimSpace(auth.AccountKey)
			if auth.ID != "" {
				out[strings.TrimSpace(auth.ID)] = strings.TrimSpace(auth.AccountKey)
			}
		}
	}
	return out
}

type routeDecisionDroppedSource struct {
	source string
	reason string
}

func routeDecisionDroppedSourcesFromStep(step gettokensrouting.DecisionStep) []routeDecisionDroppedSource {
	policy := strings.TrimSpace(step.Policy)
	reason := strings.TrimSpace(step.Reason)
	if policy != "account-route-guard" && !strings.HasPrefix(reason, "gettokens account route guard") {
		return nil
	}
	payload := ""
	if index := strings.Index(reason, ":"); index >= 0 {
		payload = strings.TrimSpace(reason[index+1:])
	}
	if payload == "" {
		return nil
	}
	out := []routeDecisionDroppedSource{}
	for _, part := range strings.Split(payload, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		source, detail, found := strings.Cut(part, "=")
		source = strings.TrimSpace(source)
		if source == "" {
			continue
		}
		if !found {
			detail = ""
		}
		out = append(out, routeDecisionDroppedSource{source: source, reason: strings.TrimSpace(detail)})
	}
	return out
}

func routeDecisionFailureScopeForSource(source string) string {
	switch strings.TrimSpace(source) {
	case "model-lockout", "model-cooldown", "model-unavailable":
		return "model"
	case "provider-lockout", "provider-cooldown", "provider-unavailable":
		return "provider"
	default:
		return "account"
	}
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
