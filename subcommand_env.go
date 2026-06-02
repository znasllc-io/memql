package main

import (
	"fmt"
	"os"

	"github.com/znasllc-io/memql/component/genesis"
)

// applySubcommandEnv layers the genesis envelope (when MEMQL_GENESIS_AUTOLOAD=
// true) and then the repo-root /.env onto the process environment, mirroring the
// server-boot bootstrap in main().
//
// Why this exists (znasllc-io/memql#751): main() dispatches subcommands BEFORE it
// runs genesis.AutoloadFromEnv(), so a subcommand that only called
// ApplyLocalOverride never decrypted the genesis envelope. On an envelope-based
// deployment (staging/prod: MEMQL_GENESIS_AUTOLOAD=true, the signing key + other
// secrets sealed in MEMQL_GENESIS_B64) the credential-minting subcommands could
// not reach sealed values like IDENTITY_SIGNING_KEY_B64 -- the #691
// service_account mint failed for exactly this reason.
//
// AutoloadFromEnv is a no-op when MEMQL_GENESIS_AUTOLOAD is unset and applies
// SET-IF-ABSENT, so host-shell / Container-App overrides still win and local-dev
// (.env) is unchanged. Fail closed on a bad envelope: minting a credential on a
// half-applied config is worse than not minting. The /.env layer stays
// best-effort (it is absent in prod, where this call is a no-op).
func applySubcommandEnv(prefix string) error {
	if res, err := genesis.AutoloadFromEnv(); err != nil {
		return fmt.Errorf("%s: genesis envelope auto-load failed: %w", prefix, err)
	} else if res.Enabled {
		fmt.Fprintf(os.Stderr, "%s: genesis envelope auto-loaded (source=%s applied=%d skipped=%d)\n",
			prefix, res.Source, len(res.Applied), len(res.Skipped))
	}
	if _, err := genesis.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "%s: local .env override failed: %v\n", prefix, err)
	}
	return nil
}
