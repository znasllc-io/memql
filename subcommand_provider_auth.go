package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// `memql provider-auth check` -- prove, from inside a pod, which credential
// this node is really using to reach Anthropic and that the provider accepts
// it (memql#4335).
//
// It is step 4 AND step 5 of the federation cutover runbook
// (docs/public/operate/auth/anthropic-federation.md): once after the overlay
// carries the ids, to confirm the exchange works before the key is deleted,
// and once after the key is gone, to confirm nothing was silently leaning on
// it. `scripts/install/verify-provider-key.sh` calls it through `kubectl exec`
// for the same reason.
//
// AVAILABLE ON EVERY NODE BINARY, unlike the credential-minting subcommands
// beside it. The identity node is the authority for MemQL's own credentials;
// this one asks about a VENDOR credential that every node holds its own copy
// of, and the answer can differ per node -- that is exactly what makes it
// worth running per node.

const providerAuthCheckExitFail = 1

func runProviderAuthSubcommand(args []string) int {
	if len(args) == 0 {
		printProviderAuthUsage()
		return 2
	}
	switch args[0] {
	case "check":
		return runProviderAuthCheck(args[1:])
	case "-h", "--help", "help":
		printProviderAuthUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "provider-auth: unknown subcommand %q\n", args[0])
	printProviderAuthUsage()
	return 2
}

func printProviderAuthUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql provider-auth <subcommand> [flags]

Subcommands:
  check   Report which credential this node uses for an AI provider, and prove
          the provider accepts it with one live, token-free models.list call.

Example (from an operator machine, against a running cluster):
  kubectl exec -n memql deploy/agent -- /app/memql provider-auth check

Exit codes:
  0  the provider accepted the credential
  1  it did not (or the credential is misconfigured); the reason is on stderr
  2  bad usage

Run "memql provider-auth check --help" for flags.`)
}

func runProviderAuthCheck(args []string) int {
	fs := flag.NewFlagSet("provider-auth check", flag.ContinueOnError)
	provider := fs.String("provider", "anthropic",
		"vendor or DSL provider name to check. `anthropic` (the vendor) picks the first available "+
			"Anthropic provider; a DSL provider name (e.g. streamClaudeSonnet) checks that one. "+
			"OpenAI is not supported here -- it has no federation mechanism; verify its key with "+
			"scripts/install/verify-provider-key.sh.")
	timeout := fs.Duration("timeout", 30*time.Second, "bound on the live provider call")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if err := applySubcommandEnv("provider-auth check"); err != nil {
		fmt.Fprintf(os.Stderr, "provider-auth check: %v\n", err)
		return providerAuthCheckExitFail
	}

	// Silence the DSL loader below ERROR. It warns once per provider that
	// could not construct -- fifteen lines on a node with no OpenAI key --
	// and burying the four-line answer under them would defeat the point of a
	// command an operator runs mid-cutover. Nothing is lost: if the provider
	// this check SELECTED is the one that failed, its construction error is
	// what CheckProviderAuth returns, and it is printed in full below.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	report, err := memql.CheckProviderAuth(ctx, logger, *provider)
	writeProviderAuthReport(os.Stdout, report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nprovider-auth check: FAILED: %v\n", err)
		fmt.Fprintln(os.Stderr, "Runbook: docs/public/operate/auth/anthropic-federation.md")
		return providerAuthCheckExitFail
	}
	fmt.Fprintln(os.Stderr, "\nprovider-auth check: OK")
	return 0
}

// writeProviderAuthReport prints the report as aligned key/value lines.
//
// Plain text, not JSON: this is read by a person at a `kubectl exec` prompt in
// the middle of a cutover. The one machine consumer,
// verify-provider-key.sh, branches on the EXIT CODE, which is the part a
// script should depend on anyway.
func writeProviderAuthReport(w io.Writer, r memql.ProviderAuthReport) {
	line := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		fmt.Fprintf(w, "%-22s %s\n", k+":", v)
	}
	line("provider", r.Provider)
	line("type", r.Type)
	line("model", r.Model)
	line("credential", r.CredentialPath)

	switch r.CredentialPath {
	case "federation":
		line("federationRuleId", r.FederationRuleID)
		line("organizationId", r.OrganizationID)
		line("serviceAccountId", r.ServiceAccountID)
		if r.WorkspaceID == "" {
			line("workspaceId", "(none -- Anthropic picks the rule's workspace)")
		} else {
			line("workspaceId", r.WorkspaceID)
		}
		line("identityTokenFile", r.IdentityTokenFile)
		// The subject is what the rule's subject_prefix matches on, so it is
		// the first thing to compare against the Console when a denial says
		// match_subject_prefix.
		line("tokenSubject", r.TokenSubject)
		line("tokenAudience", strings.Join(r.TokenAudience, ", "))
		line("exchange", r.ExchangeOutcome)
		if !r.TokenExpiresAt.IsZero() {
			line("tokenExpires", fmt.Sprintf("%s (in %s)",
				r.TokenExpiresAt.UTC().Format(time.RFC3339), r.TokenExpiresIn.Round(time.Second)))
		}
	case "api-key":
		line("note", "this node authenticates with a long-lived API key ("+
			"MEMQL_AI_ANTHROPIC_API_KEY). In the cloud that is the pre-cutover state.")
	}

	if r.ModelsListed > 0 {
		line("models.list", fmt.Sprintf("%d models returned", r.ModelsListed))
	}
	// Said every time, because the report otherwise looks like it can see
	// everything the node can.
	fmt.Fprintln(w, "\nRead from the process environment only; values seeded into concept storage")
	fmt.Fprintln(w, "(v1:platform:globalSecret / globalVariable) are not visible to this check.")
}
