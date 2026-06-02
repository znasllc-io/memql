//go:build identity

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/identity"
)

// runServiceAccountTokenSubcommand routes `memql service-account-token <sub>`.
// The class="service_account" credential (#691, deployment-v2 Phase 3) is the
// machine identity the in-cluster deploy gate (AnalysisTemplate) uses for its
// authenticated query. Unlike the voice-agent token it persists NO
// v1:identity:identity row: the verify path is DB-free (JWKS only) and the
// token is short-lived + rotatable, so revoke = expiry / signing-key rotation.
func runServiceAccountTokenSubcommand(args []string) int {
	if len(args) == 0 {
		printServiceAccountTokenUsage()
		return 2
	}
	switch args[0] {
	case "mint":
		return runServiceAccountTokenMint(args[1:])
	case "-h", "--help", "help":
		printServiceAccountTokenUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "service-account-token: unknown subcommand %q\n", args[0])
	printServiceAccountTokenUsage()
	return 2
}

func printServiceAccountTokenUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql service-account-token <subcommand> [flags]

Subcommands:
  mint    Mint a class="service_account" JWT for automation / the deploy gate.

Run "memql service-account-token mint --help" for mint-specific flags.`)
}

// runServiceAccountTokenMint bootstraps the identity stack (config + db +
// engine + identity service), signs a class="service_account" JWT against the
// identity service's Ed25519 key, and prints it once to stdout (or --out). No
// DB row is written.
func runServiceAccountTokenMint(args []string) int {
	fs := flag.NewFlagSet("service-account-token mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	subject := fs.String("subject", "system:deploy-gate",
		"JWT subject -- a stable machine-principal id stamped on the `sub` claim for audit.")
	label := fs.String("label", "",
		"Instance label for audit attribution (required, e.g. deploy-gate-staging).")
	ttl := fs.Duration("ttl", 0,
		"Token lifetime (e.g. 1h, 30m). Zero falls back to DefaultServiceAccountTokenTTLSeconds (1h).")
	out := fs.String("out", "",
		"Write the JWT to this file (mode 0600) instead of stdout.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*label) == "" {
		fmt.Fprintln(os.Stderr, "service-account-token mint: --label is required")
		fs.Usage()
		return 2
	}

	// Layer the genesis envelope (sealed signing key etc.) then /.env, exactly
	// as the server boot does -- subcommands run before main()'s autoload (#751).
	if err := applySubcommandEnv("service-account-token mint"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	// Keep stdout clean for the JWT (scripts capture it via $(...)); component
	// loggers hardcode os.Stdout, so swap it to stderr for the subcommand and
	// write the token through the saved fd. Mirrors node-/voice-agent-token.
	realStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = realStdout }()

	logger := mustCreateCLILogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	deps := selectMintDependencies(application.Dependencies)
	for _, d := range deps {
		d.Start(context.Background())
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), app.DefaultRunShutdownTimeout)
		defer cancel()
		for i := len(deps) - 1; i >= 0; i-- {
			deps[i].Stop(shutdownCtx)
		}
	}()

	svc, ok := application.IdentityService().(*identity.Service)
	if !ok || svc == nil {
		fmt.Fprintln(os.Stderr, "service-account-token mint: identity service not wired (binary missing -tags identity?)")
		return 1
	}

	now := time.Now().UTC()
	resolvedTTL := *ttl
	if resolvedTTL <= 0 {
		resolvedTTL = time.Duration(identity.DefaultServiceAccountTokenTTLSeconds) * time.Second
	}
	bearer, jwtExpiresAt, err := svc.Issuer().IssueServiceAccountAccessToken(
		identity.ServiceAccountIssueInput{
			Subject:     *subject,
			Label:       *label,
			TTLOverride: resolvedTTL,
		},
		now,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service-account-token mint: sign JWT: %v\n", err)
		return 1
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(bearer+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "service-account-token mint: write --out: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "service-account-token mint: wrote JWT to %s (mode 0600); sub=%s label=%s expires=%s\n",
			*out, *subject, *label, jwtExpiresAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(realStdout, bearer)
		fmt.Fprintf(os.Stderr, "service-account-token mint: minted sub=%s label=%s expires=%s\n",
			*subject, *label, jwtExpiresAt.Format(time.RFC3339))
	}
	return 0
}
