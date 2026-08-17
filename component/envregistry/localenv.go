package envregistry

import (
	"fmt"
	"os"
	"path/filepath"
)

// LocalEnvFilename is the conventional name of the per-repo override
// file. Each repo's root may contain one; it is git-ignored and
// shadows the matching key from the host shell for local development.
// A binary in a cluster carries no .env -- the memql-secrets Secret it
// envFroms is authoritative there.
const LocalEnvFilename = ".env"

// ReservedFromOverride lists names that the .env override MUST NOT
// touch.
//
// RE-ARGUED, NOT INHERITED (epic memql#3958). The original reason was that
// MEMQL_MASTER_KEY is the trust anchor of the sealed genesis envelope, so a
// local file rewriting it would silently switch which envelope a developer was
// decrypting. That envelope is gone, and the entry stays anyway -- for a reason
// that survives it and is arguably sharper.
//
// The master key still DECRYPTS: every v1:platform:globalSecret row in the
// cluster's database is sealed under it (component/secret.Encrypt/Decrypt). A
// repo-root .env silently swapping it does not produce an error the developer
// can act on -- it produces rows that will not decrypt, from a file they
// probably forgot was there, against a database that is fine. Rotating it is a
// deliberate act with a blast radius, so it is spelled deliberately: export it,
// or seed it. Everything else is fair game for local tuning.
//
// It is also NOT the operator credential; that is MEMQL_OPERATOR_KEY, a
// different secret since memql#3519, and it is not reserved here.
var ReservedFromOverride = map[string]struct{}{
	"MEMQL_MASTER_KEY": {},
}

// ApplyLocalOverride looks for repoRoot/.env, parses it, and applies
// each entry to the process environment via os.Setenv -- overriding
// any value already set by the host shell -- except for names in
// ReservedFromOverride.
//
// Returns the list of variable names actually overridden (for
// startup-log visibility) and any error from parsing. A missing
// .env is not an error; the function returns nil overrides.
//
// Call this once, early in main(), BEFORE any component reads its
// config. Order matters: the process environment (the host shell, or
// the memql-secrets Secret a pod envFroms) paints the baseline, and
// .env paints over it.
func ApplyLocalOverride(repoRoot string) (overridden []string, err error) {
	path := filepath.Join(repoRoot, LocalEnvFilename)
	entries, err := ParseEnvFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("envregistry: local override: %w", err)
	}
	for _, e := range entries {
		if _, reserved := ReservedFromOverride[e.Name]; reserved {
			continue
		}
		if err := os.Setenv(e.Name, e.Value); err != nil {
			return overridden, fmt.Errorf("envregistry: local override setenv %s: %w", e.Name, err)
		}
		overridden = append(overridden, e.Name)
	}
	return overridden, nil
}
