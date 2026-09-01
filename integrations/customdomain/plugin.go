package customdomain

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/component/memql"
)

// plugin.go -- self-registration and the environment-derived half of Config.
//
// Registered on EVERY node type. The concept and its queries load everywhere
// anyway (every binary loads every concept), and the two capabilities are
// cheap: `add` is reached from whichever node serves the OS shell's connection,
// and `reconcile` fires from the scheduled automation, which the automations
// cron leader elects ONE runner for across the mesh (component/automations'
// cron_leader.go). Gating the registration by build tag would make the add
// capability's availability depend on which node a browser's stream happened to
// land on -- a coin flip at two replicas, which is exactly the class of bug
// memql#4352 closed for workers.

func init() {
	memql.RegisterPlugin("customDomain", func(pctx memql.PluginContext) (memql.IntegrationProvider, error) {
		logger := pctx.Logger
		provisioner, err := SelectProvisioner()
		if err != nil {
			// NOT a boot failure. A process with no in-cluster token and no
			// checkout still needs the concept, the queries and the add
			// capability -- a person can bind a domain and read its guidance
			// records long before anything provisions. What it cannot do is
			// provision, and the honest way to say that is a refusal recorded
			// on the row every pass, which refusingProvisioner does.
			if logger != nil {
				logger.Warn("custom domains: no provisioning substrate on this node; bindings will verify but not issue",
					"component", "customDomain", "error", err)
			}
			provisioner = refusingProvisioner{reason: err.Error()}
		} else if logger != nil {
			logger.Info("custom domains: provisioning substrate selected",
				"component", "customDomain", "substrate", provisioner.Describe())
		}
		return NewIntegration(engineAdapter{pctx.Engine}, ConfigFromEnv(), provisioner, logger), nil
	})
}

// engineAdapter narrows IntegrationEngineAccess to the one method this package
// uses. Narrow deliberately: nothing here should be able to reach the AI
// provider registry or the tool surface.
type engineAdapter struct{ engine memql.IntegrationEngineAccess }

func (a engineAdapter) Execute(ctx context.Context, query string) (any, error) {
	return a.engine.Execute(ctx, query)
}

// Defaults. Every one is a VALUE an overlay may change; none of them branches
// on which target this is (design D7).
const (
	// defaultIngressClass is what both cloud overlays and the local traefik
	// front door already carry, so the objects this applies land on the same
	// controller every other Ingress in the cluster does.
	defaultIngressClass = "nginx"
	// defaultNamespace matches deploycontrol's own clusterNamespace and the
	// base kustomization's default.
	defaultNamespace = "memql"
	// defaultEdgeService / defaultEdgePort are the edge's HTTP Service, the
	// same backend the `*.<domain>` wildcard rule points at
	// (deploy/k8s/base/edge.yaml).
	defaultEdgeService = "edge"
	defaultEdgePort    = 8085
	// defaultMaxPerSite caps how many custom domains one deployable may bind.
	//
	// FIVE, and the number is about ACME rather than about storage. Let's
	// Encrypt's certificates-per-registered-domain limit is real and shared
	// across everyone under that domain; a site typically needs its apex and
	// its www, so five leaves room for a staging host and a vanity name while
	// keeping a mistake -- a loop, a script -- from turning into a rate-limit
	// incident that also locks out every other domain on the cluster.
	defaultMaxPerSite = 5
	// defaultDomain matches component/memql's site hostname policy, so a
	// cluster with no MEMQL_DOMAIN answers both consistently.
	defaultDomain = "memql.localhost"
)

// ConfigFromEnv derives the reconciler's configuration.
//
// THE EDGE HOST IS DERIVED, NOT CONFIGURED, unless an operator overrides it. It
// is composed from MEMQL_DOMAIN through component/frontdoor -- the same single
// derivation the Ingress rules, the certificate SANs and every node's own
// issuer come from -- so the host a client is told to point at cannot disagree
// with the host this cluster actually answers on. A second spelling would be a
// CNAME target that resolves nowhere, presenting to the client as "your DNS is
// wrong" with every manifest looking correct.
func ConfigFromEnv() Config {
	domain := strings.ToLower(strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")))
	if domain == "" {
		domain = defaultDomain
	}
	edgeHost := strings.TrimSpace(os.Getenv("MEMQL_CUSTOM_DOMAIN_EDGE_HOST"))
	if edgeHost == "" {
		// The OS shell's host rather than the apex: it is a named front-door
		// host the platform ships itself, so it always exists, always has an
		// exact Ingress rule and always resolves to the edge. The apex is a
		// site row an operator may never have created DNS for, and a CNAME
		// target that does not resolve makes every client's correct record
		// look wrong.
		edgeHost = frontdoor.OsHost(domain)
	}
	return Config{
		EdgeHost:     NormalizeHostname(edgeHost),
		ACMEIssuer:   strings.TrimSpace(os.Getenv("MEMQL_CUSTOM_DOMAIN_ACME_ISSUER")),
		Namespace:    envOr("MEMQL_CUSTOM_DOMAIN_NAMESPACE", defaultNamespace),
		IngressClass: envOr("MEMQL_CUSTOM_DOMAIN_INGRESS_CLASS", defaultIngressClass),
		EdgeService:  envOr("MEMQL_CUSTOM_DOMAIN_EDGE_SERVICE", defaultEdgeService),
		EdgePort:     envInt("MEMQL_CUSTOM_DOMAIN_EDGE_PORT", defaultEdgePort),
		MaxPerSite:   envInt("MEMQL_CUSTOM_DOMAIN_MAX_PER_SITE", defaultMaxPerSite),
	}
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// envInt parses a positive integer, falling back on anything else.
//
// A ZERO OR NEGATIVE VALUE FALLS BACK rather than meaning "unlimited". The one
// place this is read for a cap, MaxPerSite, is a rate-limit guard: reading an
// operator's typo as "no limit" would remove exactly the protection the value
// exists to provide, which is the mistake ai_guard.go's own zero-is-unlimited
// reading has to warn about elsewhere.
func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// refusingProvisioner is the substrate a process with neither an in-cluster
// token nor a checkout gets.
//
// It REFUSES with a reason rather than pretending, every pass, so a binding on
// such a node sits in `issuing` with a sentence naming what is missing instead
// of sitting there with nothing to explain it. That is the same choice
// component/deploycontrol made for its git-dependent verbs: name the absent
// prerequisite rather than fail as a generic error.
type refusingProvisioner struct{ reason string }

func (p refusingProvisioner) Describe() string { return "none (" + p.reason + ")" }

func (p refusingProvisioner) Bind(_ context.Context, _ BindRequest) (Outcome, error) {
	return Outcome{Reason: ReasonIssuanceFailed, Detail: "this node cannot provision cluster objects: " + p.reason}, nil
}

func (p refusingProvisioner) Unbind(_ context.Context, _ BindRequest) (Outcome, error) {
	return Outcome{Reason: ReasonIssuanceFailed, Detail: "this node cannot remove cluster objects: " + p.reason}, nil
}
