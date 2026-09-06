package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/core/id"
)

// Self-serve deployables: the OWNER STAMP and the HOSTNAME POLICY for
// v1:platform:site (memql#4344, design section 4.1/4.2). Sibling of
// platform_site_delete_guard.go, wired into the same executeWrite path and for
// the same reason: neither rule is expressible in a mutation body, and a
// UI-only check is not a check.
//
// # What changed underneath, and why the guard is the consequence
//
// v1:platform:site used to be @rowAuthz(clusterOwner) -- only an operator had
// sites at all, so there was nothing to own and every hostname was an operator's
// deliberate choice. Deployables are self-serve now, the concept declares the
// COMPOSITE tier (@rowAuthz(owner="ownerUserId", clusterOwner), memql#4312), and
// two questions appear that the DSL cannot answer:
//
//	 1. WHETHER A NEW ROW IS ANYBODY'S. createSite stamps
//	    `ownerUserId: actor.userId` -- it has to, because a declared owner tier
//	    over a caller-supplied field is a guarantee nothing provides. But the
//	    seeded OS site must land CLUSTER-OWNED, which is what an
//	    EMPTY ownerUserId means, and the SeedMaterializer runs that same
//	    mutation under a synthetic actor on every boot. "A write made as the
//	    DEPLOYMENT produces the deployment's row" is conditional on the actor's
//	    ROLE, and a mutation body has no way to ask. So the stamp is UNDONE
//	    here, and only ever undone -- see applySiteOwnerStamp.
//	 2. WHICH HOSTNAMES A USER MAY CLAIM. A user's site is `<slug>.<domain>` for
//	    the domain THIS cluster serves; the slug is bounded, cluster-unique and
//	    not one of the reserved labels. None of that is a predicate over the
//	    payload alone: the domain comes from the environment, uniqueness needs a
//	    read, and the whole rule is waived for a cluster owner.
//
// # Why the hostname rule is not merely cosmetic
//
// The front door routes every site through ONE `*.<domain>` Ingress rule, and a
// wildcard matches exactly one label (component/frontdoor). So `<slug>.<domain>`
// is the shape that routes with no operator step, and `shop.eu.<domain>` or a
// second domain entirely is a hostname nothing routes. Certificates are
// stricter still: the cloud issuer solves HTTP-01, which cannot issue a
// wildcard, so the front-door certificate names EXACT hosts (memql#4224) and a
// custom hostname needs a Certificate somebody creates by hand. Handing that
// decision to any authenticated caller would let one mint rows the cluster
// cannot serve; the reserved list on top of it is what stops a user claiming
// `identity.<domain>` and being handed sign-in traffic.
//
// The local overlay's mkcert pair IS a wildcard, so a user-created site works
// over https locally whatever its hostname. That is a TLS-source value, not a
// second policy -- and it is exactly why this rule cannot be left to "it will
// fail when it fails". Locally it would not fail.
//
// # The escape set is the one every other write-side rule uses
//
// rowAuthzWriteEscape (memql#3174): internal origin stamped for this one write,
// or the cluster owner. Plus isSystemActor, matching the sibling delete guard,
// so the SeedMaterializer's re-write of the OS site row on every boot is not
// refused by a policy written for people. `admin` is deliberately not among
// them, for the reason rowAuthzWriteEscape states at length.

// siteSlugPattern is the user-claimable label: lowercase alphanumerics and
// hyphens, 3 to 40 characters.
//
// Deliberately NARROWER than DNS. A label may be 63 characters and may carry
// uppercase in its presentation form; this refuses both, because the value is
// also a display handle and a case-folded collision ("Shop" vs "shop") would
// resolve to one site while reading as two rows.
var siteSlugPattern = regexp.MustCompile(`^[a-z0-9-]{3,40}$`)

// siteSlugMinLen / siteSlugMaxLen mirror the pattern's bounds so an error can
// name them without a reader parsing the regex.
const (
	siteSlugMinLen = 3
	siteSlugMaxLen = 40
)

// defaultSiteDomain is the domain the policy derives from when MEMQL_DOMAIN is
// unset -- the same committed local default the OS site seed carries.
//
// It is the DOMAIN, not the host: seed_materializer.go holds the host form
// (defaultOsHostname). TestSiteHostnamePolicyDefaultDomainMatchesTheOsSeed
// pins the two together through frontdoor.OsHost, so the pair cannot drift
// into a cluster whose own shell is served at one domain and whose users are
// admitted on another.
const defaultSiteDomain = "memql.localhost"

// squatReservedSiteLabels are the labels held back from users that the front
// door does not already name.
//
// The front door's own labels come from frontdoor.Roles() and
// frontdoor.OsSite rather than being re-listed here, so a new role reserves
// its label automatically -- the failure mode of a second copy is that adding a
// role silently opens its hostname to the first user who asks for it.
//
// These four are different: nothing in the cluster serves www / admin / mail /
// portal today, and that is exactly why they are reserved. They are the labels
// a person reads as the ORGANISATION's rather than a tenant's, and `mail` in
// particular is where a mail host would land if one is ever added.
//
// `portal` MOVED HERE rather than being freed when the portal was retired
// (epic memql#4984). It stopped being a front-door host and did not stop being
// a name that reads as the platform's: a user site at portal.<domain> on a
// MemQL install is exactly the confusion this list exists to prevent. And
// un-reserving a label is a ONE-WAY DOOR -- once somebody has claimed it,
// taking it back is a support conversation -- while keeping it costs one
// entry.
var squatReservedSiteLabels = []string{"www", "admin", "mail", "portal"}

// reservedSiteLabels is the closed set of labels a user may not claim, keyed
// lowercase.
func reservedSiteLabels() map[string]bool {
	out := make(map[string]bool, len(frontdoor.Roles())+1+len(squatReservedSiteLabels))
	for _, r := range frontdoor.Roles() {
		out[string(r)] = true
	}
	out[frontdoor.OsSite] = true
	for _, l := range squatReservedSiteLabels {
		out[l] = true
	}
	return out
}

// siteHostnamePolicyDomain is the domain a user's site must sit under: the one
// this cluster serves.
//
// Read from MEMQL_DOMAIN, the single key every domain-shaped value in the
// cluster derives from (component/envregistry/domain.go). Falling back to the
// committed local default rather than to "" is what keeps the policy a
// SELECTION: an empty domain would make every hostname end in "." and the
// suffix test would admit nothing at all, which is a fail-closed answer that
// reads to an operator as "site creation is broken".
func siteHostnamePolicyDomain() string {
	domain := strings.TrimSpace(os.Getenv(memqlDomainEnv))
	if domain == "" {
		return defaultSiteDomain
	}
	return strings.ToLower(domain)
}

// validateUserSiteHostname is the policy itself, pure so it can be tested
// without a database, an environment or an actor.
//
// A hostname passes when it is exactly `<slug>.<domain>`: one label under the
// domain this cluster serves, the label matching siteSlugPattern and not
// reserved. Everything else -- the apex, a deeper label, another domain, a
// reserved label -- is refused here and stays cluster-owner-only.
func validateUserSiteHostname(hostname, domain string) error {
	host := strings.ToLower(strings.TrimSpace(hostname))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" {
		return fmt.Errorf("v1:platform:site: hostname is required")
	}
	if domain == "" {
		// Unreachable through siteHostnamePolicyDomain, which never returns
		// empty. Stated anyway: a caller passing "" would otherwise get a
		// suffix test that matches every hostname ending in a dot.
		return fmt.Errorf(
			"v1:platform:site: cannot check the hostname policy for %q -- this cluster's domain "+
				"did not resolve, and admitting a hostname without one would admit every hostname",
			host)
	}
	if host == frontdoor.Apex(domain) {
		return fmt.Errorf(
			"v1:platform:site: %q is the cluster's apex, which is not a user-claimable hostname. "+
				"The apex is the deployment's own front door and stays cluster-owner-only; pick "+
				"<slug>.%s instead",
			host, domain)
	}
	suffix := frontdoor.DomainDerivationSuffix(domain)
	if !strings.HasSuffix(host, suffix) {
		return fmt.Errorf(
			"v1:platform:site: %q is not under this cluster's domain (%s). A user-created site is "+
				"%s, because the one `*.%s` Ingress rule is what routes it with no operator step; a "+
				"hostname on another domain needs its own DNS, its own Certificate and an operator, "+
				"so it stays cluster-owner-only",
			host, domain, frontdoor.SiteHost("<slug>", domain), domain)
	}
	slug := strings.TrimSuffix(host, suffix)
	if strings.Contains(slug, ".") {
		return fmt.Errorf(
			"v1:platform:site: %q is more than one label under %s. An Ingress wildcard matches "+
				"exactly ONE label, so `*.%s` does not route it and the site would resolve nowhere. "+
				"Use %s",
			host, domain, domain, frontdoor.SiteHost("<slug>", domain))
	}
	if !siteSlugPattern.MatchString(slug) {
		return fmt.Errorf(
			"v1:platform:site: %q is not a usable site name. It must be %d-%d characters of "+
				"lowercase letters, digits and hyphens (the hostname label under %s), and %q is not",
			slug, siteSlugMinLen, siteSlugMaxLen, domain, slug)
	}
	if reservedSiteLabels()[slug] {
		return fmt.Errorf(
			"v1:platform:site: %q is reserved. %s is where the cluster serves %s, and a site row "+
				"claiming it would take traffic the front door means for the platform. Reserved "+
				"labels: %s",
			slug, host, slug, strings.Join(sortedReservedSiteLabels(), ", "))
	}
	return nil
}

// sortedReservedSiteLabels renders the reserved set for an error message in a
// stable order (front-door roles first, in the order the manifests emit them,
// then the OS shell, then the squat list) so the message does not shuffle
// between runs of a map iteration.
func sortedReservedSiteLabels() []string {
	out := make([]string, 0, len(frontdoor.Roles())+1+len(squatReservedSiteLabels))
	for _, r := range frontdoor.Roles() {
		out = append(out, string(r))
	}
	out = append(out, frontdoor.OsSite)
	out = append(out, squatReservedSiteLabels...)
	return out
}

// siteWritePrivileged reports whether this caller writes sites as the
// deployment rather than as a person: a cluster owner, trusted server-side Go
// stamping internal origin for this one write, or a system actor.
//
// The three carry different reasons and all three are needed:
//
//   - rowAuthzWriteEscape is the standing write-side escape set (memql#3174).
//     Reusing it rather than re-deriving "is this an operator" is what keeps
//     this guard and the row-authz guard from drifting apart about the very
//     thing they both describe.
//   - isSystemActor covers the SeedMaterializer, which re-writes the OS site
//     row on EVERY boot. It carries a cluster-owner AccessContext today
//     (memql#3711) so rowAuthzWriteEscape would already admit it; the explicit
//     check mirrors the sibling delete guard and survives that envelope
//     changing.
func siteWritePrivileged(ctx context.Context, actor string) bool {
	if _, escaped := rowAuthzWriteEscape(ctx); escaped {
		return true
	}
	identity, _ := auth.UserIdentityFromContext(ctx)
	return isSystemActor(identity, actor)
}

// applySiteOwnerStamp UNDOES createSite's `ownerUserId: actor.userId` stamp
// when the writer is the DEPLOYMENT rather than a person, leaving the row
// cluster-owned.
//
// # It only ever narrows, and that is what makes it safe to have at all
//
// createSite stamps the owner in its own stamp{} block, because a concept
// declaring an owner tier over a CALLER-SUPPLIED field records a guarantee
// nothing provides -- TestDeclaredOwnerFieldsAreServerStamped refuses exactly
// that, and an exemption would be false here since the field genuinely is not
// forgeable. So the arg does not exist and this step cannot introduce one.
//
// What it does is DELETE the stamp, and only when the stamped value is the
// caller's OWN id -- so the outcome is either "the caller owns it" or "nobody
// does". An empty ownerUserId matches nobody (sameRowAuthzOwner refuses an
// empty owner outright), so the row becomes reachable through the cluster-owner
// branch alone. There is no path here that names a third party, which is the
// property the owner gate is actually about.
//
// # Why it has to exist
//
// The seeded OS site must land CLUSTER-OWNED: it is the
// platform's row, not any operator's, and it is how sites are managed at all.
// The SeedMaterializer runs createSite for it under a synthetic actor, and
// re-runs it on EVERY boot (memql#4705), so without this the OS site would be
// owned by "system:seedMaterializer" -- a value that is not a user and that a
// caller could, in principle, be issued.
//
// The same rule then answers the operator case for free: a cluster owner
// creating a site creates the DEPLOYMENT's site, and hands one over (or takes
// it back) by re-running createSite on the id, which the read-merge makes an
// update and the cluster-owner write escape admits.
//
// # The self-match is what keeps an operator from stealing a user's site
//
// A cluster owner running updateSiteBundle against a USER's site arrives here
// with the merged payload carrying that user's ownerUserId. It is not the
// caller's own id, so nothing is deleted and the row stays the user's. Only the
// value createSite just stamped -- the caller's own -- is undone.
//
// Runs BESIDE stampRowAuthzOwner in executeWrite, which is to say BEFORE
// canonicalizeRelationshipFields. That ordering is load-bearing in the other
// direction too: the stamped value is still the BARE caller id here, which is
// what the comparison below expects; after canonicalisation it would be
// `v1:identity:user:<id>` and a raw comparison would stop matching.
func applySiteOwnerStamp(ctx context.Context, payload map[string]any, priorExisted bool, actor string) error {
	if payload == nil {
		return nil
	}
	stamped := strings.TrimSpace(stringFromAny(payload["ownerUserId"]))

	if siteWritePrivileged(ctx, actor) {
		caller := strings.TrimSpace(rowAuthzActorUserId(ctx))
		if caller == "" {
			// A system actor whose AccessContext carries no user id. The
			// stamp rendered empty already; nothing to undo, and nothing to
			// compare against.
			caller = strings.TrimSpace(actor)
		}
		if stamped != "" && sameRowAuthzOwner(stamped, caller) {
			delete(payload, "ownerUserId")
		}
		return nil
	}

	if priorExisted {
		// An update by an ordinary caller. guardRowAuthzWrite has already
		// refused anybody who is not the stored owner, and the delta for
		// updateSiteBundle / updateSiteStatus / deleteSite does not name
		// ownerUserId at all, so the merged value is the stored one. Nothing
		// to decide.
		return nil
	}
	if stamped == "" {
		// A CREATE whose stamp rendered empty: no caller identity resolved.
		// Refuse rather than let it through, because an empty ownerUserId is
		// the CLUSTER-OWNED state -- the row would be minted as the
		// platform's own on an unauthenticated call. Same reasoning as
		// stampRowAuthzOwner's finding-4 refusal, one layer up.
		return fmt.Errorf(
			"v1:platform:site: this create carries no caller identity, so createSite's " +
				"`ownerUserId: actor.userId` stamp rendered empty. An empty ownerUserId means " +
				"CLUSTER-OWNED (the seeded OS site is the row that carries it), so writing it here " +
				"would mint an operator's row on an unauthenticated call. Refused (memql#4344)")
	}
	return nil
}

// validateSiteHostnamePolicy refuses a hostname a non-privileged caller may not
// claim, and refuses any caller a hostname another live site already answers on.
//
// The two halves have different audiences on purpose:
//
//   - The SHAPE rule (<slug>.<domain>, the slug bounds, the reserved list) is
//     about who may CLAIM what, so it runs only on a write that is actually
//     choosing a hostname -- a create, or an update that changes one. Every
//     other write inherits the stored hostname through the read-merge, and
//     judging that inherited value would mean an ordinary user who owns a site
//     at a cluster-owner-created custom hostname could never publish to it,
//     disable it or delete it. A cluster owner is exempt from the rule
//     outright: a custom apex or a second domain is a legitimate operator
//     deployment that arrives with its own DNS and its own Certificate.
//   - UNIQUENESS is about whether the cluster can serve the row at all, so it
//     binds a cluster owner too. Two live rows on one hostname make
//     siteByHostname's answer depend on row order, which is a routing defect
//     rather than a permission one. Only a SYSTEM actor is exempt, so the
//     SeedMaterializer's idempotent re-write of the portal row cannot be refused
//     by a rule written for people.
func (e *MemQLEngine) validateSiteHostnamePolicy(ctx context.Context, payload map[string]any, mutationId, actor string, priorExisted bool, priorHostname string) error {
	if e == nil || payload == nil {
		return nil
	}
	hostname := strings.ToLower(strings.TrimSpace(stringFromAny(payload["hostname"])))
	if hostname == "" {
		// `hostname` is `string!`; the concept schema refuses an empty one
		// downstream. Don't double-error on someone else's rule.
		return nil
	}

	claiming := !priorExisted ||
		!strings.EqualFold(hostname, strings.TrimSpace(priorHostname))
	if claiming && !siteWritePrivileged(ctx, actor) {
		if err := validateUserSiteHostname(hostname, siteHostnamePolicyDomain()); err != nil {
			return err
		}
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	holders, err := e.liveSiteIdsForHostname(ctx, hostname)
	if err != nil {
		// Fail CLOSED, for the reason the sibling slug guard states: "we could
		// not check" and "there is nothing to find" are different answers, and
		// an unavailable database must not be the way a duplicate hostname gets
		// in.
		return fmt.Errorf("v1:platform:site: cannot verify hostname uniqueness for %q: %w", hostname, err)
	}
	self := canonicalSiteStorageId(mutationId)
	for _, h := range holders {
		if canonicalSiteStorageId(h) == self {
			continue
		}
		return fmt.Errorf(
			"v1:platform:site: hostname %q is already served by site %q. A hostname resolves to "+
				"exactly one site -- the edge looks a request's Host up in the graph, so a second "+
				"live row here makes which site answers depend on row order. Pick another name, or "+
				"delete the existing site first (a deleted row frees its hostname).",
			hostname, h)
	}
	return nil
}

// canonicalSiteStorageId converts either spelling of a site row id into the
// stored, concept-qualified form.
//
// Load-bearing rather than cosmetic, exactly as its agentRole counterpart is: a
// mutation carries a BARE id (`portal`) while storage holds
// `v1:platform:site:portal`. Compared raw, self-exclusion never matches and the
// SeedMaterializer's own re-write of the portal row is refused as a duplicate of
// itself on the first boot after this guard lands.
func canonicalSiteStorageId(rowId string) string {
	trimmed := strings.TrimSpace(rowId)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, conceptPlatformSite+":") {
		return trimmed
	}
	return id.BuildNodeId(conceptPlatformSite, trimmed)
}

// liveSiteIdsForHostname returns the ids whose LATEST version answers on this
// hostname and is not soft-deleted.
//
// Two steps, for the reason its agentRole counterpart spells out and which
// applies unchanged here: the store is time-series, so a single
// `WHERE payload->>'hostname' = ?` matches any version that EVER carried the
// hostname -- including one since renamed away, or since deleted -- and
// answering from that set would refuse a hostname nothing currently holds.
// Scanning the whole concept instead is unbounded. So step one narrows to the
// rows that have ever carried this hostname (a predicate the database answers)
// and step two resolves only those to their latest version.
//
// staged-data: MUST-NOT-GATE -- the gate CREATES the violation it would then be
// unable to detect (epic memql#3974, task memql#3984).
//
// This is the uniqueness probe, and the shape is exactly
// agent_role_slug_unique_validation's: it looks for an existing live site
// holding the hostname so the write can be refused. Withhold a staged row and
// the probe finds nothing, the write is admitted, and the cluster now holds two
// live sites on one hostname -- one of which is the very row the probe could not
// see, so the duplicate is invisible to the next probe as well. The read is not
// a disclosure surface; it is the thing keeping a second row out. It reads
// deliberately WITHOUT row-authz narrowing for the same reason: a hostname
// another user already holds must collide even though the caller may not see
// that row, which is why the collision message names the id and nothing else
// about it.
func (e *MemQLEngine) liveSiteIdsForHostname(ctx context.Context, hostname string) ([]string, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}

	// Step 1: which rows have EVER carried this hostname.
	var candidates []string
	err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Column("id").
		Where("concept = ?", conceptPlatformSite).
		Where("lower(payload->>'hostname') = ?", hostname).
		Distinct().
		Scan(ctx, &candidates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan site hostname candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Step 2: resolve each candidate to its LATEST version and ask whether that
	// version still answers on this hostname and is not deleted.
	var nodes []memorynodes.MemoryNode
	err = db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptPlatformSite).
		Where("id IN (?)", bun.In(candidates)).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan site hostname holders: %w", err)
	}

	seen := make(map[string]struct{}, len(candidates))
	var out []string
	for _, node := range nodes {
		if _, dup := seen[node.ID]; dup {
			continue // an older version of a row already resolved
		}
		seen[node.ID] = struct{}{}

		var p map[string]any
		if jerr := json.Unmarshal(node.Payload, &p); jerr != nil {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringFromAny(p["hostname"]))) != hostname {
			continue // renamed away since; step 1 alone is not an answer
		}
		if boolFromAny(p["deleted"]) {
			continue // a deleted row frees its hostname
		}
		out = append(out, node.ID)
	}
	return out, nil
}
