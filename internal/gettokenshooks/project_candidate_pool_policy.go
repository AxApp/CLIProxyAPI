package gettokenshooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type projectCandidatePoolPolicyStore struct {
	Version int                        `json:"version"`
	Rules   []ProjectCandidatePoolRule `json:"rules"`
}

type ProjectCandidatePoolRule struct {
	ID                   string   `json:"id"`
	Channel              string   `json:"channel"`
	ProjectKey           string   `json:"projectKey"`
	ProjectName          string   `json:"projectName,omitempty"`
	ProjectKeySource     string   `json:"projectKeySource,omitempty"`
	ProjectKeyConfidence string   `json:"projectKeyConfidence,omitempty"`
	Enabled              bool     `json:"enabled"`
	AllowAccountIDs      []string `json:"allowAccountIDs"`
	CreatedAt            string   `json:"createdAt,omitempty"`
	UpdatedAt            string   `json:"updatedAt,omitempty"`
}

var projectCandidatePoolPolicyConfigPathState struct {
	sync.RWMutex
	path string
}

var projectCandidatePoolPolicySnapshotState struct {
	sync.RWMutex
	path   string
	loaded bool
	store  projectCandidatePoolPolicyStore
}

func projectCandidatePoolPolicy() gettokensrouting.Policy {
	return gettokensrouting.Policy{
		Stage:   gettokensrouting.PolicyStagePoolScope,
		Name:    "project-candidate-pool",
		Rewrite: rewriteProjectCandidatePoolCandidates,
	}
}

func rewriteProjectCandidatePoolCandidates(_ context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
	channel := channelRoutingChannelForRequest(req)
	if channel == "" || len(req.Candidates) == 0 {
		return gettokensrouting.PolicyDecision{}
	}
	identity := projectIdentityFromRouteContext(req)
	if strings.EqualFold(identity.confidence, "ambiguous") {
		return gettokensrouting.PolicyDecision{Reason: "project-candidate-pool:not-evaluated:ambiguous-project"}
	}
	matchKeys := normalizeProjectCandidatePoolIDs(identity.matchKeys)
	if len(matchKeys) == 0 {
		return gettokensrouting.PolicyDecision{Reason: "project-candidate-pool:not-evaluated:no-project-key"}
	}
	store, ok := loadProjectCandidatePoolPolicyStore()
	if !ok {
		return gettokensrouting.PolicyDecision{Reason: "project-candidate-pool:not-matched"}
	}
	matches := matchingProjectCandidatePoolRules(store.Rules, channel, matchKeys)
	switch len(matches) {
	case 0:
		return gettokensrouting.PolicyDecision{Reason: "project-candidate-pool:not-matched"}
	case 1:
		allowIDs := normalizeProjectCandidatePoolIDs(matches[0].AllowAccountIDs)
		allowFallback := false
		if len(routeCandidateIntersection(req.Candidates, allowIDs)) == 0 {
			return gettokensrouting.PolicyDecision{
				DenyIDs:       routeCandidateIDs(req.Candidates),
				AllowFallback: &allowFallback,
				Reason:        "project-candidate-pool:no-routeable-account",
			}
		}
		return gettokensrouting.PolicyDecision{
			AllowIDs:      allowIDs,
			OrderIDs:      allowIDs,
			AllowFallback: &allowFallback,
			Reason:        "project-candidate-pool:matched",
		}
	default:
		allowFallback := false
		return gettokensrouting.PolicyDecision{
			DenyIDs:       routeCandidateIDs(req.Candidates),
			AllowFallback: &allowFallback,
			Reason:        "project-candidate-pool:conflict",
		}
	}
}

type projectRouteIdentity struct {
	key        string
	name       string
	source     string
	confidence string
	matchKeys  []string
}

func projectIdentityFromRouteContext(req gettokensrouting.RouteContext) projectRouteIdentity {
	identity := projectRouteIdentity{
		key:        strings.TrimSpace(req.ProjectKey),
		name:       strings.TrimSpace(req.ProjectName),
		source:     strings.TrimSpace(req.ProjectKeySource),
		confidence: strings.TrimSpace(req.ProjectKeyConfidence),
		matchKeys:  append([]string(nil), req.ProjectMatchKeys...),
	}
	if req.CodexRequest != nil {
		if identity.key == "" {
			identity.key = strings.TrimSpace(req.CodexRequest.ProjectKey)
		}
		if identity.name == "" {
			identity.name = strings.TrimSpace(req.CodexRequest.ProjectName)
		}
		if identity.source == "" {
			identity.source = strings.TrimSpace(req.CodexRequest.ProjectKeySource)
		}
		if identity.confidence == "" {
			identity.confidence = strings.TrimSpace(req.CodexRequest.ProjectKeyConfidence)
		}
		if len(identity.matchKeys) == 0 {
			identity.matchKeys = append([]string(nil), req.CodexRequest.ProjectMatchKeys...)
		}
	}
	if len(identity.matchKeys) == 0 && identity.key != "" {
		identity.matchKeys = []string{identity.key}
	}
	return identity
}

func matchingProjectCandidatePoolRules(rules []ProjectCandidatePoolRule, channel string, matchKeys []string) []ProjectCandidatePoolRule {
	keySet := idSet(matchKeys)
	matches := make([]ProjectCandidatePoolRule, 0, 1)
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(rule.Channel), channel) {
			continue
		}
		projectKey := strings.TrimSpace(rule.ProjectKey)
		if projectKey == "" {
			continue
		}
		if _, ok := keySet[projectKey]; !ok {
			continue
		}
		matches = append(matches, rule)
	}
	return matches
}

func loadProjectCandidatePoolPolicyStore() (projectCandidatePoolPolicyStore, bool) {
	path, err := projectCandidatePoolPolicyConfigPath()
	if err != nil {
		return projectCandidatePoolPolicyStore{}, false
	}
	if store, ok := projectCandidatePoolPolicySnapshot(path); ok {
		return store, true
	}
	store, err := readProjectCandidatePoolPolicyStoreAtPath(path)
	if err != nil {
		return projectCandidatePoolPolicyStore{}, false
	}
	setProjectCandidatePoolPolicySnapshot(path, store)
	return cloneProjectCandidatePoolPolicyStore(store), true
}

func readProjectCandidatePoolPolicyStore() (projectCandidatePoolPolicyStore, error) {
	path, err := projectCandidatePoolPolicyConfigPath()
	if err != nil {
		return projectCandidatePoolPolicyStore{}, err
	}
	return readProjectCandidatePoolPolicyStoreAtPath(path)
}

func readProjectCandidatePoolPolicyStoreAtPath(path string) (projectCandidatePoolPolicyStore, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectCandidatePoolPolicyStore{Version: 1, Rules: []ProjectCandidatePoolRule{}}, nil
		}
		return projectCandidatePoolPolicyStore{}, err
	}
	var store projectCandidatePoolPolicyStore
	if err := json.Unmarshal(body, &store); err != nil {
		return projectCandidatePoolPolicyStore{}, err
	}
	if store.Version == 0 {
		store.Version = 1
	}
	if store.Rules == nil {
		store.Rules = []ProjectCandidatePoolRule{}
	}
	return store, nil
}

func projectCandidatePoolPolicySnapshot(path string) (projectCandidatePoolPolicyStore, bool) {
	projectCandidatePoolPolicySnapshotState.RLock()
	defer projectCandidatePoolPolicySnapshotState.RUnlock()
	if !projectCandidatePoolPolicySnapshotState.loaded || projectCandidatePoolPolicySnapshotState.path != path {
		return projectCandidatePoolPolicyStore{}, false
	}
	return cloneProjectCandidatePoolPolicyStore(projectCandidatePoolPolicySnapshotState.store), true
}

func setProjectCandidatePoolPolicySnapshot(path string, store projectCandidatePoolPolicyStore) {
	projectCandidatePoolPolicySnapshotState.Lock()
	projectCandidatePoolPolicySnapshotState.path = path
	projectCandidatePoolPolicySnapshotState.loaded = true
	projectCandidatePoolPolicySnapshotState.store = cloneProjectCandidatePoolPolicyStore(store)
	projectCandidatePoolPolicySnapshotState.Unlock()
}

func resetProjectCandidatePoolPolicySnapshot() {
	projectCandidatePoolPolicySnapshotState.Lock()
	projectCandidatePoolPolicySnapshotState.path = ""
	projectCandidatePoolPolicySnapshotState.loaded = false
	projectCandidatePoolPolicySnapshotState.store = projectCandidatePoolPolicyStore{}
	projectCandidatePoolPolicySnapshotState.Unlock()
}

func refreshProjectCandidatePoolPolicySnapshot(store projectCandidatePoolPolicyStore) {
	path, err := projectCandidatePoolPolicyConfigPath()
	if err != nil {
		resetProjectCandidatePoolPolicySnapshot()
		return
	}
	setProjectCandidatePoolPolicySnapshot(path, store)
}

func cloneProjectCandidatePoolPolicyStore(store projectCandidatePoolPolicyStore) projectCandidatePoolPolicyStore {
	return projectCandidatePoolPolicyStore{
		Version: store.Version,
		Rules:   cloneProjectCandidatePoolRules(store.Rules),
	}
}

func cloneProjectCandidatePoolRules(rules []ProjectCandidatePoolRule) []ProjectCandidatePoolRule {
	if rules == nil {
		return nil
	}
	out := make([]ProjectCandidatePoolRule, 0, len(rules))
	for _, rule := range rules {
		rule.AllowAccountIDs = append([]string(nil), rule.AllowAccountIDs...)
		out = append(out, rule)
	}
	return out
}

func saveProjectCandidatePoolPolicyStore(store projectCandidatePoolPolicyStore) error {
	path, err := projectCandidatePoolPolicyConfigPath()
	if err != nil {
		return err
	}
	if store.Version == 0 {
		store.Version = 1
	}
	if store.Rules == nil {
		store.Rules = []ProjectCandidatePoolRule{}
	}
	sort.SliceStable(store.Rules, func(i, j int) bool {
		left := store.Rules[i]
		right := store.Rules[j]
		if left.Channel != right.Channel {
			return left.Channel < right.Channel
		}
		if left.ProjectKey != right.ProjectKey {
			return left.ProjectKey < right.ProjectKey
		}
		return left.ID < right.ID
	})
	body, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func SetProjectCandidatePoolPolicyConfigPathFromConfig(configPath string) {
	path, err := projectCandidatePoolPolicyConfigPathFromConfig(configPath)
	if err != nil {
		return
	}
	projectCandidatePoolPolicyConfigPathState.Lock()
	projectCandidatePoolPolicyConfigPathState.path = path
	projectCandidatePoolPolicyConfigPathState.Unlock()
	resetProjectCandidatePoolPolicySnapshot()
}

func projectCandidatePoolPolicyConfigPath() (string, error) {
	projectCandidatePoolPolicyConfigPathState.RLock()
	path := projectCandidatePoolPolicyConfigPathState.path
	projectCandidatePoolPolicyConfigPathState.RUnlock()
	if strings.TrimSpace(path) != "" {
		return path, nil
	}
	return projectCandidatePoolPolicyConfigPathFromConfig("")
}

func projectCandidatePoolPolicyConfigPathFromConfig(configPath string) (string, error) {
	if dir := strings.TrimSpace(configPath); dir != "" {
		if strings.EqualFold(filepath.Base(dir), "config.yaml") || strings.EqualFold(filepath.Base(dir), "config.yml") {
			dir = filepath.Dir(dir)
		}
		return filepath.Join(dir, "project-candidate-pool-rules", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gettokens-data", "project-candidate-pool-rules", "config.json"), nil
}

func routeCandidateIDs(candidates []gettokensrouting.RouteCandidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if id := strings.TrimSpace(candidate.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func routeCandidateIntersection(candidates []gettokensrouting.RouteCandidate, ids []string) []string {
	allowed := idSet(ids)
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		id := strings.TrimSpace(candidate.ID)
		if id == "" {
			continue
		}
		if _, ok := allowed[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func idSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

func normalizeProjectCandidatePoolIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func ConfigureProjectCandidatePoolRoutes(group *gin.RouterGroup, handler *handlers.BaseAPIHandler, _ *config.Config) {
	if group == nil {
		return
	}
	group.GET("/gettokens/project-candidate-pool-rules", func(c *gin.Context) {
		store, err := readProjectCandidatePoolPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": listProjectCandidatePoolRules(store, c.Query("channel"))})
	})
	group.POST("/gettokens/project-candidate-pool-rules", func(c *gin.Context) {
		var rule ProjectCandidatePoolRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store, err := readProjectCandidatePoolPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = randomID("project-candidate-pool-rule")
		}
		rule.CreatedAt = now
		rule.UpdatedAt = now
		normalized, err := normalizeProjectCandidatePoolRule(rule)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := validateProjectCandidatePoolRule(normalized, store.Rules); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store.Rules = upsertProjectCandidatePoolRule(store.Rules, normalized)
		if err := saveProjectCandidatePoolPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		refreshProjectCandidatePoolPolicySnapshot(store)
		bumpProjectCandidatePoolPolicyEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"items": listProjectCandidatePoolRules(store, normalized.Channel)})
	})
	group.PUT("/gettokens/project-candidate-pool-rules/:id", func(c *gin.Context) {
		var rule ProjectCandidatePoolRule
		if err := c.ShouldBindJSON(&rule); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store, err := readProjectCandidatePoolPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		rule.ID = strings.TrimSpace(c.Param("id"))
		if rule.ID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
			return
		}
		if existing, ok := findProjectCandidatePoolRule(store.Rules, rule.ID); ok {
			rule.CreatedAt = existing.CreatedAt
		}
		if strings.TrimSpace(rule.CreatedAt) == "" {
			rule.CreatedAt = time.Now().UTC().Format(time.RFC3339)
		}
		rule.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		normalized, err := normalizeProjectCandidatePoolRule(rule)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := validateProjectCandidatePoolRule(normalized, store.Rules); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		store.Rules = upsertProjectCandidatePoolRule(store.Rules, normalized)
		if err := saveProjectCandidatePoolPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		refreshProjectCandidatePoolPolicySnapshot(store)
		bumpProjectCandidatePoolPolicyEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"items": listProjectCandidatePoolRules(store, normalized.Channel)})
	})
	group.DELETE("/gettokens/project-candidate-pool-rules/:id", func(c *gin.Context) {
		store, err := readProjectCandidatePoolPolicyStore()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		store.Rules = deleteProjectCandidatePoolRule(store.Rules, c.Param("id"))
		if err := saveProjectCandidatePoolPolicyStore(store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		refreshProjectCandidatePoolPolicySnapshot(store)
		bumpProjectCandidatePoolPolicyEpoch(handler)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

func bumpProjectCandidatePoolPolicyEpoch(handler *handlers.BaseAPIHandler) {
	if handler == nil || handler.AuthManager == nil {
		return
	}
	handler.AuthManager.BumpSessionAffinityPoolEpoch()
}

func listProjectCandidatePoolRules(store projectCandidatePoolPolicyStore, channel string) []ProjectCandidatePoolRule {
	channel = strings.TrimSpace(strings.ToLower(channel))
	out := make([]ProjectCandidatePoolRule, 0, len(store.Rules))
	for _, rule := range store.Rules {
		if channel != "" && !strings.EqualFold(rule.Channel, channel) {
			continue
		}
		out = append(out, rule)
	}
	return out
}

func normalizeProjectCandidatePoolRule(rule ProjectCandidatePoolRule) (ProjectCandidatePoolRule, error) {
	rule.ID = strings.TrimSpace(rule.ID)
	if rule.ID == "" {
		return ProjectCandidatePoolRule{}, errors.New("id is required")
	}
	channel, err := normalizeProjectCandidatePoolChannel(rule.Channel)
	if err != nil {
		return ProjectCandidatePoolRule{}, err
	}
	rule.Channel = channel
	rule.ProjectKey = strings.TrimSpace(rule.ProjectKey)
	rule.ProjectName = strings.TrimSpace(rule.ProjectName)
	rule.ProjectKeySource = strings.TrimSpace(rule.ProjectKeySource)
	rule.ProjectKeyConfidence = strings.TrimSpace(rule.ProjectKeyConfidence)
	rule.AllowAccountIDs = normalizeProjectCandidatePoolIDs(rule.AllowAccountIDs)
	rule.CreatedAt = strings.TrimSpace(rule.CreatedAt)
	rule.UpdatedAt = strings.TrimSpace(rule.UpdatedAt)
	return rule, nil
}

func normalizeProjectCandidatePoolChannel(channel string) (string, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	switch channel {
	case "codex", "claude":
		return channel, nil
	default:
		if channel == "" {
			return "", errors.New("channel is required")
		}
		return "", fmt.Errorf("unsupported channel %q", channel)
	}
}

func validateProjectCandidatePoolRule(rule ProjectCandidatePoolRule, existing []ProjectCandidatePoolRule) error {
	if !rule.Enabled {
		if rule.ProjectKey != "" && !projectCandidatePoolKeyHasSourcePrefix(rule.ProjectKey) {
			return fmt.Errorf("projectKey must be source-prefixed, got %q", rule.ProjectKey)
		}
		return nil
	}
	if rule.ProjectKey == "" {
		return errors.New("projectKey is required for enabled project candidate pool rules")
	}
	if !projectCandidatePoolKeyHasSourcePrefix(rule.ProjectKey) {
		return fmt.Errorf("projectKey must be source-prefixed, got %q", rule.ProjectKey)
	}
	if len(rule.AllowAccountIDs) == 0 {
		return errors.New("allowAccountIDs must not be empty for enabled project candidate pool rules")
	}
	for _, candidate := range existing {
		if !candidate.Enabled || candidate.ID == rule.ID {
			continue
		}
		if strings.EqualFold(candidate.Channel, rule.Channel) && strings.TrimSpace(candidate.ProjectKey) == rule.ProjectKey {
			return fmt.Errorf("duplicate enabled project candidate pool rule for channel %q and projectKey %q", rule.Channel, rule.ProjectKey)
		}
	}
	return nil
}

func projectCandidatePoolKeyHasSourcePrefix(key string) bool {
	key = strings.TrimSpace(key)
	for _, prefix := range []string{"workspace:", "git:", "manual:"} {
		if strings.HasPrefix(key, prefix) && len(key) > len(prefix) {
			return true
		}
	}
	return false
}

func findProjectCandidatePoolRule(rules []ProjectCandidatePoolRule, id string) (ProjectCandidatePoolRule, bool) {
	id = strings.TrimSpace(id)
	for _, rule := range rules {
		if strings.TrimSpace(rule.ID) == id {
			return rule, true
		}
	}
	return ProjectCandidatePoolRule{}, false
}

func upsertProjectCandidatePoolRule(rules []ProjectCandidatePoolRule, rule ProjectCandidatePoolRule) []ProjectCandidatePoolRule {
	out := make([]ProjectCandidatePoolRule, 0, len(rules)+1)
	replaced := false
	for _, existing := range rules {
		if strings.TrimSpace(existing.ID) == rule.ID {
			out = append(out, rule)
			replaced = true
			continue
		}
		out = append(out, existing)
	}
	if !replaced {
		out = append(out, rule)
	}
	return out
}

func deleteProjectCandidatePoolRule(rules []ProjectCandidatePoolRule, id string) []ProjectCandidatePoolRule {
	id = strings.TrimSpace(id)
	out := make([]ProjectCandidatePoolRule, 0, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.ID) == id {
			continue
		}
		out = append(out, rule)
	}
	return out
}
