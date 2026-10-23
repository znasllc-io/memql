package config

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/99designs/keyring"
)

// KeyringServiceName is the OS-keyring "service" identifier the
// cockpit registers under. Stable so the keyring entries the user
// granted access to stay accessible across upgrades. Per-cluster
// entries are keyed by the cluster name within this service.
const KeyringServiceName = "com.znasllc.memql-cockpit"

// KeyringStore implements CredentialStore over the OS keyring via
// 99designs/keyring. Selected backends mirror the platform set
// shipped on the cockpit's tier-1 targets:
//
//   - darwin -- Keychain
//   - linux  -- Secret Service (gnome-keyring / KWallet via libsecret)
//   - windows -- Credential Manager (covered when Windows lands)
//
// The 99designs library can fall back to its own encrypted file
// backend, but we explicitly DISALLOW it -- the whole point of
// memql-cockpit#65 is to NOT have plaintext-equivalent at-rest
// storage. If the platform keyring is unreachable, Resolve()
// downgrades to FileStore so the failure is loud at startup, not
// a silent re-introduction of the disk-blob path.
type KeyringStore struct {
	kr keyring.Keyring
}

// NewKeyringStore opens the OS keyring under the cockpit service
// name. Returns an error when the platform doesn't expose a usable
// keyring -- callers can probe this to decide whether to fall back
// to FileStore.
func NewKeyringStore() (*KeyringStore, error) {
	kr, err := keyring.Open(keyring.Config{
		ServiceName:                    KeyringServiceName,
		AllowedBackends:                allowedKeyringBackends(),
		KeychainTrustApplication:       true,
		KeychainAccessibleWhenUnlocked: true,
		KeychainSynchronizable:         false,
		LibSecretCollectionName:        "memql-cockpit",
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	return &KeyringStore{kr: kr}, nil
}

// allowedKeyringBackends returns the OS-native list. Explicitly
// EXCLUDES the library's BackendFile / KWallet's file fallback
// modes so we never silently re-introduce a plaintext disk store.
func allowedKeyringBackends() []keyring.BackendType {
	return []keyring.BackendType{
		keyring.KeychainBackend,      // darwin
		keyring.SecretServiceBackend, // linux (gnome-keyring / KWallet via libsecret)
		keyring.WinCredBackend,       // windows
	}
}

// Name reports the backend identifier for logs / the `creds
// status` command.
func (k *KeyringStore) Name() string { return "keyring" }

// Get reads the StoredToken stored under cluster from the keyring.
// (nil, nil) on miss -- callers' "no cached token" branch is the
// same shape FileStore returns.
func (k *KeyringStore) Get(cluster string) (*StoredToken, error) {
	if k == nil || k.kr == nil {
		return nil, errors.New("keyring store not initialized")
	}
	item, err := k.kr.Get(cluster)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("keyring get %s: %w", cluster, err)
	}
	var t StoredToken
	if err := json.Unmarshal(item.Data, &t); err != nil {
		return nil, fmt.Errorf("keyring entry %s: parse stored token: %w", cluster, err)
	}
	return &t, nil
}

// Put writes the token under the cluster key. Overwrites any
// existing entry without prompting (mirrors FileStore semantics).
func (k *KeyringStore) Put(cluster string, token *StoredToken) error {
	if k == nil || k.kr == nil {
		return errors.New("keyring store not initialized")
	}
	if token == nil {
		return fmt.Errorf("keyring store: refuse to persist nil token for %q", cluster)
	}
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	item := keyring.Item{
		Key:         cluster,
		Data:        data,
		Label:       "memQL Cockpit (" + cluster + ")",
		Description: "Cached OAuth token for memQL cluster '" + cluster + "'",
	}
	if err := k.kr.Set(item); err != nil {
		return fmt.Errorf("keyring set %s: %w", cluster, err)
	}
	return nil
}

// Delete removes the keyring entry for cluster. Missing is a no-op.
func (k *KeyringStore) Delete(cluster string) error {
	if k == nil || k.kr == nil {
		return errors.New("keyring store not initialized")
	}
	err := k.kr.Remove(cluster)
	if err != nil && !errors.Is(err, keyring.ErrKeyNotFound) {
		return fmt.Errorf("keyring remove %s: %w", cluster, err)
	}
	return nil
}

// Keys returns the cluster names with an entry in the keyring.
// Used by the migration / status commands to enumerate what's
// stored without the user having to remember every cluster name.
func (k *KeyringStore) Keys() ([]string, error) {
	if k == nil || k.kr == nil {
		return nil, errors.New("keyring store not initialized")
	}
	return k.kr.Keys()
}
