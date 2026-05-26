//go:build identity

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/core/common"
)

// dispatchSubcommand routes operator subcommands available on the
// identity binary. Returns (true, exitCode) when the first arg names
// a known subcommand; otherwise (false, 0) so main() falls through
// to the normal server bootstrap.
func dispatchSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "voice-agent-token":
		return true, runVoiceAgentTokenSubcommand(args[1:])
	case "node-token":
		return true, runNodeTokenSubcommand(args[1:])
	}
	return false, 0
}

func runVoiceAgentTokenSubcommand(args []string) int {
	if len(args) == 0 {
		printVoiceAgentTokenUsage()
		return 2
	}
	switch args[0] {
	case "mint":
		return runVoiceAgentTokenMint(args[1:])
	case "-h", "--help", "help":
		printVoiceAgentTokenUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "voice-agent-token: unknown subcommand %q\n", args[0])
	printVoiceAgentTokenUsage()
	return 2
}

func printVoiceAgentTokenUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql voice-agent-token <subcommand> [flags]

Subcommands:
  mint    Mint a class="voice_agent" JWT for a Python voice-agent process.

Run "memql voice-agent-token mint --help" for mint-specific flags.`)
}

// runVoiceAgentTokenMint bootstraps the identity stack (config + db +
// engine + identity service), mints a v1:identity:identity row of
// identityType="voice_agent_token", signs a class="voice_agent" JWT
// against the identity service's Ed25519 key, and prints the JWT
// once to stdout (or writes it to --out).
//
// The auxiliary `mql_va_<base64>` bearer whose SHA-256 hash lands on
// the identity row is NEVER returned -- the JWT is the credential
// the voice-agent uses for `Authorization: Bearer <jwt>`. The
// hash exists to satisfy the schema's keyHash @required contract and
// gives operators a stable fingerprint for audit correlation.
func runVoiceAgentTokenMint(args []string) int {
	fs := flag.NewFlagSet("voice-agent-token mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	instanceId := fs.String("instance-id", "",
		"Voice-agent instance identifier (required, e.g. voice-agent-local).")
	ttl := fs.Duration("ttl", 0,
		"Token lifetime (e.g. 90d=2160h, 24h, 30m). Zero falls back to DefaultVoiceAgentTokenTTLSeconds.")
	out := fs.String("out", "",
		"Write the bearer to this file (mode 0600) instead of stdout.")
	mintedBy := fs.String("minted-by", "system:voice-agent-token-cli",
		"v1:identity:user.id of the admin running the mint. Stamped on the credential row for audit attribution.")
	userId := fs.String("user-id", "system:voice-agent-token-cli",
		"v1:identity:user.id the credential row is owned by. Voice-agent tokens are owned by a system principal; override only if your cluster has a dedicated voice-agent operator user.")
	label := fs.String("label", "",
		"Human label for the credential row (defaults to instance-id).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*instanceId) == "" {
		fmt.Fprintln(os.Stderr, "voice-agent-token mint: --instance-id is required")
		fs.Usage()
		return 2
	}

	// Apply the same local /.env override the server bootstrap does
	// so the CLI sees the same IDENTITY_KEY_DIR / DSN as the running
	// identity service (the operator typically execs into the
	// container, where /.env is absent and this is a no-op).
	if _, err := genesis.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: local .env override failed: %v\n", err)
		// Non-fatal -- envelope/container env may already cover everything.
	}

	logger := mustCreateServiceLogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	// Start ONLY the dependencies the mint touches (database +
	// engine + identity service). Skipping transport keeps the CLI
	// from binding to ports the live identity container already
	// owns -- the subcommand is typically `docker exec`'d into the
	// running container.
	deps := selectMintDependencies(application.Dependencies)
	for _, d := range deps {
		d.Start(context.Background())
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), dependencyShutdownTimeout)
		defer cancel()
		// Stop in reverse start order; the order is small and
		// shutdown is best-effort, so a simple reverse loop is fine.
		for i := len(deps) - 1; i >= 0; i-- {
			deps[i].Stop(shutdownCtx)
		}
	}()

	svc, ok := application.IdentityService().(*identity.Service)
	if !ok || svc == nil {
		fmt.Fprintln(os.Stderr, "voice-agent-token mint: identity service not wired (binary missing -tags identity?)")
		return 1
	}

	identityId, err := newVoiceAgentIdentityId()
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: generate identity id: %v\n", err)
		return 1
	}
	auxBearer, keyHash, err := newAuxBearerAndHash()
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: generate aux bearer: %v\n", err)
		return 1
	}
	_ = auxBearer // never printed; the JWT is what callers consume.

	now := time.Now().UTC()
	resolvedTTL := *ttl
	if resolvedTTL <= 0 {
		resolvedTTL = time.Duration(identity.DefaultVoiceAgentTokenTTLSeconds) * time.Second
	}
	expiresAt := now.Add(resolvedTTL).Format(time.RFC3339Nano)

	store := &identity.Store{Engine: application.Engine(), Logger: logger}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.CreateIdentityVoiceAgentToken(
		ctx,
		identityId,
		*userId,
		*instanceId,
		keyHash,
		*mintedBy,
		expiresAt,
		*label,
	); err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: persist identity row: %v\n", err)
		return 1
	}

	bearer, jwtExpiresAt, err := svc.Issuer().IssueVoiceAgentAccessToken(
		identity.VoiceAgentIssueInput{
			IdentityId:  identityId,
			InstanceId:  *instanceId,
			TTLOverride: resolvedTTL,
		},
		now,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: sign JWT: %v\n", err)
		return 1
	}

	// Hand the bearer to the caller. --out lands at 0600 so a
	// distracted operator doesn't leave the token world-readable.
	if *out != "" {
		if err := os.WriteFile(*out, []byte(bearer+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "voice-agent-token mint: write --out: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: wrote bearer to %s (mode 0600); identity=%s instance=%s expires=%s\n",
			*out, identityId, *instanceId, jwtExpiresAt.Format(time.RFC3339))
	} else {
		// stdout = the bearer alone, no decoration, so the make
		// target / dev-refresh can capture it with $(...).
		fmt.Println(bearer)
		fmt.Fprintf(os.Stderr, "voice-agent-token mint: minted identity=%s instance=%s expires=%s\n",
			identityId, *instanceId, jwtExpiresAt.Format(time.RFC3339))
	}
	return 0
}

// selectMintDependencies returns the subset of bootstrap deps the
// mint subcommand actually needs (database + engine + identity
// service + the supporting components). Filters out the HTTP + gRPC
// transport servers so the CLI doesn't try to bind to the same
// ports as the running identity container -- the subcommand is
// typically `docker exec`'d into it.
func selectMintDependencies(all []common.Dependency) []common.Dependency {
	keep := []common.Dependency{}
	for _, d := range all {
		name := strings.ToLower(string(d.ComponentName()))
		// Skip anything whose name implies a network listener -- the
		// live container already owns those ports. The bootstrap
		// phases name servers consistently enough that a substring
		// match is reliable (grpcServer, httpServer, etc.).
		if strings.Contains(name, "server") {
			continue
		}
		keep = append(keep, d)
	}
	return keep
}

func newVoiceAgentIdentityId() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "v1:identity:identity:va_" + hex.EncodeToString(b), nil
}

func newAuxBearerAndHash() (bearer, keyHash string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return "", "", err
	}
	bearer = "mql_va_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(sum[:]), nil
}
