package gettokensrouting

import (
	"sort"
	"strings"
)

type RouteSignal struct {
	Key    string
	Value  string
	Weight int
	Reason string
}

type RouteConstraint struct {
	Name  string
	Allow func(RuntimeAccountProjection) (bool, string)
}

type RouteScoring struct {
	Name  string
	Score func(RuntimeAccountProjection) (int, string)
}

type RuntimeAccountEvaluation struct {
	Projection        RuntimeAccountProjection
	Allowed           bool
	ConstraintReasons []string
	Score             int
	ScoreReasons      []string
	Signals           []RouteSignal
}

func CoarseAvailabilityConstraint() RouteConstraint {
	return RouteConstraint{
		Name: "coarse-availability",
		Allow: func(account RuntimeAccountProjection) (bool, string) {
			if account.CoarseAvailable {
				return true, ""
			}
			if len(account.FilteredReasons) > 0 {
				return false, strings.Join(account.FilteredReasons, ",")
			}
			return false, "account-coarse-unavailable"
		},
	}
}

func ActiveSessionScoring() RouteScoring {
	return RouteScoring{
		Name: "active-sessions",
		Score: func(account RuntimeAccountProjection) (int, string) {
			return -account.ActiveSessions, "active-sessions"
		},
	}
}

func EvaluateRuntimeAccountProjection(account RuntimeAccountProjection, constraints []RouteConstraint, scorings []RouteScoring, signals []RouteSignal) RuntimeAccountEvaluation {
	evaluation := RuntimeAccountEvaluation{
		Projection: account,
		Allowed:    true,
		Signals:    append([]RouteSignal(nil), signals...),
	}
	for _, constraint := range constraints {
		if constraint.Allow == nil {
			continue
		}
		allowed, reason := constraint.Allow(account)
		if !allowed {
			evaluation.Allowed = false
			evaluation.ConstraintReasons = appendUniqueReason(evaluation.ConstraintReasons, firstNonEmptyRuntimeReason(reason, strings.TrimSpace(constraint.Name)))
		}
	}
	for _, scoring := range scorings {
		if scoring.Score == nil {
			continue
		}
		score, reason := scoring.Score(account)
		evaluation.Score += score
		evaluation.ScoreReasons = appendUniqueReason(evaluation.ScoreReasons, firstNonEmptyRuntimeReason(reason, strings.TrimSpace(scoring.Name)))
	}
	sort.Strings(evaluation.ConstraintReasons)
	return evaluation
}

func firstNonEmptyRuntimeReason(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
