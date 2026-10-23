// Command memql is MemQL Cockpit's CLI: the fleet worker runtime
// (headless and computer-use) plus the small command set that enrolls
// and operates a machine against a MemQL cluster.
//
// Usage:
//
//	memql cluster add <domain|url>       Register a cluster (discovery + OAuth login)
//	memql cluster list                   List saved clusters
//	memql cluster remove <name>          Remove a saved cluster
//	memql login <cluster>                (Re-)authenticate a saved cluster
//	memql logout <cluster>               Remove cached credentials
//	memql access [<cluster>]             Show your role, rank, groups and scope
//	memql creds <subcommand>             Inspect / migrate the credential store
//	memql worker <subcommand>            Pair / run / configure this machine's worker
//	memql lint [path]                    Validate a .memql file or DSL tree
//	memql setup project [flags]          Stamp a new product workspace from the template
//	memql --version                      Print version + build variant
//
// The engine repo also builds a binary named memql; it ships only
// inside container images and runs in pods. This CLI is what gets
// installed on operator machines -- different distribution channels,
// no PATH overlap, and the collision is deliberate (see
// docs/superpowers/specs/2026-08-25-cockpit-slim-rename-design.md, D1).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/access"
	"github.com/znasllc-io/memql-cockpit/internal/auth"
	"github.com/znasllc-io/memql-cockpit/internal/config"
	"github.com/znasllc-io/memql-cockpit/internal/lint"
	"github.com/znasllc-io/memql-cockpit/internal/setupproject"
	"github.com/znasllc-io/memql-cockpit/internal/worker"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// version is the cockpit's semantic version. The git tag is the
// source of truth (see VERSIONING.md); this tracks the VERSION file and
// is bumped with it by scripts/release/cut-release.sh.
//
// IT MUST BE A `var`, NOT A `const`. The Makefile stamps the real tag in
// with `-ldflags -X main.version=$(VERSION)`, and -X silently does
// NOTHING to a const: no error, no warning, just a binary reporting the
// source string. Measured on this tree -- built with
// `-X main.version=STAMPED-9.9.9`, a const build printed `memql 0.10.0`.
//
// So every release artifact carried the hard-coded constant rather than
// the tag it was cut from, which is the opposite of what VERSIONING.md
// promises ("the tag is the source of truth"). It is invisible because
// the number printed is always plausible.
//
// buildVariant next door stays a `const` on purpose: it is chosen by a
// build tag, not stamped, so there is nothing for -X to set.
// TestVersionIsSettableByLdflags guards the difference.
var version = "0.20.1"
var revision, dirty string

func main() {
	if handled, err := tools.RunScopedCommandChild(os.Args[1:]); handled {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
	// The worker registers this version with the cluster, and this
	// package owns the symbol -ldflags stamps. Handing it over here is
	// what keeps the machine's recorded version the one it was built
	// from rather than a second constant somebody has to remember to
	// bump (memql-cockpit#346's registration row is read by /machines).
	version = stampedVersion(version, revision, dirty == "true")
	worker.SetVersion(version)
	if openBundledApplication() {
		return
	}

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "cluster":
		handleClusterCmd(os.Args[2:])
	case "login":
		handleLoginCmd(os.Args[2:])
	case "logout":
		handleLogoutCmd(os.Args[2:])
	case "access":
		// installCredStore is HANDED OVER rather than called here, so it runs
		// after argv is validated -- the order every sibling command uses.
		// Resolving the OS keyring costs D-Bus round trips and can exit
		// non-zero when MEMQL_COCKPIT_CRED_STORE names an unavailable backend,
		// which `memql access --help` should not pay for or die of.
		os.Exit(access.HandleCommand(os.Args[2:], installCredStore))
	case "menu":
		handleMenu(os.Args[2:])
	case "worker":
		runWorkerCommand(os.Args[2:])
	case "creds":
		handleCredsCmd(os.Args[2:])
	case "lint":
		os.Exit(lint.HandleCommand(os.Args[2:]))
	case "setup":
		os.Exit(setupproject.HandleCommand(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Printf("memql %s (%s)\n", version, buildVariant)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

// installCredStore resolves the credential store (keyring preferred,
// MEMQL_COCKPIT_CRED_STORE override) and installs it for
// config.LoadToken / SaveToken / DeleteToken. Only the cluster /
// login / logout paths route through this: a LaunchAgent-launched
// `worker run` must never trigger a Keychain prompt, so the worker
// keeps the lazy file-store default it has always had.
func installCredStore() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store, err := config.Resolve(config.ResolveOptions{Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	config.SetActiveStore(store)
}

// ---------------------------------------------------------------------------
// Subcommand: cluster
// ---------------------------------------------------------------------------

func handleClusterCmd(args []string) {
	if len(args) == 0 {
		printClusterUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		handleClusterAdd(args[1:])
	case "list", "ls":
		handleClusterList()
	case "remove", "rm":
		handleClusterRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown cluster subcommand %q\n\n", args[0])
		printClusterUsage()
		os.Exit(1)
	}
}

// handleClusterAdd registers a cluster from a single input -- a bare
// domain or a URL -- and runs the OAuth login in the same motion.
// This is the headless successor to the TUI's A keypress: the
// identity service's /.well-known/memql-config.json document is the
// authoritative source; a bare domain falls back to the
// api.<domain> / identity.<domain> convention when discovery is
// unreachable (clusters that predate the endpoint).
func handleClusterAdd(args []string) {
	fs := flag.NewFlagSet("cluster add", flag.ExitOnError)
	name := fs.String("name", "", "clusters.yaml slot name (default: derived from discovery/domain)")
	noLogin := fs.Bool("no-login", false, "register only; skip the OAuth login")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: a domain or URL is required")
		fmt.Fprintln(os.Stderr, "Usage: memql cluster add <domain|url> [--name <name>] [--no-login]")
		os.Exit(1)
	}
	installCredStore()

	target := strings.TrimSpace(fs.Arg(0))
	cfg, err := resolveClusterTarget(context.Background(), target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if *name != "" {
		cfg.Name = strings.TrimSpace(*name)
	}
	if cfg.Name == "" {
		fmt.Fprintln(os.Stderr, "ERROR: could not derive a cluster name; pass --name")
		os.Exit(1)
	}
	if cfg.Name == "local" {
		fmt.Fprintln(os.Stderr, "ERROR: \"local\" is the reserved local-cluster slot; pass --name to pick another")
		os.Exit(1)
	}

	err = config.UpdateClusters(context.Background(), func(clusters *config.ClustersFile) error {
		if _, exists := clusters.Get(cfg.Name); exists {
			return fmt.Errorf("cluster %q already exists; use `memql login` to sign in or choose another name", cfg.Name)
		}
		clusters.Clusters = append(clusters.Clusters, cfg)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Cluster %q registered (endpoint %s, issuer %s)\n", cfg.Name, cfg.Endpoint, cfg.Issuer)

	if *noLogin {
		fmt.Printf("Run `memql login %s` to authenticate.\n", cfg.Name)
		return
	}
	loginCluster(cfg)
}

// resolveClusterTarget turns the user's one input into a full
// ClusterConfig. A URL input names the identity origin directly and
// discovery is the only source; a bare domain tries
// https://identity.<domain> and falls back to the front-door
// convention.
func resolveClusterTarget(ctx context.Context, target string) (config.ClusterConfig, error) {
	if strings.Contains(target, "://") {
		u, err := url.Parse(target)
		if err != nil {
			return config.ClusterConfig{}, fmt.Errorf("parse %q: %w", target, err)
		}
		if u.Host == "" {
			return config.ClusterConfig{}, fmt.Errorf("%q has no host", target)
		}
		base := u.Scheme + "://" + u.Host
		doc, err := config.FetchDiscovery(ctx, base)
		if err != nil {
			return config.ClusterConfig{}, err
		}
		return clusterFromDiscovery(doc, ""), nil
	}
	domain := strings.Trim(strings.TrimSpace(target), "/")
	doc, err := config.FetchDiscovery(ctx, "https://identity."+domain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: discovery failed (%v)\n", err)
		fmt.Fprintf(os.Stderr, "WARN: using the api.%s / identity.%s convention instead\n", domain, domain)
		return config.ComposeFromDomain(domain), nil
	}
	return clusterFromDiscovery(doc, domain), nil
}

func clusterFromDiscovery(doc *config.DiscoveryDocument, domain string) config.ClusterConfig {
	display := strings.TrimSpace(doc.ClusterName)
	if display == "" {
		display = domain
	}
	nameSource := domain
	if nameSource == "" {
		nameSource = display
	}
	if nameSource == "" {
		if u, err := url.Parse(doc.IdentityURL); err == nil {
			nameSource = u.Host
		}
	}
	endpoint := strings.TrimSpace(doc.GRPCEndpoint)
	if endpoint == "" && domain != "" {
		endpoint = "https://api." + domain
	}
	// The discovery document names the client the cockpit should present.
	// When that is the cockpit's own default -- or nothing -- the entry
	// carries no client_id at all: clusters.yaml is shared with other
	// tools, and the default is compiled in (config.ClusterConfig.ClientId).
	return config.ClusterConfig{
		Name:        config.DomainToName(nameSource),
		DisplayName: display,
		Domain:      domain,
		Endpoint:    endpoint,
		Issuer:      strings.TrimSpace(doc.IdentityURL),
		ClientId:    config.StoredClientId(doc.ClientId),
	}
}

func handleClusterList() {
	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-20s %-40s %-15s %s\n", "NAME", "ENDPOINT", "AUTH", "ISSUER")
	fmt.Printf("%-20s %-40s %-15s %s\n", "----", "--------", "----", "------")

	// `local` is rendered first whether or not it lives in
	// clusters.yaml. When the yaml has no entry the endpoint falls to
	// the ingress default and auth shows "not configured".
	local, hasLocal := clusters.Get("local")
	if !hasLocal {
		local = config.ClusterConfig{Name: "local"}
	}
	printClusterRow(local)
	for _, c := range clusters.Clusters {
		if c.Name == "local" {
			continue
		}
		printClusterRow(c)
	}
}

func printClusterRow(c config.ClusterConfig) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = "-"
	}
	issuer := c.Issuer
	if issuer == "" {
		issuer = "-"
	}
	fmt.Printf("%-20s %-40s %-15s %s\n", c.Name, endpoint, clusterAuthLabel(c), issuer)
}

// clusterAuthLabel returns a human-readable description of what kind
// of credential is configured for a cluster.
func clusterAuthLabel(c config.ClusterConfig) string {
	switch {
	case c.PAT != "":
		return "PAT"
	case c.Issuer != "":
		// The client is EffectiveClientId, which is never empty.
		return "OIDC"
	default:
		return "not configured"
	}
}

func handleClusterRemove(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql cluster remove <name>")
		os.Exit(1)
	}
	name := args[0]

	if name == "local" {
		fmt.Fprintln(os.Stderr, "ERROR: the \"local\" cluster cannot be removed (it is a permanent default)")
		os.Exit(1)
	}
	installCredStore()

	err := config.UpdateClusters(context.Background(), func(clusters *config.ClustersFile) error {
		for i, c := range clusters.Clusters {
			if c.Name != name {
				continue
			}
			clusters.Clusters = append(clusters.Clusters[:i], clusters.Clusters[i+1:]...)
			if clusters.SelectedCluster == name {
				clusters.SelectedCluster = ""
			}
			return nil
		}
		return fmt.Errorf("cluster %q not found", name)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// Also remove cached credentials.
	_ = config.DeleteToken(name)

	fmt.Printf("Cluster %q removed\n", name)
}

// ---------------------------------------------------------------------------
// Subcommand: login / logout
// ---------------------------------------------------------------------------

func handleLoginCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql login <cluster>")
		os.Exit(1)
	}
	name := args[0]
	installCredStore()

	clusters, err := config.LoadClusters()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	c, ok := clusters.Get(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: cluster %q not found in ~/.memql/clusters.yaml\n", name)
		fmt.Fprintln(os.Stderr, "Register it first: memql cluster add <domain|url>")
		os.Exit(1)
	}
	loginCluster(c)
}

func loginCluster(c config.ClusterConfig) {
	fmt.Printf("Authenticating with %q...\n", c.Name)
	token, err := auth.EnsureValidToken(context.Background(), c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	if token != "" {
		fmt.Printf("Authenticated with %q. Credentials cached (backend: %s).\n",
			c.Name, config.ActiveStore().Name())
	}
}

func handleLogoutCmd(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ERROR: cluster name is required")
		fmt.Fprintln(os.Stderr, "Usage: memql logout <cluster>")
		os.Exit(1)
	}
	name := args[0]
	installCredStore()

	if err := auth.Logout(name); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Logged out from %q. Cached credentials removed.\n", name)
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

func printUsage() {
	fmt.Println("memql — MemQL Cockpit: fleet worker runtime + cluster CLI")
	fmt.Println("")
	fmt.Println("USAGE")
	fmt.Println("  memql cluster add <domain|url>    Register a cluster (discovery + OAuth login)")
	fmt.Println("  memql cluster list                List saved clusters")
	fmt.Println("  memql cluster remove <name>       Remove a saved cluster")
	fmt.Println("  memql login <cluster>             (Re-)authenticate a saved cluster")
	fmt.Println("  memql logout <cluster>            Remove cached credentials")
	fmt.Println("  memql access [<cluster>] [--json] Show your role, rank, groups and account scope")
	fmt.Println("  memql creds <subcommand>          Inspect / migrate the credential store")
	fmt.Println("  memql worker <subcommand>         Pair / run / configure this machine's worker")
	fmt.Println("  memql menu [flags]                Linux desktop tray (install / start / uninstall)")
	fmt.Println("  memql lint [path]                 Validate a .memql file or DSL tree")
	fmt.Println("  memql setup project [flags]       Stamp a new product workspace from the template")
	fmt.Println("  memql --version                   Print version + build variant")
	fmt.Println("")
	fmt.Println("GETTING A MACHINE ONTO A CLUSTER")
	fmt.Println("  1. memql cluster add <domain>         # register + sign in")
	fmt.Println("  2. memql worker pair <code>           # redeem the pairing code from the portal")
	fmt.Println("  The worker then runs as a service (LaunchAgent / systemd; see worker --help).")
}

func printClusterUsage() {
	fmt.Println("Usage: memql cluster <subcommand>")
	fmt.Println("")
	fmt.Println("Subcommands:")
	fmt.Println("  add <domain|url> [--name N] [--no-login]   Register a cluster + OAuth login")
	fmt.Println("  list                                       List clusters (local is always present)")
	fmt.Println("  remove <name>                              Remove a cluster (local cannot be removed)")
}
