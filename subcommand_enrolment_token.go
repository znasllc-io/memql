package main

// `memql enrolment-token mint` -- the operator/installer path to a single-use
// enrolment link (memql#3408).
//
// WHY A SUBCOMMAND AND NOT A CALL. The install wizard's problem is that at the
// moment it needs an enrolment link, NOTHING can authenticate to the cluster
// yet: the owner has just been bootstrapped from env and holds no credential,
// which is the whole reason enrolment exists. So the admin issuer on
// IdentityAdminMsg -- which requires an authenticated owner/admin -- is not
// reachable from the installer, by construction. What IS available is the same
// authority `memql pat mint` and `memql voice-agent-token mint` rely on:
// somebody who can exec inside the identity pod already holds the cluster's
// secrets. Access to the process is the authorization.
//
// UNTAGGED, like `pat`, for the same reason: an enrolment token is a random
// bearer rather than a signed JWT, so minting needs the engine + database and
// nothing from the identity issuer. That lets it run wherever an operator can
// exec the binary.
//
// The plaintext link is written to the REAL stdout and is never persisted or
// logged -- only its SHA-256 hash reaches the row. Everything else, component
// startup logs included, goes to stderr so a caller's `$(kubectl exec ...)`
// capture holds the link and only the link. Same discipline as `pat mint`.

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/enrolment"
)

func runEnrolmentTokenSubcommand(args []string) int {
	if len(args) == 0 {
		printEnrolmentTokenUsage()
		return 2
	}
	switch args[0] {
	case "mint":
		return runEnrolmentTokenMint(args[1:])
	case "-h", "--help", "help":
		printEnrolmentTokenUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "enrolment-token: unknown subcommand %q\n", args[0])
	printEnrolmentTokenUsage()
	return 2
}

func printEnrolmentTokenUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql enrolment-token <subcommand> [flags]

Subcommands:
  mint    Mint a single-use enrolment link for a user and print it once.

An enrolment link authorizes exactly one action -- register a passkey as the
named user -- so a person with no credential and no mailbox can obtain their
first one. It is single-use and short-lived (15 minutes by default).

Requires the database environment (MEMQL_DATABASE_DSN) and the public identity
URL (MEMQL_IDENTITY_BASE_URL, or --base-url). Typically run via
  kubectl exec -n memql deploy/identity -- /app/memql enrolment-token mint --user-email owner@example.com

Run "memql enrolment-token mint --help" for flags.`)
}

func runEnrolmentTokenMint(args []string) int {
	fs := flag.NewFlagSet("enrolment-token mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	userId := fs.String("user-id", "",
		"v1:identity:user.id to enrol. One of --user-id / --user-email is required.")
	userEmail := fs.String("user-email", "",
		"Primary email of the user to enrol. Resolved to a user id before minting.")
	baseURL := fs.String("base-url", "",
		"Public identity base URL the link points at (defaults to MEMQL_IDENTITY_BASE_URL).")
	ttl := fs.Duration("ttl", enrolment.DefaultTTL,
		"Lifetime of the link. Clamped to the server ceiling (24h).")
	insecure := fs.Bool("allow-insecure", false,
		"Permit an http:// base URL. LOCAL DEVELOPMENT ONLY -- the link carries a credential.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*userId) == "" && strings.TrimSpace(*userEmail) == "" {
		fmt.Fprintln(os.Stderr, "enrolment-token mint: one of --user-id / --user-email is required")
		fs.Usage()
		return 2
	}

	base := strings.TrimRight(strings.TrimSpace(*baseURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(os.Getenv("MEMQL_IDENTITY_BASE_URL")), "/")
	}
	if base == "" {
		fmt.Fprintln(os.Stderr, "enrolment-token mint: no identity base URL -- pass --base-url or set MEMQL_IDENTITY_BASE_URL")
		return 2
	}
	if reason := checkEnrolmentBaseURL(base, *insecure); reason != "" {
		fmt.Fprintln(os.Stderr, "enrolment-token mint: "+reason)
		return 2
	}

	// Redirect stdout to stderr BEFORE bootstrap, for the reason `pat mint`
	// documents: the component loggers are created during app.Build and would
	// otherwise pollute a caller's command-substitution capture. The link is
	// written through the saved real-stdout fd.
	realStdout := redirectStdoutToStderr()
	defer restoreStdout(realStdout)

	deps, engine, logger, code := bootstrapPATEngine("enrolment-token mint")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	// The CLI mint is unauthenticated by design (operator exec), so stamp the
	// system actor so the engine's per-row authz gate accepts the write.
	ctx, cancel := context.WithTimeout(identity.ContextWithSystemActor(context.Background()), 30*time.Second)
	defer cancel()

	store := &identity.Store{Engine: engine, Logger: logger}
	resolvedUserId := strings.TrimSpace(*userId)
	if resolvedUserId == "" {
		row, err := store.LookupUserByEmail(ctx, strings.TrimSpace(*userEmail))
		if err != nil {
			fmt.Fprintf(os.Stderr, "enrolment-token mint: look up user by email: %v\n", err)
			return 1
		}
		if row == nil {
			// Refused rather than created. This subcommand mints a credential
			// for an EXISTING account; inventing the account here would give
			// the operator a second, unaudited user-creation path.
			fmt.Fprintf(os.Stderr, "enrolment-token mint: no user with primary email %q\n", strings.TrimSpace(*userEmail))
			return 1
		}
		resolvedUserId = row.ID
	}

	plain, hash, err := enrolment.Mint()
	if err != nil {
		fmt.Fprintf(os.Stderr, "enrolment-token mint: generate token: %v\n", err)
		return 1
	}
	enrolmentId, err := enrolment.NewId()
	if err != nil {
		fmt.Fprintf(os.Stderr, "enrolment-token mint: generate id: %v\n", err)
		return 1
	}

	lifetime := enrolment.ClampTTL(*ttl)
	expiresAt := time.Now().UTC().Add(lifetime)
	enrolStore := &enrolment.Store{Engine: engine, Logger: logger}
	// issuedBy is the user being enrolled: an operator exec has no user
	// identity of its own, and attributing the issue to a fabricated principal
	// would be worse than attributing it to the account it acts on. The audit
	// distinction that matters -- "this came from inside the pod" -- is
	// recorded by sourceIP being empty here and present on every portal issue.
	if err := enrolStore.Create(ctx, enrolmentId, resolvedUserId, hash, resolvedUserId, expiresAt, ""); err != nil {
		fmt.Fprintf(os.Stderr, "enrolment-token mint: persist enrolment row: %v\n", err)
		return 1
	}

	fmt.Fprintln(realStdout, base+"/enroll?code="+url.QueryEscape(plain))
	fmt.Fprintf(os.Stderr, "enrolment-token mint: minted id=%s user=%s expires=%s (single-use)\n",
		enrolment.CanonicalId(enrolmentId), resolvedUserId, expiresAt.Format(time.RFC3339))
	return 0
}

// checkEnrolmentBaseURL enforces HTTPS on issue. Returns "" when the URL is
// usable, or the operator-facing reason it is not.
//
// The check is on the ARTIFACT, not on a request: this path has no
// http.Request to inspect, so "HTTPS required on issue" means a plaintext
// enrolment URL is never emitted in the first place. --allow-insecure is the
// local-development escape hatch and says what it costs.
func checkEnrolmentBaseURL(base string, allowInsecure bool) string {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return "base URL is not a usable URL: " + base
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	if allowInsecure {
		fmt.Fprintln(os.Stderr, "enrolment-token mint: WARNING: emitting a plaintext (http) enrolment link because --allow-insecure was passed; the link carries a credential")
		return ""
	}
	return "enrolment links must be https (the link carries a credential); got " + base +
		" -- pass --allow-insecure for local development"
}
