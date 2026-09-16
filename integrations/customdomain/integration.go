package customdomain

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// integration.go -- the three DSL-callable capabilities:
//
//	integration.customDomain.add             the reachable create (dsl builtin customDomainAdd)
//	integration.customDomain.releaseForSite  the delete cascade's domain half (dsl builtin customDomainReleaseForSite)
//	integration.customDomain.reconcile       one sweep pass (dsl builtin customDomainReconcile)
//	integration.customDomain.reconcileFrontDoors  one account front-door pass (dsl builtin accountFrontDoorReconcile)

// resultConcept is the synthetic MemoryNode concept these capabilities return.
// An in-flight integration result, never persisted -- the same shape
// integrations/timeutil and integrations/agents use.
const resultConcept = "integration:customDomain:result"

// Integration exposes the custom-domain capabilities.
type Integration struct {
	store      *Store
	reconciler *Reconciler
	resolver   Resolver
	cfg        Config
	logger     *slog.Logger

	// doors is the account front-door sweep (epic memql#5168). A SECOND
	// reconciler on the same integration rather than a second integration,
	// because it is the same job one level up -- same resolver, same
	// provisioning substrate, same script seam -- and a second plug-in would
	// select its substrate independently and could disagree with this one
	// about what this node can do.
	//
	// Nil when this build has no engine to read accounts through, in which
	// case the capability answers a pass that did nothing rather than
	// failing: a node that cannot sweep is not a node that should refuse the
	// automation.
	doors *DoorReconciler
}

// NewIntegration builds the integration over an engine and the provisioning
// substrate this process can use.
func NewIntegration(engine Engine, cfg Config, provisioner Provisioner, logger *slog.Logger) *Integration {
	store := NewStore(engine)
	i := &Integration{
		store:      store,
		reconciler: NewReconciler(store, cfg, provisioner, logger),
		resolver:   NewSystemResolver(),
		cfg:        cfg,
		logger:     logger,
	}
	// The SAME substrate value, asserted rather than re-selected. Both
	// reconcilers apply Ingresses and Certificates through one process's one
	// capability; a node that provisions custom domains through the API server
	// and front doors through a script would be two answers to one question.
	if dp, ok := provisioner.(DoorProvisioner); ok {
		i.doors = NewDoorReconciler(NewDoorStore(engine), NewDoorAccountReader(engine), cfg.doorConfig(), dp, logger)
	} else if logger != nil {
		logger.Warn("account front doors: this node's provisioning substrate cannot serve them; doors will verify but not issue",
			"component", "accountFrontDoor", "substrate", provisioner.Describe())
	}
	return i
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "customDomain" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "dnsGuidance",
			Description: "Read the configured routing hostname and current IPv4/IPv6 targets for an accessible custom-domain binding.",
			Handler:     i.handleDNSGuidance,
			ArgsSchema:  map[string]string{"domainId": "string (required) -- the custom-domain binding to configure."},
		},
		{
			Name: "add",
			Description: "Bind a client's own domain to one of this cluster's deployables. Mints the " +
				"ownership token, prefills the account tie, and lands the row pending its DNS records.",
			Handler: i.handleAdd,
			ArgsSchema: map[string]string{
				"siteId":   "string (required) -- the v1:platform:site row this domain should serve.",
				"hostname": "string (required) -- the client's own fully qualified host.",
			},
		},
		{
			Name: "releaseForSite",
			Description: "Walk every live binding on one deployable to `removing`, so a deleted " +
				"deployable stops answering on its client's domains and frees their hostnames.",
			Handler: i.handleReleaseForSite,
			ArgsSchema: map[string]string{
				"siteId": "string (required) -- the v1:platform:site row whose bindings should come down.",
			},
		},
		{
			Name: "reconcileAccountDomains",
			Description: "Run one pass of the ACCOUNT domain walk: mint an ownership token, check " +
				"the TXT record, and stamp verification and the reserved MemQL name on success.",
			Handler:    i.handleReconcileAccountDomains,
			ArgsSchema: map[string]string{},
		},
		{
			Name: "reconcile",
			Description: "Run one custom-domain reconciliation pass: verify DNS, provision what is " +
				"ready, and remove what was asked to come down.",
			Handler:    i.handleReconcile,
			ArgsSchema: map[string]string{},
		},
		{
			Name: "reconcileFrontDoors",
			Description: "Run one account front-door pass: open a door for every held reservation, " +
				"check the three CNAMEs, provision what is ready, and take down every door whose " +
				"reservation was withdrawn.",
			Handler:    i.handleReconcileFrontDoors,
			ArgsSchema: map[string]string{},
		},
	}
}

// handleReconcileFrontDoors runs one account front-door pass.
//
// A NODE WITH NO SWEEP ANSWERS A PASS THAT DID NOTHING rather than failing.
// The automation fires on every replica's cron leader election, and a build
// that cannot provision must not turn that into an error the scheduler retries
// -- the honest report is a pass with five zeroes, which is exactly what it
// did.
func (i *Integration) handleReconcileFrontDoors(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	var res DoorPassResult
	if i.doors != nil {
		var err error
		res, err = i.doors.Run(ctx)
		if err != nil {
			return nil, err
		}
	}
	return i.node(fmt.Sprintf("reconcileFrontDoors:%d", time.Now().UnixNano()), map[string]any{
		"opened":   res.Opened,
		"checked":  res.Checked,
		"verified": res.Verified,
		"issued":   res.Issued,
		"removed":  res.Removed,
		"demoted":  res.Demoted,
		"failed":   res.Failed,
	})
}

// mintToken returns the ownership token a client publishes in DNS.
//
// 32 bytes of crypto/rand, base64url without padding: 256 bits, unguessable,
// and a single DNS-safe string with no characters a zone file or a registrar's
// form will mangle. Padding is stripped because `=` is legal in a TXT string
// but is exactly the character a copy-paste through a web form is most likely
// to lose.
//
// UNGUESSABLE MATTERS EVEN THOUGH THE VALUE IS PUBLISHED. Anyone can read the
// token once it is in DNS; what the entropy buys is that nobody can PREDICT one
// before it is minted, which is what would let an attacker pre-publish the
// record under a domain in the hope of claiming it the moment somebody here
// types the hostname.
func mintToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("customdomain: mint verification token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// handleAdd creates a binding.
//
// The three D10 guards live in component/memql's write path rather than here,
// deliberately: they have to bind the raw insert() surface and any future
// writer too, and a check that only one caller runs is not a check. What this
// handler owns is the two things only it can do -- mint the token, and resolve
// the site's account tie.
func (i *Integration) handleAdd(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := strings.TrimSpace(asString(args["siteId"]))
	hostname := NormalizeHostname(asString(args["hostname"]))
	if siteID == "" {
		return nil, fmt.Errorf("customDomainAdd: siteId is required -- a custom domain with no deployable behind it is a hostname the edge would resolve to nothing")
	}
	if hostname == "" {
		return nil, fmt.Errorf("customDomainAdd: hostname is required")
	}

	// THE SITE READ RUNS UNDER THE CALLER, not under the sweep's synthetic
	// operator, which is what makes it an authorization check rather than a
	// lookup: siteById carries v1:platform:site's composite tier, so a caller
	// who cannot read the deployable resolves zero rows and is refused by name
	// here -- before a token is minted or a row is written.
	exists, err := i.store.SiteExists(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("customDomainAdd: could not resolve deployable %q: %w", siteID, err)
	}
	if !exists {
		return nil, fmt.Errorf(
			"customDomainAdd: no deployable %q is readable by this caller, so there is nothing to bind %q to",
			siteID, hostname)
	}

	token, err := mintToken()
	if err != nil {
		return nil, err
	}

	binding := Binding{
		ID:        id.NewShortId(),
		SiteID:    siteID,
		Hostname:  hostname,
		Token:     token,
		AccountID: i.store.SiteAccountID(ctx, siteID),
		Status:    StatusPendingDNS,
	}
	if err := i.store.Create(ctx, binding); err != nil {
		return nil, err
	}

	pointing := PointingRecordFor(ctx, i.resolver, hostname, i.cfg.EdgeHost)
	payload := map[string]any{
		"domainId":         binding.ID,
		"siteId":           binding.SiteID,
		"hostname":         binding.Hostname,
		"accountId":        binding.AccountID,
		"token":            binding.Token,
		"status":           StatusPendingDNS,
		"verifyRecordName": VerifyRecordName(hostname),
		"pointsToKind":     pointing.Kind,
		"pointsToTarget":   pointing.Target,
	}
	return i.node("add:"+binding.ID, payload)
}

// handleReleaseForSite walks every live binding on one deployable to
// `removing`, which is the domain half of the delete cascade (epic
// memql#4937).
//
// IT LIVES HERE RATHER THAN IN component/packages, and the reason is module
// direction: the cascade runs from the packages integration, and having that
// package import this one would make a component depend on an integration.
// The seam it uses instead is the one it already uses to ADD a domain during a
// placement -- a builtin call through the engine -- so the actor, the rows and
// the state machine all stay owned by exactly one package.
//
// THE ROWS ARE READ UNDER THIS INTEGRATION'S OWN SYSTEM ACTOR, not the
// caller's, and that is load-bearing. `v1:platform:customDomain` is
// clusterOwner-tier, so an ordinary owner deleting their own deployable reads
// ZERO bindings under their actor -- and would tear down nothing while the
// Ingress and Certificate stayed applied and the hostname stayed claimed.
// That is the exact failure this epic exists to remove, so it must not be
// reintroduced by reading as the caller. Authorization is already settled
// upstream: `siteDelete` resolves the site under the caller's own actor and
// refuses before it reaches this.
//
// ALREADY-TERMINAL BINDINGS ARE COUNTED, NOT RE-WRITTEN. A row at `removed`
// is done and a row at `removing` is already on the sweep's list; asking again
// would rewrite `lastCheckedAt` and put a second identical entry in the audit
// history for a person to wonder about.
func (i *Integration) handleReleaseForSite(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	siteID := strings.TrimSpace(asString(args["siteId"]))
	if siteID == "" {
		return nil, fmt.Errorf("customdomain: releaseForSite needs a siteId")
	}
	bindings, err := i.store.ForSite(SystemActorContext(ctx), siteID)
	if err != nil {
		return nil, err
	}
	requested, alreadyDown := 0, 0
	hostnames := make([]string, 0, len(bindings))
	for _, b := range bindings {
		if b.Status == StatusRemoved || b.Status == StatusRemoving {
			alreadyDown++
			continue
		}
		if rerr := i.store.RequestRemoval(SystemActorContext(ctx), b.ID); rerr != nil {
			// SURFACED, NOT SWALLOWED. The caller stamps the site row LAST,
			// so a failure here leaves a deployable that is still findable
			// and still says what state it is in -- which is the whole
			// reason for that ordering.
			return nil, fmt.Errorf("customdomain: could not start removing %q: %w", b.Hostname, rerr)
		}
		requested++
		hostnames = append(hostnames, b.Hostname)
	}
	return i.node("release:"+siteID, map[string]any{
		"siteId":         siteID,
		"requested":      requested,
		"alreadyRemoved": alreadyDown,
		"hostnames":      hostnames,
	})
}

// handleReconcile runs one sweep.
// handleReconcileAccountDomains is the account walk's dispatch point (epic
// memql#5165). Beside handleReconcile because the two ask DNS the same
// question of two different rows, and sharing the resolver, the token mint and
// the schedule is the whole reason the walk lives in this package.
func (i *Integration) handleReconcileAccountDomains(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.ReconcileAccountDomains(ctx)
	if err != nil {
		return nil, err
	}
	return i.node(fmt.Sprintf("accountDomains:%d", time.Now().UnixNano()), map[string]any{
		"checked": res.Checked, "minted": res.Minted, "verified": res.Verified,
		"failed": res.Failed, "reserved": res.Reserved,
	})
}

func (i *Integration) handleReconcile(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.reconciler.Run(ctx)
	if err != nil {
		return nil, err
	}
	return i.node(fmt.Sprintf("reconcile:%d", time.Now().UnixNano()), map[string]any{
		"checked":  res.Checked,
		"verified": res.Verified,
		"issued":   res.Issued,
		"removed":  res.Removed,
		"failed":   res.Failed,
	})
}

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("customdomain: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "customDomain:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   bytes,
	}}, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
