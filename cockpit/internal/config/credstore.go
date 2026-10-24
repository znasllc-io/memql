package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// CredentialStore is the per-cluster OAuth-token persistence surface.
// Two implementations:
//
//   - FileStore -- the historical backend; tokens land in
//     ~/.memql/credentials/<cluster>.json at mode 0600. Always
//     available, used as the back-compat / dev / headless fallback.
//
//   - KeyringStore -- backed by the OS keyring (Keychain on darwin,
//     Secret Service / libsecret on linux, Credential Manager on
//     windows). Stores the marshaled StoredToken JSON under the
//     cluster name. Preferred when the platform exposes a working
//     keyring -- shipped per memql-cockpit#65 to eliminate the
//     plaintext-on-disk surface the cockpit had carried since v0.
//
// The Resolve() helper builds the active store from the chain:
// MEMQL_COCKPIT_CRED_STORE forces a specific backend, otherwise the
// keyring is tried first and the file store is the fallback. Each
// startup logs which backend won at INFO so the operator sees it.
//
// Get returns (nil, nil) for a missing entry -- the same shape
// LoadToken returned -- so callers can distinguish "no cached
// token" from a real I/O error without sentinel comparison.
type CredentialStore interface {
	Get(cluster string) (*StoredToken, error)
	Put(cluster string, token *StoredToken) error
	Delete(cluster string) error

	// Name is a short identifier suitable for log lines / the
	// `creds status` command. "file" / "keyring".
	Name() string
}

// ResolveOptions tunes the resolution chain. Tests pass an explicit
// Override + Probe to avoid touching the host OS keyring; production
// callers pass an empty value and rely on the env-var + auto-probe
// behavior.
type ResolveOptions struct {
	// Override forces a specific backend regardless of env / probe.
	// "" means "follow the chain." Honored values: "file", "keyring".
	Override string

	// KeyringProbe is the keyring-store factory. Defaults to
	// NewKeyringStore. Tests can plug a fake here that returns a
	// memory-backed store (or an error to simulate keyring
	// unavailability).
	KeyringProbe func() (CredentialStore, error)

	// FileStoreFactory builds the file backend. Defaults to the
	// real ~/.memql FileStore. Tests use t.TempDir().
	FileStoreFactory func() CredentialStore

	// Logger receives a single INFO-level line naming the chosen
	// backend. nil silences the choice.
	Logger *slog.Logger
}

// EnvCredStore is the env-var operator override. Values: "file" /
// "keyring". Anything else is ignored with a logged warning.
const EnvCredStore = "MEMQL_COCKPIT_CRED_STORE"

// Resolve picks the active CredentialStore per the chain documented
// on CredentialStore. The returned store is ready to use; errors at
// this layer are configuration mistakes (override names a backend
// that won't initialize) -- a healthy host always resolves to one
// of the two stores.
func Resolve(opts ResolveOptions) (CredentialStore, error) {
	override := strings.ToLower(strings.TrimSpace(opts.Override))
	if override == "" {
		override = strings.ToLower(strings.TrimSpace(os.Getenv(EnvCredStore)))
	}
	keyringProbe := opts.KeyringProbe
	if keyringProbe == nil {
		keyringProbe = func() (CredentialStore, error) { return NewKeyringStore() }
	}
	fileFactory := opts.FileStoreFactory
	if fileFactory == nil {
		fileFactory = func() CredentialStore { return NewFileStore() }
	}
	switch override {
	case "file":
		s := fileFactory()
		logChoice(opts.Logger, s, "operator override")
		return s, nil
	case "keyring":
		s, err := keyringProbe()
		if err != nil {
			return nil, fmt.Errorf("MEMQL_COCKPIT_CRED_STORE=keyring but keyring unavailable: %w", err)
		}
		logChoice(opts.Logger, s, "operator override")
		return s, nil
	case "":
		// fall through to auto-probe
	default:
		if opts.Logger != nil {
			opts.Logger.Warn("ignoring unrecognized MEMQL_COCKPIT_CRED_STORE value (expected 'file' or 'keyring')",
				"value", override)
		}
	}
	if s, err := keyringProbe(); err == nil && s != nil {
		logChoice(opts.Logger, s, "auto-probe")
		return s, nil
	} else if err != nil && opts.Logger != nil {
		opts.Logger.Info("OS keyring unavailable; falling back to file store",
			"error", err)
	}
	s := fileFactory()
	logChoice(opts.Logger, s, "keyring fallback")
	return s, nil
}

func logChoice(l *slog.Logger, s CredentialStore, reason string) {
	if l == nil || s == nil {
		return
	}
	l.Info("credential store active",
		"backend", s.Name(),
		"reason", reason,
	)
}

// ErrNotFound is returned by store probe helpers when a key isn't
// present. Stores translate to (nil, nil) at the public Get() so
// the caller's missing-token branch is the same shape it was
// pre-refactor.
var ErrNotFound = errors.New("credential not found")
