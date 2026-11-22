package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// `memql provider-auth check` -- report, from inside a pod, which credential
// this node is really using to reach a cloud vendor and whether the vendor
// accepted it (memql#4335, extended to both vendors by memql#5088).
//
// IT REPORTS THE ENGINE'S OWN SIGN-IN AND MAKES NONE (design record
// 2026-11-21-vendor-accounts-and-api-keys, A5). The engine in the pod signs in
// at start and records what happened; this command reads that record. It used
// to sign in itself, as a second process, and a vendor that accepts an identity
// token once refuses whichever of two signers goes second.
//
// --provider takes `anthropic` or `openai` (the vendor) or one DSL provider
// name. It is the verification step of either federation runbook
// (docs/public/operate/auth/{anthropic,openai}-federation.md), run after the
// overlay carries the ids to confirm the exchange works.
// `scripts/install/verify-provider-key.sh` calls it through `kubectl exec` for
// the same reason.
//
// AVAILABLE ON EVERY NODE BINARY, unlike the credential-minting subcommands
// beside it. The identity node is the authority for MemQL's own credentials;
// this one asks about a VENDOR credential that every node holds its own copy
// of, and the answer can differ per node -- that is exactly what makes it
// worth running per node.

const (
	// providerAuthCheckExitFail: the vendor refused the engine's sign-in.
	// `scripts/install/verify-provider-key.sh` reads exactly this code as a
	// refusal, so nothing else may use it.
	providerAuthCheckExitFail = 1
	// providerAuthCheckExitNotVerified: the check cannot say. Nothing is
	// recorded yet, the engine is waiting for its next identity token, or the
	// vendor never answered. The installer reads any code but 0 and 1 as "says
	// nothing about the credential", which is what this is.
	providerAuthCheckExitNotVerified = 4
)

// providerAuthExitCode maps CheckProviderAuth's error to the exit code.
func providerAuthExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, memql.ErrSignInNotVerified):
		return providerAuthCheckExitNotVerified
	default:
		return providerAuthCheckExitFail
	}
}

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
  check   Report which credential this node uses for an AI provider and what
          the vendor said to the engine's own sign-in. The engine in this pod
          signs in once at start and lists the vendor's models; this command
          reads its record and contacts no vendor.

Example (from an operator machine, against a running cluster):
  kubectl exec -n memql deploy/agent -- /app/memql provider-auth check

Exit codes:
  0  the vendor accepted the engine's sign-in
  1  the vendor refused it; the vendor's reason is on stderr
  2  bad usage
  4  cannot say yet: nothing is recorded, the engine is waiting for its next
     identity token, or the vendor did not answer. Run it again

Run "memql provider-auth check --help" for flags.`)
}

func runProviderAuthCheck(args []string) int {
	fs := flag.NewFlagSet("provider-auth check", flag.ContinueOnError)
	provider := fs.String("provider", "anthropic",
		"vendor or DSL provider name to check. `anthropic` or `openai` (the vendor) picks the first "+
			"available provider of that vendor; a DSL provider name (e.g. streamClaudeSonnet, "+
			"chat54Mini) checks that one. The default stays `anthropic` so existing runbook steps "+
			"and CI invocations keep meaning what they meant.")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to wait for the engine's sign-in to be recorded")
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
	code := providerAuthExitCode(err)
	switch code {
	case 0:
		fmt.Fprintln(os.Stderr, "\nprovider-auth check: OK")
	case providerAuthCheckExitNotVerified:
		fmt.Fprintf(os.Stderr, "\nprovider-auth check: NOT VERIFIED YET: %v\n", err)
	default:
		fmt.Fprintf(os.Stderr, "\nprovider-auth check: FAILED: %v\n", err)
		runbook := "docs/public/operate/auth/anthropic-federation.md"
		if report.Vendor == "openai" {
			runbook = "docs/public/operate/auth/openai-federation.md"
		}
		fmt.Fprintln(os.Stderr, "Runbook: "+runbook)
	}
	return code
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
	line("vendor", r.Vendor)
	line("type", r.Type)
	line("model", r.Model)
	line("credential", r.CredentialPath)

	switch r.CredentialPath {
	case "federation":
		// Each vendor fills only its own ids, and `line` drops an empty value,
		// so this prints one vendor's set without a switch. The workspace id
		// is the exception: it is Anthropic's and OPTIONAL, so its absence is
		// worth saying out loud rather than leaving as a missing row an
		// operator has to notice.
		line("identityProviderId", r.IdentityProviderID)
		line("federationRuleId", r.FederationRuleID)
		line("organizationId", r.OrganizationID)
		line("serviceAccountId", r.ServiceAccountID)
		// Keyed on the ids the report CARRIES rather than on r.Vendor, which is
		// a separate field that can be unset on a hand-built report. The
		// workspace id belongs to Anthropic's id set, and a rule id is what
		// says this is one -- so the "(none)" explanation cannot go missing
		// because a caller forgot to stamp a vendor.
		if r.FederationRuleID != "" || r.OrganizationID != "" {
			if r.WorkspaceID == "" {
				line("workspaceId", "(none -- Anthropic picks the rule's workspace)")
			} else {
				line("workspaceId", r.WorkspaceID)
			}
		}
		line("identityTokenFile", r.IdentityTokenFile)
		// The subject is what the rule's subject_prefix matches on, so it is
		// the first thing to compare against the Console when a denial says
		// match_subject_prefix.
		line("tokenSubject", r.TokenSubject)
		line("tokenAudience", strings.Join(r.TokenAudience, ", "))
		if !r.Recorded {
			line("signIn", "nothing recorded by the engine in this pod")
			break
		}
		line("signIn", r.ExchangeOutcome)
		if !r.SignInAt.IsZero() {
			line("signInAt", r.SignInAt.UTC().Format(time.RFC3339))
		}
		if !r.TokenExpiresAt.IsZero() {
			line("tokenExpires", fmt.Sprintf("%s (in %s)",
				r.TokenExpiresAt.UTC().Format(time.RFC3339), r.TokenExpiresIn.Round(time.Second)))
		}
		line("vendorSaid", r.Detail)
		if r.MayBeReplay {
			line("note", "the identity token was issued before this engine process started, so the refusal "+
				"may only mean an earlier process in this pod already used it")
		}
		if !r.NextTokenDue.IsZero() {
			line("nextTokenDue", r.NextTokenDue.UTC().Format(time.RFC3339))
		}
	case "unavailable":
		line("note", "this node has NO credential for this vendor: federation is not configured. "+
			"That is the normal state of a cluster nobody has set a vendor up on, local or cloud. "+
			"The federation runbooks have the steps for both. The cluster has no API key to fall back to.")
	}

	if r.ModelsListed > 0 {
		line("models.list", fmt.Sprintf("%d models returned to the engine", r.ModelsListed))
	}
	// Said every time, because the report otherwise reads as though this
	// command had asked the vendor itself.
	if r.Recorded {
		fmt.Fprintln(w, "\nThis is the engine's own sign-in in this pod, as the engine recorded it. The ids")
		fmt.Fprintln(w, "are the ones the engine used, including any applied through Settings. This command")
		fmt.Fprintln(w, "contacts no vendor.")
		return
	}
	fmt.Fprintln(w, "\nThe ids above are read from the process environment only; values seeded into")
	fmt.Fprintln(w, "concept storage (v1:platform:globalSecret / globalVariable) are not visible here")
	fmt.Fprintln(w, "until the engine has signed in with them. This command contacts no vendor.")
}
