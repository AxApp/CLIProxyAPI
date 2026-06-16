package synthesizer

import (
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/gettokens/accountstore"
)

// SynthesisContext provides the context needed for auth synthesis.
type SynthesisContext struct {
	// Config is the current configuration
	Config *config.Config
	// AuthDir is the directory containing auth files
	AuthDir string
	// Now is the current time for timestamps
	Now time.Time
	// IDGenerator generates stable IDs for auth entries
	IDGenerator *StableIDGenerator
	// AccountStoreLoaded records whether AccountStoreAccounts has been resolved
	// for this synthesis pass.
	AccountStoreLoaded bool
	// AccountStoreActive records whether the sidecar account store exists and
	// was readable for this synthesis pass, even when it has zero active rows.
	AccountStoreActive bool
	// AccountStoreLoadError records why a configured account store could not be
	// read. Callers that reconcile hot runtime state must not treat this as an
	// authoritative empty account store.
	AccountStoreLoadError error
	// AccountStoreAccounts caches sidecar-owned accounts for one synthesis pass.
	AccountStoreAccounts []accountstore.AccountRecord
}

func (c *SynthesisContext) SetAccountStoreLoadError(err error) {
	if c == nil || err == nil {
		return
	}
	c.AccountStoreLoadError = err
}

func (c *SynthesisContext) AccountStoreLoadErrorf(format string, args ...any) {
	if c == nil {
		return
	}
	c.SetAccountStoreLoadError(fmt.Errorf(format, args...))
}
