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

// integration.go -- the two DSL-callable capabilities:
//
//	integration.customDomain.add        the reachable create (dsl builtin customDomainAdd)
//	integration.customDomain.reconcile  one sweep pass (dsl builtin customDomainReconcile)

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
}

// NewIntegration builds the integration over an engine and the provisioning
// substrate this process can use.
func NewIntegration(engine Engine, cfg Config, provisioner Provisioner, logger *slog.Logger) *Integration {
	store := NewStore(engine)
	return &Integration{
		store:      store,
		reconciler: NewReconciler(store, cfg, provisioner, logger),
		resolver:   NewSystemResolver(),
		cfg:        cfg,
		logger:     logger,
	}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "customDomain" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
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
			Name: "reconcile",
			Description: "Run one custom-domain reconciliation pass: verify DNS, provision what is " +
				"ready, and remove what was asked to come down.",
			Handler:    i.handleReconcile,
			ArgsSchema: map[string]string{},
		},
	}
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

// handleReconcile runs one sweep.
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
