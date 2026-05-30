package genesis

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/secret"
)

// Env var names that gate and feed the cloud A2 auto-load path.
const (
	// EnvAutoload, when "true", opts the boot sequence into decrypting
	// and applying the genesis envelope in-process. Unset / any other
	// value leaves boot behavior unchanged (local dev's docker-compose
	// env_file path is untouched).
	EnvAutoload = "MEMQL_GENESIS_AUTOLOAD"

	// EnvGenesisB64 carries the base64-encoded ENCRYPTED envelope
	// directly in an env var. Preferred for cloud: the ciphertext
	// never lands on disk; it is decoded + decrypted in memory.
	EnvGenesisB64 = "MEMQL_GENESIS_B64"

	// EnvGenesisPath points at a sealed envelope file on disk. Used
	// when EnvGenesisB64 is absent; defaults to ~/.memql/genesis.znas.
	EnvGenesisPath = "MEMQL_GENESIS_PATH"
)

// DefaultGenesisPath is the fallback location for the sealed envelope
// when MEMQL_GENESIS_PATH is unset: ~/.memql/genesis.znas.
const DefaultGenesisPath = "~/.memql/genesis.znas"

// AutoloadResult reports what AutoloadFromEnv did, for startup-log
// visibility.
type AutoloadResult struct {
	// Enabled is false when MEMQL_GENESIS_AUTOLOAD was not "true";
	// every other field is zero in that case (no-op).
	Enabled bool

	// Source describes where the envelope came from: "b64" (from
	// MEMQL_GENESIS_B64) or the resolved file path.
	Source string

	// Applied lists the env var names actually set by the auto-load
	// (i.e. those that were absent before -- set-if-absent semantics).
	Applied []string

	// Skipped lists the env var names that were already set in the
	// process environment and therefore left untouched (Container App
	// overrides win).
	Skipped []string
}

// AutoloadFromEnv is the cloud A2 boot hook. When
// MEMQL_GENESIS_AUTOLOAD=true it sources the ENCRYPTED genesis
// envelope (preferring the in-memory MEMQL_GENESIS_B64 over the
// MEMQL_GENESIS_PATH file), decrypts it in-process under
// MEMQL_MASTER_KEY via the existing secret/genesis decrypt path, and
// applies each entry to the process environment SET-IF-ABSENT --
// never clobbering a value already present. This lets Container App
// overrides (Tiger DSN, node type, identity host URLs, allowed
// origins) win over the envelope's local-dev defaults.
//
// Fail-closed: when auto-load is enabled but no envelope source is
// available, the master key is missing, or decryption fails, it
// returns an error. Callers should treat that as fatal -- booting on
// from a partially-applied or wrong config is worse than not booting.
//
// When MEMQL_GENESIS_AUTOLOAD is not "true" it is a no-op and returns
// a result with Enabled=false and a nil error.
//
// Call this EARLY in main(), before any config is read, and before
// ApplyLocalOverride: the genesis envelope is the base layer
// (set-if-absent), then the repo-root .env override layers on top for
// local dev (overwrite). In cloud there is no .env, so the envelope +
// the pre-set Container App overrides are the whole story.
func AutoloadFromEnv() (*AutoloadResult, error) {
	if strings.TrimSpace(os.Getenv(EnvAutoload)) != "true" {
		return &AutoloadResult{Enabled: false}, nil
	}

	if strings.TrimSpace(os.Getenv(secret.EnvMasterKey)) == "" {
		return nil, fmt.Errorf("genesis autoload: %s=true but %s is not set; cannot decrypt the envelope", EnvAutoload, secret.EnvMasterKey)
	}

	entries, source, err := loadEnvelopeEntries()
	if err != nil {
		return nil, err
	}

	res := &AutoloadResult{Enabled: true, Source: source}
	for _, e := range entries {
		if _, present := os.LookupEnv(e.Name); present {
			res.Skipped = append(res.Skipped, e.Name)
			continue
		}
		if err := os.Setenv(e.Name, e.Value); err != nil {
			return nil, fmt.Errorf("genesis autoload: setenv %s: %w", e.Name, err)
		}
		res.Applied = append(res.Applied, e.Name)
	}
	return res, nil
}

// loadEnvelopeEntries resolves the envelope source (B64 preferred,
// else file) and returns the decrypted entries plus a source label.
func loadEnvelopeEntries() (entries []EnvEntry, source string, err error) {
	if b64 := strings.TrimSpace(os.Getenv(EnvGenesisB64)); b64 != "" {
		data, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			return nil, "", fmt.Errorf("genesis autoload: %s is not valid base64: %w", EnvGenesisB64, derr)
		}
		entries, err = OpenBytes(data)
		if err != nil {
			return nil, "", fmt.Errorf("genesis autoload: decrypt %s: %w", EnvGenesisB64, err)
		}
		return entries, "b64", nil
	}

	path, perr := resolveGenesisPath()
	if perr != nil {
		return nil, "", perr
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, "", fmt.Errorf("genesis autoload: %s=true but no envelope source: %s is empty and %s does not exist", EnvAutoload, EnvGenesisB64, path)
		}
		return nil, "", fmt.Errorf("genesis autoload: stat %s: %w", path, statErr)
	}
	entries, err = OpenFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("genesis autoload: %w", err)
	}
	return entries, path, nil
}

// resolveGenesisPath returns the configured envelope file path,
// defaulting to ~/.memql/genesis.znas, with a leading ~ expanded to
// the current user's home directory.
func resolveGenesisPath() (string, error) {
	raw := strings.TrimSpace(os.Getenv(EnvGenesisPath))
	if raw == "" {
		raw = DefaultGenesisPath
	}
	return expandHome(raw)
}

// expandHome rewrites a leading "~" or "~/" to the user's home
// directory. Other paths pass through unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("genesis autoload: resolve home for %q: %w", path, err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}
