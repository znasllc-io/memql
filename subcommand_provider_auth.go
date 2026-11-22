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

// The exit codes. `scripts/install/verify-provider-key.sh` branches on them, so
// each means one thing and 0 means exactly one: VERIFIED. It used to also be
// what a vendor nobody had set up exited with, and the installer read that as
// "federation verified" (design record 2026-11-21, A5; the review of part 1).
const (
	// providerAuthCheckExitFail: refused. By the vendor (the sign-in), by the
	// vendor's API (the bearer), or by the engine itself (ids it will not use).
	providerAuthCheckExitFail = 1
	// providerAuthCheckExitNotSetUp: the engine holds no account at this vendor
	// and says so: no federation ids on this node, or a node type that signs in
	// to none. An answer, not a fault, and not "verified".
	providerAuthCheckExitNotSetUp = 3
	// providerAuthCheckExitNotVerified: the check cannot say yet. Nothing is
	// recorded, the engine is waiting for its next identity token, the vendor
	// never answered, or nothing has used the bearer. The installer reads it as
	// "says nothing about the credential", which is what this is.
	providerAuthCheckExitNotVerified = 4
)

// providerAuthExitCode maps CheckProviderAuth's error to the exit code.
func providerAuthExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, memql.ErrVendorNotSetUp):
		return providerAuthCheckExitNotSetUp
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
  0  verified: the vendor accepted the engine's sign-in and its API has taken
     the bearer
  1  refused: by the vendor, by its API, or by the engine itself (ids it will
     not use). The reason is on stderr
  2  bad usage
  3  not set up: no federation ids on this node, or a node type that signs in
     to no vendor
  4  cannot say yet: nothing is recorded, the engine is waiting for its next
     identity token, the vendor did not answer, or nothing has used the bearer.
     Run it again

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
	return writeProviderAuthVerdict(os.Stderr, report, err)
}

// writeProviderAuthVerdict prints the last line of the check and returns the
// exit code. Each answer has words of its own, and only a refusal names the
// runbook: the other three are not something a runbook step fixes.
func writeProviderAuthVerdict(w io.Writer, report memql.ProviderAuthReport, err error) int {
	code := providerAuthExitCode(err)
	switch code {
	case 0:
		fmt.Fprintln(w, "\nprovider-auth check: OK")
	case providerAuthCheckExitNotSetUp:
		fmt.Fprintf(w, "\nprovider-auth check: NOT SET UP: %v\n", err)
	case providerAuthCheckExitNotVerified:
		fmt.Fprintf(w, "\nprovider-auth check: NOT VERIFIED YET: %v\n", err)
	default:
		fmt.Fprintf(w, "\nprovider-auth check: FAILED: %v\n", err)
		runbook := "docs/public/operate/auth/anthropic-federation.md"
		if report.Vendor == "openai" {
			runbook = "docs/public/operate/auth/openai-federation.md"
		}
		fmt.Fprintln(w, "Runbook: "+runbook)
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
			expires := r.TokenExpiresAt.UTC().Format(time.RFC3339)
			if r.TokenExpiresIn > 0 {
				line("tokenExpires", fmt.Sprintf("%s (in %s)", expires, r.TokenExpiresIn.Round(time.Second)))
			} else {
				// A node with no traffic lets its bearer run out: a renewal is
				// made when a call needs one, never on a timer.
				line("tokenExpires", expires+" (expired; the engine signs in again when a call needs it)")
			}
		}
		if r.ExchangeOutcome == memql.SignInOutcomeMisconfigured {
			// The engine's own refusal, made before any vendor was asked.
			line("engineSaid", r.Detail)
		} else {
			line("vendorSaid", r.Detail)
		}
		if r.MayBeReplay {
			line("note", "the identity token was issued before this engine process started, so the refusal "+
				"may only mean an earlier process in this pod already used it")
		}
		if !r.NextTokenDue.IsZero() {
			line("nextTokenDue", r.NextTokenDue.UTC().Format(time.RFC3339))
		}
		if r.ExchangeOutcome != memql.SignInOutcomeOK {
			break
		}
		// AN ACCEPTED SIGN-IN IS A BEARER. Whether the vendor's API takes it is
		// a second answer, and the one that makes the vendor usable.
		switch r.Proof {
		case memql.SignInProofOK:
			shown := ""
			if !r.ProofAt.IsZero() {
				shown = ", last shown " + r.ProofAt.UTC().Format(time.RFC3339)
			}
			line("api", "took the bearer"+shown)
		case memql.SignInProofRefused:
			line("api", fmt.Sprintf("refused the bearer (status %d): %s", r.ProofStatus, r.ProofDetail))
		case memql.SignInProofError:
			line("api", "gave no answer: "+r.ProofDetail)
		default:
			line("api", "nothing has used the bearer yet")
		}
		if !r.BearerDroppedAt.IsZero() {
			line("bearerDropped", r.BearerDroppedAt.UTC().Format(time.RFC3339)+
				" (the vendor's API answered 401 for it; the engine signs in again with the next identity token)")
		}
	case "unavailable":
		switch {
		case !r.Recorded:
			// NOTHING IS CLAIMED. Ids applied through Settings are in the
			// cluster's storage, which only the engine reads.
			line("note", "this process sees no federation ids in its environment, and the engine in this pod "+
				"has recorded nothing, so this command cannot say whether the vendor is set up: ids applied "+
				"through Settings are the engine's to see.")
		case r.ExchangeOutcome == memql.SignInOutcomeNotUsed:
			line("note", r.Detail+". Run the check on a node that does, for example deploy/agent.")
		default:
			line("note", "this node has NO credential for this vendor: federation is not configured. "+
				"That is the normal state of a cluster nobody has set a vendor up on, local or cloud. "+
				"The federation runbooks have the steps for both. The cluster has no API key to fall back to.")
		}
	}

	if r.ModelsListed > 0 {
		line("models.list", fmt.Sprintf("%d models returned to the engine", r.ModelsListed))
	}
	// Said every time, because the report otherwise reads as though this
	// command had asked the vendor itself.
	if r.Recorded {
		switch r.ExchangeOutcome {
		case memql.SignInOutcomeNotConfigured, memql.SignInOutcomeNotUsed, memql.SignInOutcomeMisconfigured:
			// The engine made no sign-in, so there is none to describe.
			fmt.Fprintln(w, "\nThis is what the engine in this pod recorded about the vendor, which includes ids")
			fmt.Fprintln(w, "applied through Settings. This command contacts no vendor.")
		default:
			fmt.Fprintln(w, "\nThis is the engine's own sign-in in this pod, as the engine recorded it. The ids")
			fmt.Fprintln(w, "are the ones the engine used, including any applied through Settings. This command")
			fmt.Fprintln(w, "contacts no vendor.")
		}
		return
	}
	fmt.Fprintln(w, "\nThe ids above are read from the process environment only; values seeded into")
	fmt.Fprintln(w, "concept storage (v1:platform:globalSecret / globalVariable) are not visible here")
	fmt.Fprintln(w, "until the engine has signed in with them. This command contacts no vendor.")
}
