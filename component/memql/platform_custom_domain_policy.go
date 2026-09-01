package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/frontdoor"
	"github.com/znasllc-io/memql/core/id"
)

// Custom domains: the THREE GUARDS (epic memql#4805, design D10). Sibling of
// platform_site_hostname_policy.go, wired into the same executeWrite path and
// for the same reason -- none of these is expressible in a mutation body:
//
//  1. NOT UNDER THE CLUSTER'S OWN DOMAIN. `shop.<domain>` is slug territory,
//     owned by createSite and routed by the one `*.<domain>` Ingress rule that
//     already exists. Binding it here would mint a second Ingress and a second
//     Certificate for a host the front door already serves, and two rules on
//     one host makes which one answers a property of the controller's ordering.
//     The domain comes from the environment, so a mutation body cannot ask.
//
//  2. NOT A COLLISION. A hostname resolves to exactly ONE site: the edge asks
//     siteByHostname first and liveCustomDomainByHostname second, so a custom
//     domain that duplicates a site's hostname -- or another binding's -- makes
//     which site answers depend on row order. Checking needs a READ, and one
//     that deliberately ignores row-authz narrowing: a hostname another user's
//     site holds must still collide even though this caller cannot see that
//     row. The front-door hosts are in the same check, because api / identity /
//     mcp / portal / os / the apex are served by rules this cluster ships and a
//     binding claiming one would be a certificate request for the platform's
//     own sign-in host.
//
//  3. NOT PAST THE PER-SITE MAXIMUM. The cap is about ACME rather than storage:
//     Let's Encrypt's certificates-per-registered-domain limit is shared by
//     everyone under that domain, so a loop that bound domains without bound
//     would take out issuance for every other domain on the cluster too. The
//     maximum is env-tunable, which is exactly why a mutation body cannot carry
//     it.
//
// A fourth rule lives here for the same reason the site delete guard does: a
// TERMINAL row does not walk again. `removeCustomDomain` on a row that is
// already `removed` would put it back into `removing`, the sweep would unbind
// objects that are already gone, and the row's history would record a second
// removal that removed nothing.
//
// # The escape set is deliberately NARROWER than the site policy's
//
// v1:platform:customDomain is clusterOwner tier, so every caller who reaches
// these writes is already an operator or the engine. There is therefore no
// "privileged caller is exempt" branch: an operator binding `api.<domain>` to a
// customer's site is not exercising a privilege, they are making a mistake the
// cluster cannot serve. Only a SYSTEM actor is exempt from the collision read,
// matching the site policy's treatment of the SeedMaterializer -- and nothing
// in the tree seeds a custom domain today, so that branch is a shape rather
// than a live path.

// conceptPlatformCustomDomain is v1:platform:customDomain's canonical concept
// id.
const conceptPlatformCustomDomain = "v1:platform:customDomain"

// customDomainMaxPerSiteEnv is the env-tunable per-site cap (design D10).
const customDomainMaxPerSiteEnv = "MEMQL_CUSTOM_DOMAIN_MAX_PER_SITE"

// defaultCustomDomainMaxPerSite mirrors integrations/customdomain's own
// default. Two spellings of one number is a drift risk, and the alternative --
// importing integrations from component/memql -- would invert the module
// direction; TestCustomDomainMaxPerSiteDefaultsAgree pins them together
// instead.
const defaultCustomDomainMaxPerSite = 5

// customDomainMaxPerSite reads the cap.
//
// A ZERO OR NEGATIVE VALUE FALLS BACK rather than meaning "unlimited". This is
// a rate-limit guard, so reading an operator's typo as "no limit" would remove
// exactly the protection the value exists to provide.
func customDomainMaxPerSite() int {
	raw := strings.TrimSpace(os.Getenv(customDomainMaxPerSiteEnv))
	if raw == "" {
		return defaultCustomDomainMaxPerSite
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultCustomDomainMaxPerSite
	}
	return n
}

// customDomainTerminalStatus reports whether a status is one nothing walks out
// of.
func customDomainTerminalStatus(status string) bool {
	return strings.TrimSpace(status) == "removed"
}

// validateCustomDomainHostname is guards 1 and 2's PURE half: the shape rules
// that need no database. Split out so the whole policy is testable with no
// database, no environment and no actor.
func validateCustomDomainHostname(hostname, domain string) error {
	host := strings.ToLower(strings.TrimSpace(hostname))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" {
		return fmt.Errorf("v1:platform:customDomain: hostname is required")
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf(
			"v1:platform:customDomain: %q is a wildcard, and a wildcard cannot be bound. The "+
				"certificate for a custom domain is issued over HTTP-01, which cannot issue a "+
				"wildcard at all, and one wildcard dnsName fails the whole ACME order (memql#4224). "+
				"Bind each hostname you want served",
			host)
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf(
			"v1:platform:customDomain: %q is a single label, not a domain. A custom domain is the "+
				"client's own fully qualified host, e.g. www.acme.com",
			host)
	}
	if domain == "" {
		// Unreachable through customDomainPolicyDomain, which never returns
		// empty. Stated anyway: an empty domain would make the suffix test
		// below admit nothing, which is a fail-closed answer that reads to an
		// operator as "binding a domain is broken".
		return fmt.Errorf(
			"v1:platform:customDomain: cannot check %q -- this cluster's own domain did not resolve, "+
				"and admitting a hostname without one would admit every hostname",
			host)
	}
	if host == frontdoor.Apex(domain) || strings.HasSuffix(host, frontdoor.DomainDerivationSuffix(domain)) {
		return fmt.Errorf(
			"v1:platform:customDomain: %q is under this cluster's own domain (%s), which is not a "+
				"custom domain. A hostname there is a DEPLOYABLE's own hostname -- create the site "+
				"with that name instead, and the `*.%s` front-door rule already routes it with no "+
				"DNS record, no certificate and no verification. This flow is for a hostname the "+
				"CLIENT owns",
			host, domain, domain)
	}
	for _, h := range frontdoor.Hosts(domain) {
		if host == strings.ToLower(h.Name) {
			return fmt.Errorf(
				"v1:platform:customDomain: %q is one of this cluster's front-door hosts, which the "+
					"platform serves itself. Binding it would request a certificate for the host the "+
					"cluster's own %s surface answers on",
				host, h.Role)
		}
	}
	return nil
}

// customDomainPolicyDomain is the domain this cluster serves. Same read and
// same fallback as the site hostname policy's, so the two answer consistently
// about what is "ours".
func customDomainPolicyDomain() string {
	domain := strings.TrimSpace(os.Getenv(memqlDomainEnv))
	if domain == "" {
		return defaultSiteDomain
	}
	return strings.ToLower(domain)
}

// validateCustomDomainPolicy runs the three guards plus the terminal-row rule.
//
// Runs on a write that is CLAIMING a hostname -- a create, or an update that
// changes one. Every other write inherits the stored hostname through the
// read-merge, and re-judging an inherited value would mean a binding created
// before a front-door role was added could never be marked removed.
func (e *MemQLEngine) validateCustomDomainPolicy(
	ctx context.Context,
	payload map[string]any,
	mutationId, actor string,
	priorExisted bool,
	priorHostname, priorStatus string,
) error {
	if e == nil || payload == nil {
		return nil
	}

	// The terminal-row rule, first: it is about the row rather than the
	// hostname, so it binds even a write that changes nothing else.
	if priorExisted && customDomainTerminalStatus(priorStatus) {
		next := strings.TrimSpace(stringFromAny(payload["status"]))
		if next != "" && !customDomainTerminalStatus(next) {
			return fmt.Errorf(
				"v1:platform:customDomain: %q is already removed, and a removed binding does not walk "+
					"again. Its row survives as the record of what this cluster served and when "+
					"(nothing deletes it). To serve the hostname again, bind it afresh -- which mints "+
					"a new ownership token, because control of a domain is not something last month's "+
					"verification can still vouch for",
				strings.TrimSpace(priorHostname))
		}
	}

	hostname := strings.ToLower(strings.TrimSpace(stringFromAny(payload["hostname"])))
	if hostname == "" {
		// `hostname` is `string!`; the concept schema refuses an empty one
		// downstream. Don't double-error on someone else's rule.
		return nil
	}
	claiming := !priorExisted || !strings.EqualFold(hostname, strings.TrimSpace(priorHostname))
	if !claiming {
		return nil
	}

	if err := validateCustomDomainHostname(hostname, customDomainPolicyDomain()); err != nil {
		return err
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if isSystemActor(identity, actor) {
		return nil
	}

	// Guard 2's read half: a live site or another live binding already
	// answering on this hostname.
	if holders, err := e.liveSiteIdsForHostname(ctx, hostname); err != nil {
		return fmt.Errorf("v1:platform:customDomain: cannot verify hostname uniqueness for %q: %w", hostname, err)
	} else if len(holders) > 0 {
		return fmt.Errorf(
			"v1:platform:customDomain: hostname %q is already served by deployable %q. A hostname "+
				"resolves to exactly one site -- the edge looks a request's Host up in the graph, so "+
				"a second claim makes which site answers depend on row order",
			hostname, holders[0])
	}

	self := canonicalCustomDomainStorageId(mutationId)
	claimers, err := e.liveCustomDomainIdsForHostname(ctx, hostname)
	if err != nil {
		// Fail CLOSED, for the reason the sibling site guard states: "we could
		// not check" and "there is nothing to find" are different answers, and
		// an unavailable database must not be the way a duplicate hostname
		// gets in.
		return fmt.Errorf("v1:platform:customDomain: cannot verify hostname uniqueness for %q: %w", hostname, err)
	}
	for _, h := range claimers {
		if canonicalCustomDomainStorageId(h) == self {
			continue
		}
		return fmt.Errorf(
			"v1:platform:customDomain: hostname %q is already bound by %q. Remove that binding first "+
				"-- a removed one frees its hostname, and its row survives as history",
			hostname, h)
	}

	// Guard 3: the per-site cap. Only on a CREATE -- an update that renames a
	// hostname does not change how many the site holds, and counting it as one
	// more would refuse the rename of the last binding a site is allowed.
	if priorExisted {
		return nil
	}
	siteId := strings.TrimSpace(stringFromAny(payload["siteId"]))
	if siteId == "" {
		return nil
	}
	max := customDomainMaxPerSite()
	held, err := e.activeCustomDomainCountForSite(ctx, siteId, self)
	if err != nil {
		return fmt.Errorf("v1:platform:customDomain: cannot count existing domains for %q: %w", siteId, err)
	}
	if held >= max {
		return fmt.Errorf(
			"v1:platform:customDomain: deployable %q already has %d custom domain(s), which is this "+
				"cluster's maximum per deployable (%s=%d). The cap is about certificate issuance "+
				"rather than storage: Let's Encrypt's per-registered-domain limit is shared by every "+
				"domain on this cluster, so an unbounded binding loop would stop issuance for all of "+
				"them. Remove a binding, or raise %s",
			siteId, held, customDomainMaxPerSiteEnv, max, customDomainMaxPerSiteEnv)
	}
	return nil
}

// canonicalCustomDomainStorageId converts either spelling of a binding's row id
// into the stored, concept-qualified form. Same reasoning as its site
// counterpart: a mutation carries a BARE id while storage holds the qualified
// one, so a raw comparison would make self-exclusion never match and refuse a
// row as a duplicate of itself.
func canonicalCustomDomainStorageId(rowId string) string {
	trimmed := strings.TrimSpace(rowId)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, conceptPlatformCustomDomain+":") {
		return trimmed
	}
	return id.BuildNodeId(conceptPlatformCustomDomain, trimmed)
}

// liveCustomDomainIdsForHostname returns the ids whose LATEST version claims
// this hostname and is not removed.
//
// TWO STEPS, exactly as liveSiteIdsForHostname does, and for the same reason:
// the store is time-series, so a single `WHERE payload->>'hostname' = ?`
// matches any version that EVER carried the hostname -- including one since
// removed -- and answering from that set would refuse a hostname nothing
// currently holds. Scanning the whole concept instead is unbounded. So step one
// narrows to the rows that have ever carried this hostname (a predicate the
// database answers) and step two resolves only those to their latest version.
//
// staged-data: MUST-NOT-GATE -- the gate CREATES the violation it would then be
// unable to detect. Withhold a staged row and the probe finds nothing, the
// write is admitted, and the cluster now holds two live bindings on one
// hostname, one of which is invisible to the next probe as well. It reads
// deliberately WITHOUT row-authz narrowing for the same reason its site
// counterpart does: a hostname another binding holds must collide even where
// the caller cannot see that row, which is why the message names the id and
// nothing else about it.
func (e *MemQLEngine) liveCustomDomainIdsForHostname(ctx context.Context, hostname string) ([]string, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}
	host := strings.ToLower(strings.TrimSpace(hostname))

	var candidates []string
	err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Column("id").
		Where("concept = ?", conceptPlatformCustomDomain).
		Where("lower(payload->>'hostname') = ?", host).
		Distinct().
		Scan(ctx, &candidates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan custom domain hostname candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	rows, err := e.latestCustomDomainVersions(ctx, candidates)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if strings.ToLower(r.hostname) != host {
			continue // renamed away since; step 1 alone is not an answer
		}
		if customDomainTerminalStatus(r.status) {
			continue // a removed binding frees its hostname
		}
		out = append(out, r.id)
	}
	return out, nil
}

// activeCustomDomainCountForSite counts the bindings a deployable currently
// holds, excluding the row being written and every removed one.
//
// REMOVED ROWS DO NOT COUNT. They survive as history, and a cap that counted
// them would make a site permanently unable to bind a sixth domain because it
// once bound and removed five -- turning an audit trail into a quota.
//
// staged-data: MUST-NOT-GATE -- the gate CREATES the violation it would then be
// unable to detect, exactly as for the hostname probe above. This count is what
// keeps a site under its ACME ceiling; withhold a staged binding and the count
// comes back short, the write is admitted, and the site now holds one more
// domain than the cap allows -- one of which is invisible to the next count as
// well, so the overflow compounds silently. The cap exists because Let's
// Encrypt's per-registered-domain limit is shared by every domain on the
// cluster, so the bug this would cause is issuance failing for domains that
// have nothing to do with the one that overflowed.
//
// The `siteId` predicate is matched in BOTH spellings because the field is an
// outgoing @relationship: canonicalizeRelationshipFields rewrites it to
// `v1:platform:site:<id>` on the way in, while a caller passes the bare id.
// Comparing one spelling would count zero and admit every claim.
func (e *MemQLEngine) activeCustomDomainCountForSite(ctx context.Context, siteId, selfStorageId string) (int, error) {
	db := e.database()
	if db == nil {
		return 0, fmt.Errorf("memory engine database not configured")
	}
	bare := strings.TrimSpace(siteId)
	canonical := id.BuildNodeId(conceptPlatformSite, bare)

	var candidates []string
	err := db.NewSelect().
		Model((*memorynodes.MemoryNode)(nil)).
		Column("id").
		Where("concept = ?", conceptPlatformCustomDomain).
		Where("payload->>'siteId' IN (?)", bun.In([]string{bare, canonical})).
		Distinct().
		Scan(ctx, &candidates)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("scan custom domain candidates for site: %w", err)
	}
	if len(candidates) == 0 {
		return 0, nil
	}

	rows, err := e.latestCustomDomainVersions(ctx, candidates)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.id == selfStorageId {
			continue
		}
		if customDomainTerminalStatus(r.status) {
			continue
		}
		if r.siteId != bare && r.siteId != canonical {
			continue // re-pointed at another deployable since
		}
		n++
	}
	return n, nil
}

// customDomainRow is the narrow projection the two probes above read.
type customDomainRow struct {
	id       string
	siteId   string
	hostname string
	status   string
}

// latestCustomDomainVersions resolves each candidate id to its LATEST version.
//
// staged-data: MUST-NOT-GATE -- it is step two of both probes above and carries
// their verdict. Withholding a staged version here is the same defect one layer
// down and is WORSE, because it does not merely hide a row: it would resolve a
// binding to a STALE version, so a hostname freed by a removal that is still
// staged would read as taken, and a hostname taken by a staged create would
// read as free. Both are the uniqueness guarantee inverted.
func (e *MemQLEngine) latestCustomDomainVersions(ctx context.Context, candidates []string) ([]customDomainRow, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("memory engine database not configured")
	}
	var nodes []memorynodes.MemoryNode
	err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptPlatformCustomDomain).
		Where("id IN (?)", bun.In(candidates)).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan custom domain versions: %w", err)
	}
	// createdAt DESC, so the FIRST row seen for an id is its latest version.
	seen := make(map[string]struct{}, len(candidates))
	out := make([]customDomainRow, 0, len(candidates))
	for _, node := range nodes {
		if _, dup := seen[node.ID]; dup {
			continue
		}
		seen[node.ID] = struct{}{}
		var p map[string]any
		if jerr := json.Unmarshal(node.Payload, &p); jerr != nil {
			continue
		}
		out = append(out, customDomainRow{
			id:       node.ID,
			siteId:   strings.TrimSpace(stringFromAny(p["siteId"])),
			hostname: strings.TrimSpace(stringFromAny(p["hostname"])),
			status:   strings.TrimSpace(stringFromAny(p["status"])),
		})
	}
	return out, nil
}
