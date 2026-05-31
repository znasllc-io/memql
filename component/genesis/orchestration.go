package genesis

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql/component/secret"
)

// ReconcileAction describes what ReconcileMasterKey did to a slice
// of entries: nothing, replaced an existing mismatching value, or
// appended a new entry.
type ReconcileAction int

const (
	ReconcileNoop ReconcileAction = iota
	ReconcileReplaced
	ReconcileAdded
)

// ReconcileMasterKey makes the in-envelope MEMQL_MASTER_KEY entry
// match masterKeyHex (the key used to seal the envelope). The
// returned slice shares backing storage with the input when the
// value was replaced in place; callers should treat the input as
// consumed.
func ReconcileMasterKey(entries []EnvEntry, masterKeyHex string) ([]EnvEntry, ReconcileAction) {
	for i := range entries {
		if entries[i].Name != secret.EnvMasterKey {
			continue
		}
		if entries[i].Value == masterKeyHex {
			return entries, ReconcileNoop
		}
		entries[i].Value = masterKeyHex
		return entries, ReconcileReplaced
	}
	return append(entries, EnvEntry{Name: secret.EnvMasterKey, Value: masterKeyHex}), ReconcileAdded
}

// GenerateMasterKey returns 32 random bytes as a 64-character lowercase
// hex string. The hex form is what memql binaries expect in
// MEMQL_MASTER_KEY.
func GenerateMasterKey() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// ValidateMasterKeyHex checks that s looks like a generated master
// key: lowercase/uppercase hex, 64 chars (32 raw bytes). Used to
// reject garbage env values before sealing under them.
func ValidateMasterKeyHex(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("expected 64 hex chars (32 bytes), got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return fmt.Errorf("not valid hex")
	}
	return nil
}

// WriteGenesisAtomic writes envelope to outPath via a same-directory
// temp file + rename, so the destination is never left half-written.
// Permission bits on the final file are 0600. On any error before
// the rename, the previous file at outPath is untouched.
func WriteGenesisAtomic(outPath string, envelope []byte) error {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(outPath)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(envelope); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, outPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, outPath, err)
	}
	cleanup = false
	return nil
}

// SealOptions configures a Seal run.
type SealOptions struct {
	// Entries are the parsed .env rows. ReconcileMasterKey runs over
	// them in-place before sealing, so the caller should treat the
	// slice as consumed after Seal returns.
	Entries []EnvEntry

	// Manifest is the floor used for strict-superset validation. When
	// non-nil, Seal returns an error if any manifest entry is missing
	// from Entries; pass nil to skip validation (advanced callers).
	Manifest *Manifest

	// OutPath is where the sealed genesis envelope is written.
	OutPath string

	// SourceEnvPath is the .env file the entries came from. When
	// non-empty Seal reconciles MEMQL_MASTER_KEY in that file after a
	// successful seal so source + envelope stay in lockstep. Empty
	// = skip source-file sync.
	SourceEnvPath string

	// SyncShellRCs controls whether ~/.bashrc and ~/.zshrc get
	// MEMQL_MASTER_KEY synced to the sealing key. Useful default for
	// the cockpit wizard; identity / scripted callers can opt out.
	SyncShellRCs bool

	// MasterKey, if non-empty and valid hex, is the key Seal will
	// use. Otherwise Seal generates one (first-time) or reads
	// MEMQL_MASTER_KEY from the process environment (update).
	MasterKey string

	// ConfirmMasterKey is invoked exactly once when Seal generated a
	// fresh master key. The callback receives the hex key (so it can
	// be displayed to the operator) and returns true if the operator
	// has saved it. Returning false aborts the seal without writing.
	// A nil callback auto-confirms (used by automated / scripted
	// callers where there is no operator to wait for).
	ConfirmMasterKey func(masterKeyHex string) (bool, error)
}

// SealResult reports what Seal actually did, so callers can render
// useful status messages.
type SealResult struct {
	MasterKey        string          // the key used (newly generated, reused, or supplied)
	FirstTime        bool            // OutPath did not exist before this run
	ReusedKeyFromEnv bool            // first-time path, key came from MEMQL_MASTER_KEY in env
	EntriesWritten   int             // number of entries in the envelope
	ManifestSource   string          // manifest.Source if validation ran
	Reconcile        ReconcileAction // what happened to the in-envelope MEMQL_MASTER_KEY
}

// Seal validates a set of env entries against the manifest, encrypts
// them under MEMQL_MASTER_KEY, and writes the result to OutPath.
//
// First-time / update is decided by whether OutPath already exists:
//   - First-time: a fresh master key is generated (unless one is
//     already in the process environment, in which case Seal reuses
//     it so the operator's existing shell config keeps working).
//     ConfirmMasterKey is invoked when a key is generated; the
//     callback must return true before anything is written.
//   - Update: MEMQL_MASTER_KEY must be set in the process environment
//     and must decrypt the existing envelope. Seal re-encrypts under
//     that same key. ConfirmMasterKey is NOT invoked on update.
//
// After a successful write Seal optionally syncs the in-envelope
// MEMQL_MASTER_KEY assignment back into SourceEnvPath, and the same
// key into shell rc files when SyncShellRCs is true. Both syncs are
// best-effort: failures are returned via the error but the sealed
// envelope is already on disk and the master key in SealResult is
// authoritative.
func Seal(opts SealOptions) (*SealResult, error) {
	if opts.OutPath == "" {
		return nil, fmt.Errorf("seal: OutPath is required")
	}

	if opts.Manifest != nil {
		if missing := FindMissing(opts.Entries, opts.Manifest.Names()); len(missing) > 0 {
			return nil, fmt.Errorf("manifest violation: missing required entries: %s (manifest source: %s)", strings.Join(missing, ", "), opts.Manifest.Source)
		}
	}

	res := &SealResult{}
	firstTime := !fileExists(opts.OutPath)
	res.FirstTime = firstTime

	masterKeyHex := strings.TrimSpace(opts.MasterKey)
	if masterKeyHex != "" {
		if err := ValidateMasterKeyHex(masterKeyHex); err != nil {
			return nil, fmt.Errorf("seal: caller-supplied MasterKey invalid: %w", err)
		}
	}

	if firstTime {
		if masterKeyHex == "" {
			if envKey := strings.TrimSpace(os.Getenv(secret.EnvMasterKey)); envKey != "" {
				if verr := ValidateMasterKeyHex(envKey); verr == nil {
					masterKeyHex = envKey
					res.ReusedKeyFromEnv = true
				}
			}
		}
		if masterKeyHex == "" {
			generated, err := GenerateMasterKey()
			if err != nil {
				return nil, fmt.Errorf("seal: generate master key: %w", err)
			}
			masterKeyHex = generated
		}
	} else {
		if masterKeyHex == "" {
			masterKeyHex = strings.TrimSpace(os.Getenv(secret.EnvMasterKey))
		}
		if masterKeyHex == "" {
			return nil, fmt.Errorf("seal: %s exists; supply MEMQL_MASTER_KEY (env or SealOptions.MasterKey) to update it", opts.OutPath)
		}
		existing, rerr := os.ReadFile(opts.OutPath)
		if rerr != nil {
			return nil, fmt.Errorf("seal: read existing %s: %w", opts.OutPath, rerr)
		}
		// Ensure the supplied key decrypts the existing envelope
		// BEFORE we re-seal -- protects against re-keying by accident
		// when the operator's env has drifted from the file.
		prev := os.Getenv(secret.EnvMasterKey)
		if err := os.Setenv(secret.EnvMasterKey, masterKeyHex); err != nil {
			return nil, fmt.Errorf("seal: set %s in-process: %w", secret.EnvMasterKey, err)
		}
		_, derr := secret.OpenBlob(existing)
		_ = os.Setenv(secret.EnvMasterKey, prev)
		if derr != nil {
			return nil, fmt.Errorf("seal: %s won't decrypt with the supplied master key: %w", opts.OutPath, derr)
		}
	}

	// Confirmation gate -- only when we generated a fresh key.
	// Reusing from env or update paths skip this; the operator
	// already has the key in hand.
	freshlyGenerated := firstTime && !res.ReusedKeyFromEnv && opts.MasterKey == ""
	if freshlyGenerated && opts.ConfirmMasterKey != nil {
		ok, err := opts.ConfirmMasterKey(masterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("seal: confirmation: %w", err)
		}
		if !ok {
			return nil, fmt.Errorf("seal: master key not confirmed -- nothing written")
		}
	}

	// In-process env must hold masterKeyHex so secret.SealBlob picks
	// it up. Restore on exit so callers don't see a surprise env mutation.
	prevKey := os.Getenv(secret.EnvMasterKey)
	if err := os.Setenv(secret.EnvMasterKey, masterKeyHex); err != nil {
		return nil, fmt.Errorf("seal: set %s in-process: %w", secret.EnvMasterKey, err)
	}
	defer func() { _ = os.Setenv(secret.EnvMasterKey, prevKey) }()

	entries, rec := ReconcileMasterKey(opts.Entries, masterKeyHex)
	res.Reconcile = rec
	res.MasterKey = masterKeyHex
	res.EntriesWritten = len(entries)
	if opts.Manifest != nil {
		res.ManifestSource = opts.Manifest.Source
	}

	plaintext := SerializeEntries(entries)
	envelope, err := secret.SealBlob(plaintext)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutPath), 0o700); err != nil {
		return nil, fmt.Errorf("seal: mkdir %s: %w", filepath.Dir(opts.OutPath), err)
	}
	if err := WriteGenesisAtomic(opts.OutPath, envelope); err != nil {
		return nil, fmt.Errorf("seal: write %s: %w", opts.OutPath, err)
	}

	// Best-effort source-env reconcile. Only when reconcile actually
	// changed the in-memory entries -- a no-op there means the source
	// file is already correct.
	if opts.SourceEnvPath != "" && rec != ReconcileNoop {
		if err := SyncSourceEnvFile(opts.SourceEnvPath, masterKeyHex); err != nil {
			// Non-fatal: envelope is the artifact; bubble the error
			// so the caller can log it but don't unwind the seal.
			res.MasterKey = masterKeyHex
			return res, fmt.Errorf("seal: source .env sync (%s): %w (envelope written, source out of sync)", opts.SourceEnvPath, err)
		}
	}

	if opts.SyncShellRCs {
		paths, perr := RcCandidatePaths()
		if perr == nil {
			for _, p := range paths {
				if _, rerr := ReconcileShellRC(p, masterKeyHex); rerr != nil {
					// Same logic: warn via error wrap, don't unwind.
					return res, fmt.Errorf("seal: shell rc sync (%s): %w (envelope written, shell rc out of sync)", p, rerr)
				}
			}
		}
	}

	return res, nil
}

// SyncSourceEnvFile rewrites path so its MEMQL_MASTER_KEY line
// carries masterKeyHex. Preserves comments, blank lines, and
// formatting; only the assignment line itself changes (or a fresh
// one appends to the end). Used internally by Seal but exported for
// callers that manage source-file sync themselves.
func SyncSourceEnvFile(path, masterKeyHex string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	out, replaced, appended := RewriteMasterKeyAssignment(raw, masterKeyHex)
	if !replaced && !appended {
		return nil // nothing to do
	}
	return writeRCAtomic(path, out, info.Mode().Perm())
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// OpenFile reads a sealed genesis envelope, decrypts it under
// MEMQL_MASTER_KEY (from the process environment), and parses the
// inner .env content into EnvEntry rows in source order. Used by
// memql binaries that need to look up settings inside a sealed
// envelope without re-implementing the secret-package boilerplate.
//
// The master key must be available in the process env via
// secret.EnvMasterKey before calling. Returns a typed error if the
// file is missing, the key is wrong, or the payload doesn't parse
// as a .env.
func OpenFile(path string) ([]EnvEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("genesis: read %s: %w", path, err)
	}
	entries, err := openBytes(raw, path)
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// OpenBytes decrypts a sealed genesis envelope held in memory and
// parses the inner .env content into EnvEntry rows in source order.
// It is the in-memory twin of OpenFile: same decrypt + parse core,
// but the ciphertext never touches disk. Used by the cloud auto-load
// path where the envelope arrives base64-encoded in an env var and
// must be decrypted in-process (writing it to a temp file would
// defeat the at-rest goal).
//
// The master key must be available in the process env via
// secret.EnvMasterKey before calling. Returns a typed error if the
// key is wrong or the payload doesn't parse as a .env.
func OpenBytes(data []byte) ([]EnvEntry, error) {
	return openBytes(data, "<genesis-envelope>")
}

// openBytes is the shared decrypt+parse core behind OpenFile and
// OpenBytes. label is used in error messages and parse diagnostics.
func openBytes(data []byte, label string) ([]EnvEntry, error) {
	plaintext, err := secret.OpenBlob(data)
	if err != nil {
		return nil, fmt.Errorf("genesis: open %s: %w", label, err)
	}
	return parseEnv(strings.NewReader(string(plaintext)), label)
}

// LookupEnv returns the value of an entry by name, with ok=false
// when the name isn't present. Convenience for callers that fetch
// a single setting (typical pattern: get IDENTITY_BOOTSTRAP_DOMAIN
// out of an opened envelope).
func LookupEnv(entries []EnvEntry, name string) (string, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}
