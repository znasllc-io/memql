package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/pat"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// runPATSubcommand routes the `memql pat` operator subcommands. PATs are random
// bearer tokens (mql_pat_<base64url>), NOT signed JWTs, so minting needs only
// the engine + database -- no identity issuer. That is why this subcommand is
// UNtagged (like `migrate`) and routed from both the identity and non-identity
// dispatch tables: it must work wherever the operator can exec the binary,
// including the identity pod (#677).
func runPATSubcommand(args []string) int {
	if len(args) == 0 {
		printPATUsage()
		return 2
	}
	switch args[0] {
	case "mint":
		return runPATMint(args[1:])
	case "list":
		return runPATList(args[1:])
	case "revoke":
		return runPATRevoke(args[1:])
	case "-h", "--help", "help":
		printPATUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "pat: unknown subcommand %q\n", args[0])
	printPATUsage()
	return 2
}

func printPATUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql pat <subcommand> [flags]

Subcommands:
  mint    Mint a personal access token (mql_pat_...) owned by a user and print it once.
  list    List PAT identity rows (no key material) for a user or the whole cluster.
  revoke  Revoke a PAT identity row by id (flips active=false).

Requires the database environment (MEMORY_NODES_DATABASE_DSN). Typically run via
  kubectl exec -n memql deploy/memql-identity -- /app/memql pat mint --user-id <id>
against a cluster whose schema is already migrated.

Run "memql pat <subcommand> --help" for subcommand-specific flags.`)
}

// runPATMint mints a PAT for an existing user and prints the plaintext once.
func runPATMint(args []string) int {
	fs := flag.NewFlagSet("pat mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	userId := fs.String("user-id", "",
		"v1:identity:user.id the PAT is owned by (required). Authorization uses this user's role/grants, so pick a user with the access the token needs.")
	label := fs.String("label", "",
		"Human label for the credential row (defaults to \"pat-cli\").")
	out := fs.String("out", "",
		"Write the token to this file (mode 0600) instead of stdout.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*userId) == "" {
		fmt.Fprintln(os.Stderr, "pat mint: --user-id is required")
		fs.Usage()
		return 2
	}
	resolvedLabel := strings.TrimSpace(*label)
	if resolvedLabel == "" {
		resolvedLabel = "pat-cli"
	}

	// Redirect stdout to stderr BEFORE bootstrap (#686): the 15+ component-
	// internal loggers (hardcoded to os.Stdout) are created during app.Build +
	// dependency Start inside bootstrapPATEngine, so the swap MUST precede it or
	// startup logs pollute the caller's `tok=$(kubectl exec ... pat mint)`
	// capture. The plaintext token is written through the saved real-stdout fd.
	// Same ordering as node-token / voice-agent-token mint.
	realStdout := redirectStdoutToStderr()
	defer restoreStdout(realStdout)

	deps, engine, logger, code := bootstrapPATEngine("pat mint")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	plain, keyHash, err := pat.Mint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pat mint: generate token: %v\n", err)
		return 1
	}
	identityId, err := pat.NewId()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pat mint: generate identity id: %v\n", err)
		return 1
	}

	store := &pat.Store{Engine: engine, Logger: logger}
	// The CLI mint is unauthenticated by design (operator exec), so stamp the
	// system actor so the engine's per-row authz gate accepts the write.
	ctx, cancel := context.WithTimeout(identity.ContextWithSystemActor(context.Background()), 30*time.Second)
	defer cancel()
	if err := store.Create(ctx, identityId, *userId, resolvedLabel, keyHash); err != nil {
		fmt.Fprintf(os.Stderr, "pat mint: persist identity row: %v\n", err)
		return 1
	}

	canonical := pat.CanonicalId(identityId)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(plain+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "pat mint: write --out: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "pat mint: wrote token to %s (mode 0600); id=%s user=%s label=%q\n",
			*out, canonical, *userId, resolvedLabel)
	} else {
		fmt.Fprintln(realStdout, plain)
		fmt.Fprintf(os.Stderr, "pat mint: minted id=%s user=%s label=%q (revoke later: memql pat revoke --id %s)\n",
			canonical, *userId, resolvedLabel, canonical)
	}
	return 0
}

// runPATList prints PAT rows (no key material) for a user or the whole cluster.
func runPATList(args []string) int {
	fs := flag.NewFlagSet("pat list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	userId := fs.String("user-id", "",
		"Restrict to PATs owned by this v1:identity:user.id. Empty lists all PATs.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	deps, engine, logger, code := bootstrapPATEngine("pat list")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	store := &pat.Store{Engine: engine, Logger: logger}
	ctx, cancel := context.WithTimeout(identity.ContextWithSystemActor(context.Background()), 30*time.Second)
	defer cancel()

	var rows []pat.PATRow
	var err error
	if strings.TrimSpace(*userId) == "" {
		rows, err = store.ListAll(ctx)
	} else {
		rows, err = store.ListForUser(ctx, *userId)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "pat list: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "%-40s  %-24s  %-7s  %s\n", "ID", "USER", "ACTIVE", "LABEL")
	for _, r := range rows {
		fmt.Fprintf(os.Stdout, "%-40s  %-24s  %-7t  %s\n", r.ID, r.UserId, r.Active, r.Label)
	}
	fmt.Fprintf(os.Stderr, "pat list: %d row(s)\n", len(rows))
	return 0
}

// runPATRevoke flips a PAT row's active flag to false.
func runPATRevoke(args []string) int {
	fs := flag.NewFlagSet("pat revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "",
		"Canonical or bare PAT identity id to revoke (required, e.g. v1:identity:identity:pat-... or pat-...).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*id) == "" {
		fmt.Fprintln(os.Stderr, "pat revoke: --id is required")
		fs.Usage()
		return 2
	}

	deps, engine, logger, code := bootstrapPATEngine("pat revoke")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	store := &pat.Store{Engine: engine, Logger: logger}
	ctx, cancel := context.WithTimeout(identity.ContextWithSystemActor(context.Background()), 30*time.Second)
	defer cancel()
	if err := store.Revoke(ctx, pat.CanonicalId(strings.TrimSpace(*id))); err != nil {
		fmt.Fprintf(os.Stderr, "pat revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "pat revoke: revoked %s\n", pat.CanonicalId(strings.TrimSpace(*id)))
	return 0
}

// bootstrapPATEngine builds the app, applies the local .env overlay, and starts
// the dependencies up to AND INCLUDING the engine (config -> database ->
// memQLEngine), then STOPS -- it does NOT start the identity service,
// automations, or the transport servers that come after the engine in the
// bootstrap order. Returns the started deps, the engine, the CLI logger, and a
// zero exit code on success.
//
// Stopping after the engine (#686, mirroring the migrate subcommand's
// stop-after-database) matters because `pat mint` only needs the engine +
// database; starting the identity service would fatal-validate
// (IDENTITY_KEY_ENCRYPTION_KEY required ...) and abort the mint on the identity
// binary -- the only binary the `pat` subcommand is dispatched on.
func bootstrapPATEngine(prefix string) ([]common.Dependency, *memql.MemQLEngine, *slog.Logger, int) {
	// Mirror the server bootstrap: decrypt the genesis envelope (sealed DSN /
	// signing key) then overlay /.env, so the mint sees the same config the
	// running identity does (#751 -- subcommands run before main()'s autoload).
	if err := applySubcommandEnv(prefix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, nil, 1
	}

	logger := mustCreateCLILogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	selected, ok := depsUpToEngine(application.Dependencies)
	if !ok || application.Engine() == nil {
		fmt.Fprintf(os.Stderr, "%s: engine dependency not present in this build\n", prefix)
		return nil, nil, logger, 1
	}
	deps := make([]common.Dependency, 0, len(selected))
	for _, d := range selected {
		d.Start(context.Background())
		deps = append(deps, d)
	}
	return deps, application.Engine(), logger, 0
}

// depsUpToEngine returns the prefix of the bootstrap dependency list up to AND
// INCLUDING the memQLEngine (config -> database -> engine + the supporting
// components ordered before it), plus whether the engine was found. pat mint
// starts ONLY these -- it must NOT start the identity service / automations /
// transport servers that follow the engine in the order (#686), since the
// identity service fatal-validates and would abort the mint. Pure (no side
// effects) so it is unit-tested without a database.
func depsUpToEngine(all []common.Dependency) ([]common.Dependency, bool) {
	for i, d := range all {
		if d.ComponentName() == memql.ComponentName {
			return all[:i+1], true
		}
	}
	return all, false
}

func stopPATDependencies(deps []common.Dependency) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), app.DefaultRunShutdownTimeout)
	defer cancel()
	for i := len(deps) - 1; i >= 0; i-- {
		deps[i].Stop(shutdownCtx)
	}
}

// redirectStdoutToStderr swaps the global os.Stdout to os.Stderr so component-
// internal loggers don't pollute a caller's token capture, returning the saved
// real stdout to write the token through. Pair with restoreStdout via defer.
func redirectStdoutToStderr() *os.File {
	real := os.Stdout
	os.Stdout = os.Stderr
	return real
}

func restoreStdout(real *os.File) { os.Stdout = real }
