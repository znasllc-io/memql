package main

import (
	"fmt"
	"os"

	"github.com/znasllc-io/memql/component/envregistry"
)

// applySubcommandEnv layers the repo-root /.env onto the process environment,
// then the legacy-name bridge and the domain derivations, mirroring the
// server-boot bootstrap in main().
//
// Why this exists (znasllc-io/memql#751): main() dispatches subcommands before
// it runs the boot-time env layering, so a subcommand that skipped this saw a
// bare process environment. The original case was a sealed genesis envelope
// the credential-minting subcommands could not reach into; the envelope is
// gone (epic memql#3958) and the /.env layer, the legacy aliases and the
// domain derivations are not -- a subcommand still has to apply all three or
// it reports a different environment than the node it runs beside.
//
// The /.env layer is best-effort: it is absent in a cluster, where this call
// is a no-op.
func applySubcommandEnv(prefix string) error {
	if _, err := envregistry.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "%s: local .env override failed: %v\n", prefix, err)
	}
	// Epic 7.3 (memql#2106): bridge any pre-7.3 LEGACY env names the Secret or
	// .env still carries onto their new MEMQL_ names (set-if-absent), mirroring
	// main(). Without it a subcommand sees only the legacy names: the voice-agent
	// subcommand fail-fast'd on the required MEMQL_OPENAI_API_KEY on a cluster
	// still carrying the legacy OPENAI_API_KEY. nil logger:
	// this path logs to stderr via Fprintf, not slog, so the bridge runs silently.
	envregistry.ApplyLegacyEnvAliases(nil)
	// Mirrors main(): `memql env` must report the environment a node actually
	// boots with, derivations included (memql#3593). nil logger -- this path
	// writes to stderr via Fprintf, not slog.
	envregistry.ApplyDomainDerivations(nil)
	return nil
}
