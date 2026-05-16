package genesis

import (
	"fmt"
	"os"
	"path/filepath"
)

// LocalEnvFilename is the conventional name of the per-repo override
// file. Each repo's root may contain one; it is git-ignored and
// shadows the matching key in the genesis envelope for local
// development. Production binaries don't carry a .env -- the
// envelope is authoritative there.
const LocalEnvFilename = ".env"

// ReservedFromOverride lists names that the .env override MUST NOT
// touch. The master key is the trust anchor for the genesis envelope
// itself: letting a local file rewrite it would mean a developer can
// silently switch which envelope they're decrypting (or fail to
// decrypt anything) without realizing it. Genesis stays in charge of
// this one variable; everything else is fair game for local tuning.
var ReservedFromOverride = map[string]struct{}{
	"MEMQL_MASTER_KEY": {},
}

// ApplyLocalOverride looks for repoRoot/.env, parses it, and applies
// each entry to the process environment via os.Setenv -- overriding
// any value already set by the genesis envelope or the host shell --
// except for names in ReservedFromOverride.
//
// Returns the list of variable names actually overridden (for
// startup-log visibility) and any error from parsing. A missing
// .env is not an error; the function returns nil overrides.
//
// Call this once, early in main(), AFTER the genesis envelope has
// been applied to os.Environ and BEFORE any component reads its
// config. Order matters: genesis paints the baseline, .env paints
// over it.
func ApplyLocalOverride(repoRoot string) (overridden []string, err error) {
	path := filepath.Join(repoRoot, LocalEnvFilename)
	entries, err := ParseEnvFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("genesis: local override: %w", err)
	}
	for _, e := range entries {
		if _, reserved := ReservedFromOverride[e.Name]; reserved {
			continue
		}
		if err := os.Setenv(e.Name, e.Value); err != nil {
			return overridden, fmt.Errorf("genesis: local override setenv %s: %w", e.Name, err)
		}
		overridden = append(overridden, e.Name)
	}
	return overridden, nil
}
