package gettokenshooks

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokensrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	AccountRouteGuardSourceManualDisabled       = "manual-disabled"
	AccountRouteGuardSourceRateLimit            = "rate-limit"
	AccountRouteGuardSourceAuthError            = "auth-error"
	AccountRouteGuardSourceUpstreamRateLimit    = "upstream-rate-limit"
	AccountRouteGuardSourceUpstreamTransientErr = "upstream-error"
)

var accountRouteGuardResultTransientSources = []string{
	AccountRouteGuardSourceAuthError,
	AccountRouteGuardSourceUpstreamRateLimit,
	AccountRouteGuardSourceUpstreamTransientErr,
}

type AccountRouteGuardBlock struct {
	Source     string
	AuthID     string
	AccountKey string
	LookupKeys []string
	Reason     string
	ExpiresAt  time.Time
	UpdatedAt  time.Time
}

type AccountRouteGuardStore struct {
	mu     sync.RWMutex
	blocks map[string]map[string]AccountRouteGuardBlock
	lookup map[string]map[string]AccountRouteGuardBlock
}

var defaultAccountRouteGuardStore = NewAccountRouteGuardStore()

func NewAccountRouteGuardStore() *AccountRouteGuardStore {
	return &AccountRouteGuardStore{
		blocks: map[string]map[string]AccountRouteGuardBlock{},
		lookup: map[string]map[string]AccountRouteGuardBlock{},
	}
}

func DefaultAccountRouteGuardStore() *AccountRouteGuardStore {
	return defaultAccountRouteGuardStore
}

func MarkAccountRouteGuardBlocked(block AccountRouteGuardBlock) {
	defaultAccountRouteGuardStore.MarkBlocked(block)
}

func ReplaceAccountRouteGuardSource(source string, blocks []AccountRouteGuardBlock) {
	defaultAccountRouteGuardStore.ReplaceSource(source, blocks)
}

func ClearAccountRouteGuardSource(source string) {
	defaultAccountRouteGuardStore.ClearSource(source)
}

func ClearAccountRouteGuardAuth(source string, authID string) {
	defaultAccountRouteGuardStore.ClearAuth(source, authID)
}

func MarkManualDisabledAuth(auth *coreauth.Auth, reason string) {
	if auth == nil {
		return
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = "account disabled"
	}
	defaultAccountRouteGuardStore.MarkBlocked(AccountRouteGuardBlock{
		Source:     AccountRouteGuardSourceManualDisabled,
		AuthID:     strings.TrimSpace(auth.ID),
		AccountKey: "auth-id:" + strings.TrimSpace(auth.ID),
		LookupKeys: accountRouteGuardIdentityKeysForAuth(auth),
		Reason:     reason,
	})
}

func ClearManualDisabledAuth(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	defaultAccountRouteGuardStore.ClearAuth(AccountRouteGuardSourceManualDisabled, auth.ID)
}

type AccountRouteGuardResultHook struct {
	coreauth.NoopHook
	Store *AccountRouteGuardStore
}

func (h AccountRouteGuardResultHook) OnResult(_ context.Context, result coreauth.Result) {
	store := h.Store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	store.MarkResult(result)
}

func (s *AccountRouteGuardStore) MarkResult(result coreauth.Result) {
	if s == nil || strings.TrimSpace(result.AuthID) == "" {
		return
	}
	if result.Success {
		for _, source := range accountRouteGuardResultTransientSources {
			s.ClearAuth(source, result.AuthID)
		}
		return
	}
	block, ok := accountRouteGuardBlockForResult(result)
	if !ok {
		return
	}
	s.MarkBlocked(block)
}

func (s *AccountRouteGuardStore) MarkBlocked(block AccountRouteGuardBlock) {
	if s == nil {
		return
	}
	block = normalizeAccountRouteGuardBlock(block)
	if block.Source == "" || accountRouteGuardBlockKey(block) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(block.Source, accountRouteGuardBlockKey(block))
	if s.blocks[block.Source] == nil {
		s.blocks[block.Source] = map[string]AccountRouteGuardBlock{}
	}
	s.blocks[block.Source][accountRouteGuardBlockKey(block)] = block
	s.indexLocked(block)
}

func (s *AccountRouteGuardStore) ReplaceSource(source string, blocks []AccountRouteGuardBlock) {
	if s == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearSourceLocked(source)
	for _, block := range blocks {
		block.Source = source
		block = normalizeAccountRouteGuardBlock(block)
		if accountRouteGuardBlockKey(block) == "" {
			continue
		}
		if s.blocks[source] == nil {
			s.blocks[source] = map[string]AccountRouteGuardBlock{}
		}
		s.blocks[source][accountRouteGuardBlockKey(block)] = block
		s.indexLocked(block)
	}
}

func (s *AccountRouteGuardStore) ClearSource(source string) {
	if s == nil {
		return
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearSourceLocked(source)
}

func (s *AccountRouteGuardStore) ClearAuth(source string, authID string) {
	if s == nil {
		return
	}
	source = strings.TrimSpace(source)
	authID = strings.TrimSpace(authID)
	if source == "" || authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeLocked(source, authID)
}

func (s *AccountRouteGuardStore) DenyIDsForCandidates(candidates []*coreauth.Auth) []string {
	if s == nil || len(candidates) == 0 {
		return nil
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		for _, key := range accountRouteGuardKeysForAuth(candidate) {
			if s.lookupHasActiveBlockLocked(key, now) {
				id := strings.TrimSpace(candidate.ID)
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					out = append(out, id)
				}
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *AccountRouteGuardStore) IsAuthBlocked(auth *coreauth.Auth) bool {
	if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range accountRouteGuardKeysForAuth(auth) {
		if s.lookupHasActiveBlockLocked(key, now) {
			return true
		}
	}
	return false
}

func AccountRouteGuardBlocksAuth(auth *coreauth.Auth) bool {
	return defaultAccountRouteGuardStore.IsAuthBlocked(auth)
}

func (s *AccountRouteGuardStore) lookupHasActiveBlockLocked(key string, now time.Time) bool {
	blocks := s.lookup[strings.TrimSpace(key)]
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if block.ExpiresAt.IsZero() || block.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func (s *AccountRouteGuardStore) clearSourceLocked(source string) {
	for key := range s.blocks[source] {
		s.removeLocked(source, key)
	}
	delete(s.blocks, source)
}

func (s *AccountRouteGuardStore) removeLocked(source string, key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	block, ok := s.blocks[source][key]
	if !ok {
		return
	}
	delete(s.blocks[source], key)
	if len(s.blocks[source]) == 0 {
		delete(s.blocks, source)
	}
	for _, lookupKey := range block.LookupKeys {
		if entries := s.lookup[lookupKey]; len(entries) > 0 {
			delete(entries, source+"\x00"+key)
			if len(entries) == 0 {
				delete(s.lookup, lookupKey)
			}
		}
	}
}

func (s *AccountRouteGuardStore) indexLocked(block AccountRouteGuardBlock) {
	key := accountRouteGuardBlockKey(block)
	for _, lookupKey := range block.LookupKeys {
		if s.lookup[lookupKey] == nil {
			s.lookup[lookupKey] = map[string]AccountRouteGuardBlock{}
		}
		s.lookup[lookupKey][block.Source+"\x00"+key] = block
	}
}

type accountRouteGuardPolicy struct {
	store *AccountRouteGuardStore
}

func (p accountRouteGuardPolicy) RoutePolicyStage() gettokensrouting.PolicyStage {
	return gettokensrouting.PolicyStageHardFilter
}

func (p accountRouteGuardPolicy) RewriteCandidates(ctx context.Context, req coreauth.RoutePolicyRequest) coreauth.RoutePolicyDecision {
	store := p.store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	deny := store.DenyIDsForCandidates(req.Candidates)
	if len(deny) == 0 {
		return coreauth.RoutePolicyDecision{}
	}
	return coreauth.RoutePolicyDecision{DenyIDs: deny, Reason: "gettokens account route guard"}
}

func normalizeAccountRouteGuardBlock(block AccountRouteGuardBlock) AccountRouteGuardBlock {
	block.Source = strings.TrimSpace(block.Source)
	block.AuthID = strings.TrimSpace(block.AuthID)
	block.AccountKey = strings.TrimSpace(block.AccountKey)
	block.Reason = strings.TrimSpace(block.Reason)
	if block.UpdatedAt.IsZero() {
		block.UpdatedAt = time.Now().UTC()
	}
	keys := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	add(block.AuthID)
	if block.AuthID != "" {
		add("auth-id:" + block.AuthID)
	}
	add(block.AccountKey)
	for _, key := range block.LookupKeys {
		add(key)
	}
	block.LookupKeys = keys
	return block
}

func accountRouteGuardBlockForResult(result coreauth.Result) (AccountRouteGuardBlock, bool) {
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" || result.Error == nil {
		return AccountRouteGuardBlock{}, false
	}
	status := result.Error.StatusCode()
	source := ""
	reason := strings.TrimSpace(result.Error.Code)
	if reason == "" {
		reason = strings.TrimSpace(result.Error.Message)
	}
	cooldown := time.Duration(0)
	switch {
	case status == http.StatusUnauthorized:
		source = AccountRouteGuardSourceAuthError
		reason = defaultAccountRouteGuardReason(reason, "auth error")
	case status == http.StatusTooManyRequests:
		source = AccountRouteGuardSourceUpstreamRateLimit
		reason = defaultAccountRouteGuardReason(reason, "upstream rate limit")
		cooldown = time.Minute
	case status == http.StatusRequestTimeout || (status >= http.StatusInternalServerError && status <= 599):
		source = AccountRouteGuardSourceUpstreamTransientErr
		reason = defaultAccountRouteGuardReason(reason, "upstream transient error")
		cooldown = 30 * time.Second
	case strings.Contains(strings.ToLower(strings.TrimSpace(result.Error.Code)), "timeout") || strings.Contains(strings.ToLower(strings.TrimSpace(result.Error.Message)), "timeout"):
		source = AccountRouteGuardSourceUpstreamTransientErr
		reason = defaultAccountRouteGuardReason(reason, "upstream timeout")
		cooldown = 30 * time.Second
	default:
		return AccountRouteGuardBlock{}, false
	}
	if result.RetryAfter != nil && *result.RetryAfter > 0 {
		cooldown = *result.RetryAfter
	}
	block := AccountRouteGuardBlock{
		Source:     source,
		AuthID:     authID,
		AccountKey: "auth-id:" + authID,
		Reason:     reason,
	}
	if cooldown > 0 {
		block.ExpiresAt = time.Now().UTC().Add(cooldown)
	}
	return block, true
}

func defaultAccountRouteGuardReason(reason string, fallback string) string {
	reason = strings.TrimSpace(reason)
	if reason != "" {
		return reason
	}
	return fallback
}

func accountRouteGuardBlockKey(block AccountRouteGuardBlock) string {
	if block.AuthID != "" {
		return block.AuthID
	}
	return block.AccountKey
}

func accountRouteGuardKeysForAuth(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	keys := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	add(auth.ID)
	add(auth.AccountKey)
	add("auth-id:" + auth.ID)
	add(auth.Index)
	add("auth-index:" + auth.Index)
	add("provider:" + strings.ToLower(strings.TrimSpace(auth.Provider)))
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		add("auth-file:" + filepath.Base(fileName))
	}
	return keys
}

func accountRouteGuardIdentityKeysForAuth(auth *coreauth.Auth) []string {
	if auth == nil {
		return nil
	}
	keys := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	add(auth.ID)
	add("auth-id:" + auth.ID)
	add(auth.Index)
	add("auth-index:" + auth.Index)
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		add("auth-file:" + filepath.Base(fileName))
	}
	return keys
}
