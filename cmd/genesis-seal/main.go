// Command genesis-seal seals a plaintext .env into the encrypted
// ~/.memql/genesis.znas envelope that the local k3d cluster decrypts
// into k8s Secrets at `make up` (scripts/k3d/seed-secrets.sh).
//
// It is the headless equivalent of the cockpit's first-launch genesis
// wizard: parse the .env, validate it against the secrets manifest, and
// encrypt it under MEMQL_MASTER_KEY -- reused from the environment when
// present, generated (and printed, so you can save it) on first use.
//
// Usage:
//
//	make genesis-seal ENV_FILE=~/Downloads/local.genesis.env
//
// or directly:
//
//	go run ./cmd/genesis-seal --env-file=path/to/local.genesis.env
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/znasllc-io/memql/component/genesis"
)

// defaultOutPath mirrors memql's genesis resolution: $MEMQL_GENESIS_PATH
// wins, otherwise ~/.memql/genesis.znas.
func defaultOutPath() string {
	if p := os.Getenv("MEMQL_GENESIS_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".memql", "genesis.znas")
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}

func main() {
	envFile := flag.String("env-file", "", "path to the plaintext .env to seal (required)")
	out := flag.String("out", defaultOutPath(), "output path for the sealed genesis envelope")
	manifestPath := flag.String("manifest", "", "manifest.yaml path (default: resolved by LoadManifest -- MEMQL_MANIFEST_PATH / MEMQL_REPO / embedded)")
	syncShell := flag.Bool("sync-shell", true, "sync MEMQL_MASTER_KEY into ~/.bashrc and ~/.zshrc (matches the cockpit wizard)")
	syncSource := flag.Bool("sync-source", false, "rewrite the source .env's MEMQL_MASTER_KEY line to match the envelope")
	flag.Parse()

	if *envFile == "" {
		fmt.Fprintln(os.Stderr, "genesis-seal: --env-file is required")
		flag.Usage()
		os.Exit(2)
	}

	entries, err := genesis.ParseEnvFile(*envFile)
	if err != nil {
		fail("parse env file: %v", err)
	}
	manifest, err := genesis.LoadManifest(*manifestPath)
	if err != nil {
		fail("load manifest: %v", err)
	}

	sourceEnvPath := ""
	if *syncSource {
		sourceEnvPath = *envFile
	}

	res, err := genesis.Seal(genesis.SealOptions{
		Entries:       entries,
		Manifest:      manifest,
		OutPath:       *out,
		SourceEnvPath: sourceEnvPath,
		SyncShellRCs:  *syncShell,
		// nil confirm => auto-confirm a freshly generated key. We print
		// the key below so the operator can save it.
		ConfirmMasterKey: nil,
	})
	if err != nil {
		fail("seal: %v", err)
	}

	fmt.Printf("genesis-seal: wrote %s (%d entries, manifest: %s)\n", *out, res.EntriesWritten, res.ManifestSource)
	switch {
	case !res.FirstTime:
		fmt.Println("genesis-seal: updated the existing envelope using MEMQL_MASTER_KEY from your environment.")
	case res.ReusedKeyFromEnv:
		fmt.Println("genesis-seal: sealed under the MEMQL_MASTER_KEY already in your environment (no new key).")
	default:
		// Freshly generated key -- the operator MUST save it or they
		// can never decrypt the envelope again.
		fmt.Println()
		fmt.Println("  A NEW MEMQL_MASTER_KEY was generated -- SAVE THIS NOW (e.g. password manager):")
		fmt.Printf("    export MEMQL_MASTER_KEY=%s\n", res.MasterKey)
		if *syncShell {
			fmt.Println("  (it was also synced into your shell rc files).")
		}
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genesis-seal: "+format+"\n", args...)
	os.Exit(1)
}
