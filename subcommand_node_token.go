//go:build identity

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/identity"
)

// validNodeTokenTypes pins the node-type values the dev cluster's
// docker-compose stack uses today. Adding a new node-type binary
// also needs to extend this list so the operator CLI rejects
// typos at mint time rather than letting them slip into a runtime
// auth failure.
var validNodeTokenTypes = map[string]bool{
	"bff":       true,
	"voice":     true,
	"cognition": true,
	"agent":     true,
	"planner":   true,
	"workbench": true,
}

func runNodeTokenSubcommand(args []string) int {
	if len(args) == 0 {
		printNodeTokenUsage()
		return 2
	}
	switch args[0] {
	case "mint":
		return runNodeTokenMint(args[1:])
	case "-h", "--help", "help":
		printNodeTokenUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "node-token: unknown subcommand %q\n", args[0])
	printNodeTokenUsage()
	return 2
}

func printNodeTokenUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql node-token <subcommand> [flags]

Subcommands:
  mint    Mint a class="node" JWT for an inter-node `+"`NodeService.Stream`"+` peer call.

Run "memql node-token mint --help" for mint-specific flags.`)
}

// runNodeTokenMint bootstraps the identity stack (config + db +
// engine + identity service), signs a class="node" JWT against the
// identity service's Ed25519 key for the requested (node-id,
// node-type) pair, and prints the JWT once to stdout (or writes it
// to --out).
//
// Unlike voice-agent-token mint, this does NOT persist a
// v1:identity:identity row. The receiving NodeService interceptor
// validates the JWT via signature + class + node_id/node_type
// claims alone -- it never dereferences IdentityId against the
// database (see component/identity/verifier/verifier.go). Operators
// who want a credential audit row for a node token should
// additionally mint via the identity admin web app.
//
// memql#338 -- this subcommand is the missing piece the dev
// docker-compose stack needs to wire MEMQL_NODE_TOKEN per
// cluster-node service.
func runNodeTokenMint(args []string) int {
	fs := flag.NewFlagSet("node-token mint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	nodeId := fs.String("node-id", "",
		"Node id (required, e.g. bff-local). Stamped on the `node_id` claim; the receiving NodeService interceptor cross-checks NodeHello.NodeId against it.")
	nodeType := fs.String("node-type", "",
		"Node type (required; one of: bff / voice / cognition / agent / planner / workbench). Stamped on the `node_type` claim; cross-checked against NodeHello.NodeType.")
	ttl := fs.Duration("ttl", 0,
		"Token lifetime (e.g. 30d=720h, 24h). Zero falls back to identity.DefaultNodeTokenTTLSeconds (30 days).")
	out := fs.String("out", "",
		"Write the bearer to this file (mode 0600) instead of stdout.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*nodeId) == "" {
		fmt.Fprintln(os.Stderr, "node-token mint: --node-id is required")
		fs.Usage()
		return 2
	}
	if strings.TrimSpace(*nodeType) == "" {
		fmt.Fprintln(os.Stderr, "node-token mint: --node-type is required")
		fs.Usage()
		return 2
	}
	if !validNodeTokenTypes[*nodeType] {
		valid := make([]string, 0, len(validNodeTokenTypes))
		for k := range validNodeTokenTypes {
			valid = append(valid, k)
		}
		fmt.Fprintf(os.Stderr, "node-token mint: --node-type %q is invalid (allowed: %s)\n",
			*nodeType, strings.Join(valid, ", "))
		return 2
	}

	// Apply the same local /.env override the server bootstrap does
	// so the CLI sees the same IDENTITY_KEY_DIR / DSN as the running
	// identity service (the operator typically execs into the
	// container, where /.env is absent and this is a no-op).
	if _, err := genesis.ApplyLocalOverride("."); err != nil {
		fmt.Fprintf(os.Stderr, "node-token mint: local .env override failed: %v\n", err)
		// Non-fatal -- envelope/container env may already cover everything.
	}

	logger := mustCreateServiceLogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	// Start ONLY the dependencies the mint touches (database +
	// engine + identity service). Same selection helper
	// voice-agent-token mint uses; skipping transport keeps the CLI
	// from binding to ports the live identity container already
	// owns -- the subcommand is typically `docker exec`'d into it.
	deps := selectMintDependencies(application.Dependencies)
	for _, d := range deps {
		d.Start(context.Background())
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), dependencyShutdownTimeout)
		defer cancel()
		for i := len(deps) - 1; i >= 0; i-- {
			deps[i].Stop(shutdownCtx)
		}
	}()

	svc, ok := application.IdentityService().(*identity.Service)
	if !ok || svc == nil {
		fmt.Fprintln(os.Stderr, "node-token mint: identity service not wired (binary missing -tags identity?)")
		return 1
	}

	identityId, err := newNodeTokenIdentityId(*nodeType)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-token mint: generate identity id: %v\n", err)
		return 1
	}

	resolvedTTL := *ttl
	if resolvedTTL <= 0 {
		resolvedTTL = time.Duration(identity.DefaultNodeTokenTTLSeconds) * time.Second
	}

	now := time.Now().UTC()
	bearer, jwtExpiresAt, err := svc.Issuer().IssueNodeAccessToken(
		identity.NodeIssueInput{
			IdentityId:  identityId,
			NodeId:      *nodeId,
			NodeType:    *nodeType,
			TTLOverride: resolvedTTL,
		},
		now,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node-token mint: sign JWT: %v\n", err)
		return 1
	}

	if *out != "" {
		if err := os.WriteFile(*out, []byte(bearer+"\n"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "node-token mint: write --out: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "node-token mint: wrote bearer to %s (mode 0600); node=%s type=%s expires=%s\n",
			*out, *nodeId, *nodeType, jwtExpiresAt.Format(time.RFC3339))
	} else {
		// stdout = the bearer alone, no decoration, so the make
		// target / dev-refresh can capture it with $(...).
		fmt.Println(bearer)
		fmt.Fprintf(os.Stderr, "node-token mint: minted node=%s type=%s expires=%s\n",
			*nodeId, *nodeType, jwtExpiresAt.Format(time.RFC3339))
	}
	return 0
}

// newNodeTokenIdentityId synthesises a v1:identity:identity id used
// as the JWT subject. The identity row does not actually exist in
// the database -- the verifier validates the JWT via signature +
// class + node_id/node_type claims alone -- so this is a purely
// stable subject string for log correlation, not a foreign key.
func newNodeTokenIdentityId(nodeType string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "v1:identity:identity:node_" + nodeType + "_" + hex.EncodeToString(b), nil
}
