package gettokenshooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type persistedChannelAccountRuntimeState struct {
	AccountID string                                        `json:"accountID"`
	Sources   map[string]persistedChannelRuntimeStateSource `json:"sources,omitempty"`
	UpdatedAt string                                        `json:"updatedAt,omitempty"`
}

type persistedChannelRuntimeStateSource struct {
	Source    string `json:"source"`
	Scope     string `json:"scope,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

var channelRoutingRuntimeStateFileMu sync.Mutex

func activePersistedChannelRuntimeBlocksForCandidates(candidates []*coreauth.Auth) map[string][]AccountRouteGuardBlock {
	states, err := loadPersistedChannelRuntimeStates()
	if err != nil || len(states) == 0 || len(candidates) == 0 {
		return nil
	}
	now := time.Now().UTC()
	out := map[string][]AccountRouteGuardBlock{}
	for _, candidate := range candidates {
		if candidate == nil || strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		keys := accountRouteGuardKeysForAuth(candidate)
		blocks := make([]AccountRouteGuardBlock, 0)
		seen := map[string]struct{}{}
		for _, key := range keys {
			state, ok := states[strings.TrimSpace(key)]
			if !ok {
				continue
			}
			for _, block := range accountRouteGuardBlocksFromPersistedState(key, state, now) {
				blockKey := block.Source + "\x00" + accountRouteGuardBlockKey(block)
				if _, exists := seen[blockKey]; exists {
					continue
				}
				seen[blockKey] = struct{}{}
				blocks = append(blocks, block)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		sort.SliceStable(blocks, func(i, j int) bool {
			if blocks[i].Source != blocks[j].Source {
				return blocks[i].Source < blocks[j].Source
			}
			return accountRouteGuardBlockKey(blocks[i]) < accountRouteGuardBlockKey(blocks[j])
		})
		out[strings.TrimSpace(candidate.ID)] = blocks
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func accountRouteGuardBlocksFromPersistedState(key string, state persistedChannelAccountRuntimeState, now time.Time) []AccountRouteGuardBlock {
	accountID := strings.TrimSpace(state.AccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(key)
	}
	if accountID == "" || len(state.Sources) == 0 {
		return nil
	}
	out := make([]AccountRouteGuardBlock, 0, len(state.Sources))
	for sourceKey, source := range state.Sources {
		source.Source = firstNonEmptyRouteGuardString(source.Source, sourceKey)
		source.Source = strings.TrimSpace(source.Source)
		if source.Source == "" || !isPersistedChannelRuntimeSourceRouteBlocking(source.Source) {
			continue
		}
		expiresAt := parseRouteGuardTime(source.ExpiresAt)
		if !expiresAt.IsZero() && !expiresAt.After(now) {
			continue
		}
		updatedAt := parseRouteGuardTime(source.UpdatedAt)
		if updatedAt.IsZero() {
			updatedAt = parseRouteGuardTime(state.UpdatedAt)
		}
		if updatedAt.IsZero() {
			updatedAt = now
		}
		block := AccountRouteGuardBlock{
			Source:       source.Source,
			FailureScope: normalizeRouteResilienceScope(RouteResilienceScope(source.Scope), source.Source),
			Model:        strings.TrimSpace(source.Model),
			Reason:       strings.TrimSpace(source.Reason),
			ExpiresAt:    expiresAt,
			UpdatedAt:    updatedAt,
			LookupKeys:   []string{accountID},
		}
		if strings.HasPrefix(accountID, "auth-id:") {
			block.AuthID = strings.TrimPrefix(accountID, "auth-id:")
			block.AccountKey = accountID
		} else if strings.HasPrefix(accountID, "acct_") {
			block.AccountKey = accountID
		} else {
			block.AuthID = accountID
			block.AccountKey = "auth-id:" + accountID
		}
		out = append(out, normalizeAccountRouteGuardBlock(block))
	}
	return out
}

func hydrateAccountRouteGuardStoreFromPersistedRuntimeStates(store *AccountRouteGuardStore, now time.Time) error {
	if store == nil {
		return nil
	}
	states, err := loadPersistedChannelRuntimeStates()
	if err != nil || len(states) == 0 {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	blocks := make([]AccountRouteGuardBlock, 0, len(states))
	for key, state := range states {
		blocks = append(blocks, accountRouteGuardBlocksFromPersistedState(key, state, now)...)
	}
	if len(blocks) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, block := range blocks {
		block = normalizeAccountRouteGuardBlock(block)
		blockKey := accountRouteGuardBlockKey(block)
		if block.Source == "" || blockKey == "" {
			continue
		}
		store.removeLocked(block.Source, blockKey)
		if store.blocks[block.Source] == nil {
			store.blocks[block.Source] = map[string]AccountRouteGuardBlock{}
		}
		store.blocks[block.Source][blockKey] = block
		store.indexLocked(block)
	}
	return nil
}

func persistAccountRouteGuardBlock(block AccountRouteGuardBlock) {
	block = normalizeAccountRouteGuardBlock(block)
	accountID := persistedAccountRuntimeIDForBlock(block)
	if accountID == "" || block.Source == "" || !isPersistedChannelRuntimeSourceRouteBlocking(block.Source) {
		return
	}
	channelRoutingRuntimeStateFileMu.Lock()
	defer channelRoutingRuntimeStateFileMu.Unlock()
	store, path, err := loadChannelRoutingPolicyStoreForWrite()
	if err != nil {
		return
	}
	if store.RuntimeStates == nil {
		store.RuntimeStates = map[string]persistedChannelAccountRuntimeState{}
	}
	state := store.RuntimeStates[accountID]
	state.AccountID = accountID
	if state.Sources == nil {
		state.Sources = map[string]persistedChannelRuntimeStateSource{}
	}
	source := persistedRuntimeSourceFromRouteGuardBlock(block)
	state.Sources[block.Source] = source
	state.UpdatedAt = source.UpdatedAt
	store.RuntimeStates[accountID] = state
	_ = writeChannelRoutingPolicyStore(path, store)
}

func replacePersistedAccountRouteGuardSource(source string, blocks []AccountRouteGuardBlock) {
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	channelRoutingRuntimeStateFileMu.Lock()
	defer channelRoutingRuntimeStateFileMu.Unlock()
	store, path, err := loadChannelRoutingPolicyStoreForWrite()
	if err != nil {
		return
	}
	removePersistedRouteGuardSourceLocked(store.RuntimeStates, source)
	if !isPersistedChannelRuntimeSourceRouteBlocking(source) {
		_ = writeChannelRoutingPolicyStore(path, store)
		return
	}
	if store.RuntimeStates == nil {
		store.RuntimeStates = map[string]persistedChannelAccountRuntimeState{}
	}
	for _, block := range blocks {
		block.Source = source
		block = normalizeAccountRouteGuardBlock(block)
		accountID := persistedAccountRuntimeIDForBlock(block)
		if accountID == "" {
			continue
		}
		state := store.RuntimeStates[accountID]
		state.AccountID = accountID
		if state.Sources == nil {
			state.Sources = map[string]persistedChannelRuntimeStateSource{}
		}
		persistedSource := persistedRuntimeSourceFromRouteGuardBlock(block)
		state.Sources[source] = persistedSource
		state.UpdatedAt = persistedSource.UpdatedAt
		store.RuntimeStates[accountID] = state
	}
	_ = writeChannelRoutingPolicyStore(path, store)
}

func clearPersistedAccountRouteGuardSource(source string) {
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	channelRoutingRuntimeStateFileMu.Lock()
	defer channelRoutingRuntimeStateFileMu.Unlock()
	store, path, err := loadChannelRoutingPolicyStoreForWrite()
	if err != nil {
		return
	}
	removePersistedRouteGuardSourceLocked(store.RuntimeStates, source)
	_ = writeChannelRoutingPolicyStore(path, store)
}

func clearPersistedAccountRouteGuardAuth(source string, authID string, blocks []AccountRouteGuardBlock) {
	source = strings.TrimSpace(source)
	authID = strings.TrimSpace(authID)
	if source == "" || authID == "" && len(blocks) == 0 {
		return
	}
	channelRoutingRuntimeStateFileMu.Lock()
	defer channelRoutingRuntimeStateFileMu.Unlock()
	store, path, err := loadChannelRoutingPolicyStoreForWrite()
	if err != nil {
		return
	}
	targets := map[string]struct{}{}
	if authID != "" {
		targets[authID] = struct{}{}
		targets["auth-id:"+authID] = struct{}{}
	}
	for _, block := range blocks {
		if id := persistedAccountRuntimeIDForBlock(block); id != "" {
			targets[id] = struct{}{}
		}
	}
	for target := range targets {
		state, ok := store.RuntimeStates[target]
		if !ok || len(state.Sources) == 0 {
			continue
		}
		delete(state.Sources, source)
		if len(state.Sources) == 0 {
			delete(store.RuntimeStates, target)
			continue
		}
		state.UpdatedAt = latestPersistedSourceUpdatedAt(state.Sources)
		store.RuntimeStates[target] = state
	}
	_ = writeChannelRoutingPolicyStore(path, store)
}

func removePersistedRouteGuardSourceLocked(states map[string]persistedChannelAccountRuntimeState, source string) {
	for accountID, state := range states {
		if len(state.Sources) == 0 {
			continue
		}
		delete(state.Sources, source)
		if len(state.Sources) == 0 {
			delete(states, accountID)
			continue
		}
		state.UpdatedAt = latestPersistedSourceUpdatedAt(state.Sources)
		states[accountID] = state
	}
}

func loadPersistedChannelRuntimeStates() (map[string]persistedChannelAccountRuntimeState, error) {
	path, ok := explicitChannelRoutingPolicyConfigPath()
	if !ok {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var store channelRoutingPolicyStore
	if err := json.Unmarshal(body, &store); err != nil {
		return nil, err
	}
	return store.RuntimeStates, nil
}

func loadChannelRoutingPolicyStoreForWrite() (channelRoutingPolicyStore, string, error) {
	path, ok := explicitChannelRoutingPolicyConfigPath()
	if !ok {
		return channelRoutingPolicyStore{}, "", os.ErrNotExist
	}
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return channelRoutingPolicyStore{}, "", err
	}
	store := channelRoutingPolicyStore{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &store); err != nil {
			return channelRoutingPolicyStore{}, "", err
		}
	}
	if store.Channels == nil {
		store.Channels = map[string]json.RawMessage{}
	}
	if store.RuntimeStates == nil {
		store.RuntimeStates = map[string]persistedChannelAccountRuntimeState{}
	}
	return store, path, nil
}

func explicitChannelRoutingPolicyConfigPath() (string, bool) {
	channelRoutingPolicyConfigPathState.RLock()
	path := strings.TrimSpace(channelRoutingPolicyConfigPathState.path)
	channelRoutingPolicyConfigPathState.RUnlock()
	if path == "" {
		return "", false
	}
	return path, true
}

func writeChannelRoutingPolicyStore(path string, store channelRoutingPolicyStore) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if store.Channels == nil {
		store.Channels = map[string]json.RawMessage{}
	}
	if len(store.RuntimeStates) == 0 {
		store.RuntimeStates = nil
	}
	body, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func isPersistedChannelRuntimeSourceRouteBlocking(source string) bool {
	switch strings.TrimSpace(source) {
	case "", AccountRouteGuardSourceManualDisabled:
		return false
	default:
		return true
	}
}

func persistedRuntimeSourceFromRouteGuardBlock(block AccountRouteGuardBlock) persistedChannelRuntimeStateSource {
	updatedAt := block.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	source := persistedChannelRuntimeStateSource{
		Source:    strings.TrimSpace(block.Source),
		Scope:     string(normalizeRouteResilienceScope(block.FailureScope, block.Source)),
		Reason:    strings.TrimSpace(block.Reason),
		Model:     strings.TrimSpace(block.Model),
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339Nano),
	}
	if !block.ExpiresAt.IsZero() {
		source.ExpiresAt = block.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return source
}

func persistedAccountRuntimeIDForBlock(block AccountRouteGuardBlock) string {
	if key := strings.TrimSpace(block.AccountKey); strings.HasPrefix(key, "acct_") {
		return key
	}
	if id := strings.TrimSpace(block.AuthID); id != "" {
		return "auth-id:" + id
	}
	if key := strings.TrimSpace(block.AccountKey); key != "" {
		return key
	}
	return ""
}

func latestPersistedSourceUpdatedAt(sources map[string]persistedChannelRuntimeStateSource) string {
	latest := ""
	for _, source := range sources {
		if strings.TrimSpace(source.UpdatedAt) > latest {
			latest = strings.TrimSpace(source.UpdatedAt)
		}
	}
	return latest
}

func parseRouteGuardTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
	}
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func firstNonEmptyRouteGuardString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
