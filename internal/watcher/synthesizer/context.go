package synthesizer

import (
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
	// AccountStoreAccounts caches sidecar-owned accounts for one synthesis pass.
	AccountStoreAccounts []accountstore.AccountRecord
}
