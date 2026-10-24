package config

import (
	"sync"
	"time"
)

// StoredToken holds a cached OAuth2 token for a cluster.
type StoredToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry"`
}

// expiryBuffer is the slack window IsExpired enforces ahead of the
// actual Expiry timestamp. Treating tokens as "expired" a bit early
// keeps us from handing out an access token that will be rejected by
// the server seconds later (clock skew + in-flight latency). 60s is
// generous enough to absorb both while remaining small enough to be
// shorter than the smallest TTL operators are likely to set: the
// identity service caps the access TTL at 60s on the low end
// (MinAccessTokenTTLSeconds in memql core), so anything tighter would
// make every cached token look expired the moment it's saved.
//
// History: this used to be 5 minutes, which silently broke the
// refresh path under short-TTL test configs (3-min access tokens
// would always look expired, and pre-refresh-wiring that meant the
// browser flow fired on every dial). When we ship the refresh path,
// the buffer needs to be < TTL or refresh never runs proactively.
const expiryBuffer = 60 * time.Second

// IsExpired returns true if the token has expired or will expire
// within expiryBuffer.
func (t *StoredToken) IsExpired() bool {
	return time.Now().Add(expiryBuffer).After(t.Expiry)
}

// LoadToken / SaveToken / DeleteToken are the package-level
// shorthand the rest of the cockpit calls into. They route through
// the package's active CredentialStore, which Resolve() picks at
// startup and SetActiveStore() installs. Tests inject a fake
// directly via SetActiveStore.
//
// Backwards-compatibility note: before memql-cockpit#65 these three
// functions read/wrote ~/.memql/credentials/<cluster>.json
// directly. The behavior is preserved when the active store is a
// FileStore -- the only observable change is that when the keyring
// store wins resolution, the underlying I/O lands in the OS
// keyring instead.

var (
	storeMu     sync.RWMutex
	activeStore CredentialStore
)

// SetActiveStore installs the CredentialStore the package-level
// LoadToken / SaveToken / DeleteToken route through. The cockpit's
// boot path calls Resolve() and passes the result here; tests pass
// a fake. Passing nil restores the lazy-FileStore default.
func SetActiveStore(s CredentialStore) {
	storeMu.Lock()
	activeStore = s
	storeMu.Unlock()
}

// ActiveStore returns the currently-installed store. Lazily
// constructs a FileStore on first call when nothing was installed
// (preserves the pre-#65 behavior for code paths that load tokens
// before app boot has run resolution -- e.g. the migration
// command, the worker subcommand).
func ActiveStore() CredentialStore {
	storeMu.RLock()
	s := activeStore
	storeMu.RUnlock()
	if s != nil {
		return s
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if activeStore == nil {
		activeStore = NewFileStore()
	}
	return activeStore
}

// LoadToken reads a cached token for the given cluster.
//
// Routes through ActiveStore(). FileStore behavior is preserved
// (mode 0600 enforced + group/world-readable rejected); the
// keyring backend has no on-disk surface so the mode check
// doesn't apply there.
func LoadToken(clusterName string) (*StoredToken, error) {
	return ActiveStore().Get(clusterName)
}

// SaveToken writes a token via ActiveStore().
func SaveToken(clusterName string, token *StoredToken) error {
	return ActiveStore().Put(clusterName, token)
}

// DeleteToken removes the cached token via ActiveStore().
func DeleteToken(clusterName string) error {
	return ActiveStore().Delete(clusterName)
}
