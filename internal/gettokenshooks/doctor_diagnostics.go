package gettokenshooks

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

const (
	DoctorDiagnosticCheckRouteGuard = "route_guard_dropped_reasons"
	DoctorDiagnosticCheckQuotaFacts = "quota_facts"

	DoctorDiagnosticStatusOK       = "ok"
	DoctorDiagnosticStatusNotReady = "not_ready"
	DoctorDiagnosticStatusWarning  = "warning"
	DoctorDiagnosticStatusBlocking = "blocking"

	DoctorDiagnosticEvidenceRouteDroppedReason = "route_dropped_reason"
	DoctorDiagnosticEvidenceQuotaFact          = "quota_fact"
)

type DoctorDiagnosticsResponse struct {
	Authority   string                   `json:"authority"`
	Source      string                   `json:"source"`
	GeneratedAt string                   `json:"generatedAt"`
	Summary     DoctorDiagnosticsSummary `json:"summary"`
	Checks      []DoctorDiagnosticCheck  `json:"checks"`
}

type DoctorDiagnosticsSummary struct {
	Status   string `json:"status"`
	Total    int    `json:"total"`
	OK       int    `json:"ok"`
	NotReady int    `json:"notReady"`
	Warning  int    `json:"warning"`
	Blocking int    `json:"blocking"`
	Evidence int    `json:"evidence"`
}

type DoctorDiagnosticCheck struct {
	ID            string                     `json:"id"`
	Status        string                     `json:"status"`
	Reason        string                     `json:"reason"`
	Repairability string                     `json:"repairability"`
	Evidence      []DoctorDiagnosticEvidence `json:"evidence"`
}

type DoctorDiagnosticEvidence struct {
	Kind          string                       `json:"kind"`
	AccountKey    string                       `json:"accountKey,omitempty"`
	AuthID        string                       `json:"authId,omitempty"`
	Source        string                       `json:"source,omitempty"`
	Scope         string                       `json:"scope,omitempty"`
	Reason        string                       `json:"reason,omitempty"`
	Model         string                       `json:"model,omitempty"`
	ExpiresAt     string                       `json:"expiresAt,omitempty"`
	UpdatedAt     string                       `json:"updatedAt,omitempty"`
	RouteBlocking bool                         `json:"routeBlocking,omitempty"`
	State         string                       `json:"state,omitempty"`
	Freshness     string                       `json:"freshness,omitempty"`
	Confidence    string                       `json:"confidence,omitempty"`
	Risk          string                       `json:"risk,omitempty"`
	Explanation   string                       `json:"explanation,omitempty"`
	ObservedAt    string                       `json:"observedAt,omitempty"`
	EvidenceRefs  []string                     `json:"evidenceRefs,omitempty"`
	DroppedReason *ChannelRoutingDroppedReason `json:"droppedReason,omitempty"`
	QuotaFact     *QuotaRuntimeFact            `json:"quotaFact,omitempty"`
}

type DoctorDiagnosticsOptions struct {
	Handler    *handlers.BaseAPIHandler
	QuotaStore *QuotaRuntimeStore
	GuardStore *AccountRouteGuardStore
	Now        time.Time
}

type doctorDiagnosticTarget struct {
	AuthID     string
	AccountKey string
}

func ConfigureDoctorDiagnosticsRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, _ *config.Config) {
	configureDoctorDiagnosticsRoutes(group, DoctorDiagnosticsOptions{Handler: handler})
}

func configureDoctorDiagnosticsRoutes(group *gin.RouterGroup, options DoctorDiagnosticsOptions) {
	if group == nil {
		return
	}
	group.GET("/gettokens/doctor-diagnostics", func(c *gin.Context) {
		c.JSON(http.StatusOK, BuildDoctorDiagnosticsSnapshot(options))
	})
}

func BuildDoctorDiagnosticsSnapshot(options DoctorDiagnosticsOptions) DoctorDiagnosticsResponse {
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	checks := []DoctorDiagnosticCheck{
		buildDoctorRouteGuardCheck(options),
		buildDoctorQuotaFactCheck(options),
	}
	return DoctorDiagnosticsResponse{
		Authority:   "sidecar",
		Source:      "sidecar-diagnostics",
		GeneratedAt: now.Format(time.RFC3339),
		Summary:     buildDoctorDiagnosticsSummary(checks),
		Checks:      checks,
	}
}

func buildDoctorRouteGuardCheck(options DoctorDiagnosticsOptions) DoctorDiagnosticCheck {
	store := doctorDiagnosticsGuardStore(options.GuardStore)
	targets := doctorDiagnosticsTargets(options)
	evidence := make([]DoctorDiagnosticEvidence, 0)
	seen := map[string]struct{}{}
	for _, target := range targets {
		auth := &coreauth.Auth{ID: target.AuthID, AccountKey: target.AccountKey}
		blocks := routeResilienceStatesFromBlocks(store.ActiveBlocksForAuth(auth))
		input := RouteResilienceActionRequest{AccountKey: target.AccountKey, AuthID: target.AuthID}
		for _, dropped := range routeResilienceActionEvidenceFromBlocks(input, blocks).DroppedReasons {
			item := doctorDiagnosticsEvidenceFromDroppedReason(dropped)
			key := strings.Join([]string{item.Kind, item.AccountKey, item.AuthID, item.Source, item.Reason, item.Model}, "\x00")
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			evidence = append(evidence, item)
		}
	}
	sort.SliceStable(evidence, func(i, j int) bool {
		if evidence[i].AccountKey != evidence[j].AccountKey {
			return evidence[i].AccountKey < evidence[j].AccountKey
		}
		if evidence[i].Source != evidence[j].Source {
			return evidence[i].Source < evidence[j].Source
		}
		return evidence[i].Reason < evidence[j].Reason
	})
	if len(evidence) == 0 {
		return DoctorDiagnosticCheck{
			ID:            DoctorDiagnosticCheckRouteGuard,
			Status:        DoctorDiagnosticStatusNotReady,
			Reason:        "No active route guard dropped reason evidence has been observed.",
			Repairability: "read_only",
			Evidence:      []DoctorDiagnosticEvidence{},
		}
	}
	return DoctorDiagnosticCheck{
		ID:            DoctorDiagnosticCheckRouteGuard,
		Status:        DoctorDiagnosticStatusWarning,
		Reason:        "Active route guard dropped reason evidence is present.",
		Repairability: "read_only",
		Evidence:      evidence,
	}
}

func buildDoctorQuotaFactCheck(options DoctorDiagnosticsOptions) DoctorDiagnosticCheck {
	store := doctorDiagnosticsQuotaStore(options.QuotaStore)
	states := store.States()
	evidence := make([]DoctorDiagnosticEvidence, 0, len(states))
	status := DoctorDiagnosticStatusOK
	for _, state := range states {
		if state.Fact == nil {
			continue
		}
		item := doctorDiagnosticsEvidenceFromQuotaState(state)
		evidence = append(evidence, item)
		status = worseDoctorDiagnosticStatus(status, doctorDiagnosticStatusForQuotaFact(state.Fact))
	}
	if len(evidence) == 0 {
		return DoctorDiagnosticCheck{
			ID:            DoctorDiagnosticCheckQuotaFacts,
			Status:        DoctorDiagnosticStatusNotReady,
			Reason:        "No quota runtime fact evidence has been observed.",
			Repairability: "read_only",
			Evidence:      []DoctorDiagnosticEvidence{},
		}
	}
	return DoctorDiagnosticCheck{
		ID:            DoctorDiagnosticCheckQuotaFacts,
		Status:        status,
		Reason:        "Quota runtime facts are available from sidecar runtime state.",
		Repairability: "read_only",
		Evidence:      evidence,
	}
}

func buildDoctorDiagnosticsSummary(checks []DoctorDiagnosticCheck) DoctorDiagnosticsSummary {
	summary := DoctorDiagnosticsSummary{
		Status: DoctorDiagnosticStatusOK,
		Total:  len(checks),
	}
	for _, check := range checks {
		summary.Evidence += len(check.Evidence)
		summary.Status = worseDoctorDiagnosticStatus(summary.Status, check.Status)
		switch check.Status {
		case DoctorDiagnosticStatusBlocking:
			summary.Blocking++
		case DoctorDiagnosticStatusWarning:
			summary.Warning++
		case DoctorDiagnosticStatusNotReady:
			summary.NotReady++
		default:
			summary.OK++
		}
	}
	return summary
}

func doctorDiagnosticsTargets(options DoctorDiagnosticsOptions) []doctorDiagnosticTarget {
	targets := make([]doctorDiagnosticTarget, 0)
	seen := map[string]struct{}{}
	add := func(authID string, accountKey string) {
		authID = strings.TrimSpace(authID)
		accountKey = strings.TrimSpace(accountKey)
		if authID == "" && accountKey == "" {
			return
		}
		key := authID + "\x00" + accountKey
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		targets = append(targets, doctorDiagnosticTarget{AuthID: authID, AccountKey: accountKey})
	}
	if options.Handler != nil && options.Handler.AuthManager != nil {
		for _, auth := range options.Handler.AuthManager.List() {
			if auth == nil {
				continue
			}
			add(auth.ID, auth.AccountKey)
		}
	}
	for _, state := range doctorDiagnosticsQuotaStore(options.QuotaStore).States() {
		add("", state.AccountKey)
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].AccountKey != targets[j].AccountKey {
			return targets[i].AccountKey < targets[j].AccountKey
		}
		return targets[i].AuthID < targets[j].AuthID
	})
	return targets
}

func doctorDiagnosticsEvidenceFromDroppedReason(reason ChannelRoutingDroppedReason) DoctorDiagnosticEvidence {
	dropped := reason
	return DoctorDiagnosticEvidence{
		Kind:          DoctorDiagnosticEvidenceRouteDroppedReason,
		AccountKey:    strings.TrimSpace(reason.AccountID),
		AuthID:        strings.TrimSpace(reason.AuthID),
		Source:        strings.TrimSpace(reason.Source),
		Scope:         strings.TrimSpace(string(reason.Scope)),
		Reason:        strings.TrimSpace(reason.Reason),
		Model:         strings.TrimSpace(reason.Model),
		ExpiresAt:     strings.TrimSpace(reason.ExpiresAt),
		UpdatedAt:     strings.TrimSpace(reason.UpdatedAt),
		RouteBlocking: reason.RouteBlocking,
		DroppedReason: &dropped,
	}
}

func doctorDiagnosticsEvidenceFromQuotaState(state QuotaRuntimeState) DoctorDiagnosticEvidence {
	fact := *state.Fact
	fact.EvidenceRefs = append([]string(nil), state.Fact.EvidenceRefs...)
	return DoctorDiagnosticEvidence{
		Kind:         DoctorDiagnosticEvidenceQuotaFact,
		AccountKey:   strings.TrimSpace(state.AccountKey),
		Source:       strings.TrimSpace(fact.Source),
		State:        strings.TrimSpace(fact.State),
		Freshness:    strings.TrimSpace(fact.Freshness),
		Confidence:   strings.TrimSpace(fact.Confidence),
		Risk:         strings.TrimSpace(fact.Risk),
		Explanation:  strings.TrimSpace(fact.Explanation),
		ObservedAt:   strings.TrimSpace(fact.ObservedAt),
		ExpiresAt:    strings.TrimSpace(fact.ExpiresAt),
		EvidenceRefs: append([]string(nil), fact.EvidenceRefs...),
		QuotaFact:    &fact,
	}
}

func doctorDiagnosticStatusForQuotaFact(fact *QuotaRuntimeFact) string {
	if fact == nil {
		return DoctorDiagnosticStatusNotReady
	}
	switch strings.TrimSpace(fact.State) {
	case QuotaFactStateNoQuota:
		return DoctorDiagnosticStatusBlocking
	case QuotaFactStateStale, QuotaFactStateDenied, QuotaFactStateUnknown, QuotaFactStateUnsupported:
		return DoctorDiagnosticStatusWarning
	default:
		return DoctorDiagnosticStatusOK
	}
}

func worseDoctorDiagnosticStatus(current string, next string) string {
	if doctorDiagnosticStatusRank(next) > doctorDiagnosticStatusRank(current) {
		return next
	}
	return current
}

func doctorDiagnosticStatusRank(status string) int {
	switch status {
	case DoctorDiagnosticStatusBlocking:
		return 3
	case DoctorDiagnosticStatusWarning:
		return 2
	case DoctorDiagnosticStatusNotReady:
		return 1
	default:
		return 0
	}
}

func doctorDiagnosticsQuotaStore(store *QuotaRuntimeStore) *QuotaRuntimeStore {
	if store != nil {
		return store
	}
	return defaultQuotaRuntimeStore
}

func doctorDiagnosticsGuardStore(store *AccountRouteGuardStore) *AccountRouteGuardStore {
	if store != nil {
		return store
	}
	return defaultAccountRouteGuardStore
}
