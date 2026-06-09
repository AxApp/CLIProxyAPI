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
	AccountRouteGuardSourceQuotaEmpty           = "quota-empty"
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
		AccountKey: firstNonEmptyRouteGuardString(auth.AccountKey, "auth-id:"+strings.TrimSpace(auth.ID)),
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

func (h AccountRouteGuardResultHook) OnAuthRegistered(_ context.Context, auth *coreauth.Auth) {
	store := h.Store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	store.SyncQuotaEmptyAuth(auth, time.Now().UTC())
}

func (h AccountRouteGuardResultHook) OnAuthUpdated(_ context.Context, auth *coreauth.Auth) {
	store := h.Store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	store.SyncQuotaEmptyAuth(auth, time.Now().UTC())
}

func (h AccountRouteGuardResultHook) OnResult(_ context.Context, result coreauth.Result) {
	store := h.Store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	if result.Success {
		store.ClearAuth(AccountRouteGuardSourceQuotaEmpty, result.AuthID)
		store.MarkResult(result)
		return
	}
	if block, ok := quotaEmptyRouteGuardBlockForResult(result, time.Now().UTC()); ok {
		store.MarkBlocked(block)
		return
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
	s.removeLocked(block.Source, accountRouteGuardBlockKey(block))
	if s.blocks[block.Source] == nil {
		s.blocks[block.Source] = map[string]AccountRouteGuardBlock{}
	}
	s.blocks[block.Source][accountRouteGuardBlockKey(block)] = block
	s.indexLocked(block)
	s.mu.Unlock()
	persistAccountRouteGuardBlock(block)
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
	s.clearSourceLocked(source)
	persistedBlocks := make([]AccountRouteGuardBlock, 0, len(blocks))
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
		persistedBlocks = append(persistedBlocks, block)
	}
	s.mu.Unlock()
	replacePersistedAccountRouteGuardSource(source, persistedBlocks)
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
	s.clearSourceLocked(source)
	s.mu.Unlock()
	clearPersistedAccountRouteGuardSource(source)
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
	affected := s.blocksForAuthLocked(source, authID)
	s.removeLocked(source, authID)
	s.removeLookupKeyLocked(source, authID)
	s.mu.Unlock()
	clearPersistedAccountRouteGuardAuth(source, authID, affected)
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

func (s *AccountRouteGuardStore) ActiveBlocksForAuth(auth *coreauth.Auth) []AccountRouteGuardBlock {
	if s == nil || auth == nil {
		return nil
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []AccountRouteGuardBlock{}
	seen := map[string]struct{}{}
	for _, key := range accountRouteGuardKeysForAuth(auth) {
		blocks := s.lookup[strings.TrimSpace(key)]
		for blockKey, block := range blocks {
			if !block.ExpiresAt.IsZero() && !block.ExpiresAt.After(now) {
				continue
			}
			if _, exists := seen[blockKey]; exists {
				continue
			}
			seen[blockKey] = struct{}{}
			block.LookupKeys = append([]string(nil), block.LookupKeys...)
			out = append(out, block)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return accountRouteGuardBlockKey(out[i]) < accountRouteGuardBlockKey(out[j])
	})
	return out
}

func (s *AccountRouteGuardStore) ActiveBlocksForCandidates(candidates []*coreauth.Auth) map[string][]AccountRouteGuardBlock {
	if s == nil || len(candidates) == 0 {
		return nil
	}
	out := map[string][]AccountRouteGuardBlock{}
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		blocks := s.ActiveBlocksForAuth(candidate)
		if len(blocks) == 0 {
			continue
		}
		out[strings.TrimSpace(candidate.ID)] = blocks
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func AccountRouteGuardBlocksAuth(auth *coreauth.Auth) bool {
	return defaultAccountRouteGuardStore.IsAuthBlocked(auth)
}

func ActiveAccountRouteGuardBlocksForAuth(auth *coreauth.Auth) []AccountRouteGuardBlock {
	return defaultAccountRouteGuardStore.ActiveBlocksForAuth(auth)
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

func (s *AccountRouteGuardStore) removeLookupKeyLocked(source string, lookupKey string) {
	lookupKey = strings.TrimSpace(lookupKey)
	if lookupKey == "" {
		return
	}
	entries := s.lookup[lookupKey]
	if len(entries) == 0 {
		return
	}
	blockKeys := make([]string, 0, len(entries))
	for _, block := range entries {
		if strings.TrimSpace(block.Source) != source {
			continue
		}
		if key := accountRouteGuardBlockKey(block); key != "" {
			blockKeys = append(blockKeys, key)
		}
	}
	for _, key := range blockKeys {
		s.removeLocked(source, key)
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

func accountRouteGuardRoutingPolicy(store *AccountRouteGuardStore) gettokensrouting.Policy {
	return gettokensrouting.Policy{
		Stage: gettokensrouting.PolicyStageHardFilter,
		Name:  "account-route-guard",
		Rewrite: func(ctx context.Context, routeCtx gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
			return accountRouteGuardPolicy{store: store}.RewriteCandidates(ctx, routeCtx)
		},
	}
}

func (p accountRouteGuardPolicy) RewriteCandidates(ctx context.Context, req gettokensrouting.RouteContext) gettokensrouting.PolicyDecision {
	store := p.store
	if store == nil {
		store = defaultAccountRouteGuardStore
	}
	candidates := authCandidatesFromRouteContext(req)
	blocksByID := store.ActiveBlocksForCandidates(candidates)
	blocksByID = mergeAccountRouteGuardBlocks(blocksByID, activePersistedChannelRuntimeBlocksForCandidates(candidates))
	if len(blocksByID) == 0 {
		return gettokensrouting.PolicyDecision{}
	}
	deny := make([]string, 0, len(blocksByID))
	for id := range blocksByID {
		deny = append(deny, id)
	}
	sort.Strings(deny)
	if len(deny) == 0 {
		return gettokensrouting.PolicyDecision{}
	}
	return gettokensrouting.PolicyDecision{DenyIDs: deny, Reason: accountRouteGuardDecisionReason(blocksByID)}
}

func accountRouteGuardDecisionReason(blocksByID map[string][]AccountRouteGuardBlock) string {
	const base = "gettokens account route guard"
	if len(blocksByID) == 0 {
		return base
	}
	parts := []string{}
	seen := map[string]struct{}{}
	for _, blocks := range blocksByID {
		for _, block := range blocks {
			source := strings.TrimSpace(block.Source)
			if source == "" {
				continue
			}
			part := source
			if reason := strings.TrimSpace(block.Reason); reason != "" {
				part += "=" + reason
			}
			if _, exists := seen[part]; exists {
				continue
			}
			seen[part] = struct{}{}
			parts = append(parts, part)
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return base
	}
	return base + ": " + strings.Join(parts, "; ")
}

func mergeAccountRouteGuardBlocks(target map[string][]AccountRouteGuardBlock, extra map[string][]AccountRouteGuardBlock) map[string][]AccountRouteGuardBlock {
	if len(extra) == 0 {
		return target
	}
	if target == nil {
		target = map[string][]AccountRouteGuardBlock{}
	}
	for id, blocks := range extra {
		id = strings.TrimSpace(id)
		if id == "" || len(blocks) == 0 {
			continue
		}
		target[id] = append(target[id], blocks...)
	}
	return target
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

func (s *AccountRouteGuardStore) blocksForAuthLocked(source string, authID string) []AccountRouteGuardBlock {
	source = strings.TrimSpace(source)
	authID = strings.TrimSpace(authID)
	if source == "" || authID == "" {
		return nil
	}
	out := []AccountRouteGuardBlock{}
	seen := map[string]struct{}{}
	add := func(block AccountRouteGuardBlock) {
		key := accountRouteGuardBlockKey(block)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, block)
	}
	if block, ok := s.blocks[source][authID]; ok {
		add(block)
	}
	for _, lookupKey := range []string{authID, "auth-id:" + authID} {
		for _, block := range s.lookup[lookupKey] {
			if strings.TrimSpace(block.Source) == source {
				add(block)
			}
		}
	}
	return out
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
