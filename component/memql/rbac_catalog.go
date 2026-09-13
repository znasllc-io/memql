package memql

// THE ENGINE'S CAPABILITY CATALOG (epic memql#5166, decision D1).
//
// component/auth declares what a catalog IS (capability_catalog.go); this file
// is the one that reads the rows. It loads v1:rbac:role and v1:rbac:capability
// with the append-only collapse, resolves allow-minus-deny once, keeps the
// result in memory, installs it into component/auth so every Capable call and
// every Can* adapter resolves through it, and reloads on the two concepts'
// graph events so a role created on one replica is real on all of them.
//
// ===========================================================================
// IT IS ALSO THE LADDER, AND THAT IS THE POINT RATHER THAN AN OPTIMISATION
// ===========================================================================
//
// rankLadder(ctx) resolved the same rows independently, for the row gate; this
// resolves them for the data gate. Two resolutions of one catalog can disagree
// -- about an alias, about which version of a re-ranked role is current, about
// whether a deactivated role still ranks -- and the disagreement presents as
// one request being admitted by the row gate and refused by the data gate, or
// the reverse. So rankLadder reads this structure, and the two gates cannot
// hold different opinions about a rank.
//
// ===========================================================================
// A FAILED RELOAD KEEPS THE LAST GOOD SNAPSHOT
// ===========================================================================
//
// The alternative -- clear the pointer, let the compiled mirror answer -- takes
// every custom role's permissions away because the database blinked, mid
// request, on one replica. The log line says the reload failed and the snapshot
// is left alone. A snapshot minutes out of date is a smaller wrong answer than
// a cluster that forgets its own roles.

import (
	"context"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
)

// conceptRbacCapability is the canonical concept id of the capability catalog.
// Its sibling conceptRbacRole lives in rbac_role_immutable_validation.go.
const conceptRbacCapability = "v1:rbac:capability"

// rbacCatalog is ONE immutable resolution of the two catalog concepts.
//
// Immutable is load-bearing: it is published through an atomic pointer and read
// without a lock by every gate on every request, so a reload builds a fresh one
// and swaps rather than editing this in place.
type rbacCatalog struct {
	// ranks carries the slug AND every alias, so one lookup answers both
	// vocabularies. canonical maps an alias back to the slug that names its
	// rung, which is what lets Holds / Active / Name / Scope / Grants take
	// either spelling -- the rule component/auth's interface states.
	ranks     map[string]int
	canonical map[string]string
	names     map[string]string
	scopes    map[string]string
	active    map[string]bool
	// grants is the RESOLVED allow set per slug: every active allow row whose
	// (verb, resource) carries no active deny row. Resolved once here so no
	// gate downstream has to remember that deny wins.
	grants map[string]map[auth.VerbResource]bool
	// roleCount and capabilityCount are what the boot line reports. Kept
	// because "the catalog installed" and "the catalog installed and is empty"
	// are different facts and only one of them is fine.
	roleCount       int
	capabilityCount int
}

// Rank resolves a slug or alias to its rung.
func (c *rbacCatalog) Rank(slug string) (int, bool) {
	if c == nil {
		return 0, false
	}
	rank, ok := c.ranks[normalizeCatalogSlug(slug)]
	return rank, ok
}

// Name is the display name of the rung a slug or alias names.
func (c *rbacCatalog) Name(slug string) string {
	if c == nil {
		return ""
	}
	return c.names[c.resolve(slug)]
}

// Holds is the decision. False for an unknown slug and false for every pair of
// a deactivated role -- D8 says a deactivated role keeps its rung and loses its
// grants, which is what makes deactivation different from deletion.
func (c *rbacCatalog) Holds(slug, verb, resource string) bool {
	if c == nil {
		return false
	}
	canonical := c.resolve(slug)
	if canonical == "" || !c.active[canonical] {
		return false
	}
	return c.grants[canonical][auth.VerbResource{Verb: verb, Resource: resource}]
}

// Grants is the resolved allow set, sorted so a refusal naming a missing pair
// names the same one every time. Empty for a deactivated role, for Holds'
// reason: the subset guard asks what a caller may hand out, and a caller whose
// own role is deactivated may hand out nothing.
func (c *rbacCatalog) Grants(slug string) []auth.VerbResource {
	if c == nil {
		return nil
	}
	canonical := c.resolve(slug)
	if canonical == "" || !c.active[canonical] {
		return nil
	}
	out := make([]auth.VerbResource, 0, len(c.grants[canonical]))
	for vr := range c.grants[canonical] {
		out = append(out, vr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Resource != out[j].Resource {
			return out[i].Resource < out[j].Resource
		}
		return out[i].Verb < out[j].Verb
	})
	return out
}

// Scope is the account a role is scoped to (D10), or "" for a global one.
func (c *rbacCatalog) Scope(slug string) string {
	if c == nil {
		return ""
	}
	return c.scopes[c.resolve(slug)]
}

// Active reports the lifecycle flag of the rung a slug or alias names.
func (c *rbacCatalog) Active(slug string) bool {
	if c == nil {
		return false
	}
	canonical := c.resolve(slug)
	return canonical != "" && c.active[canonical]
}

// resolve turns a slug or alias into the canonical slug, or "" when the catalog
// carries neither.
func (c *rbacCatalog) resolve(slug string) string {
	return c.canonical[normalizeCatalogSlug(slug)]
}

// ladder projects the catalog as the roleLadder the row gate reads. One
// structure, two readers -- see this file's header.
//
// A DEACTIVATED ROLE IS NOT IN IT, and that is the record's sentence rather
// than an efficiency: "the resolver then treats the holder as unknown: nothing,
// everywhere, until re-roled" (section I). A rung left in the ladder would keep
// ranking its holders after their role was retired, so they would still clear
// every @requiresRank floor and still be visible to their old peers under
// rankVisible -- holding no capability at all. Half-retired is the one state
// this must not produce.
//
// The catalog itself still CARRIES the role: Rank answers for it, so the role
// builtins can refuse a create that reuses a retired slug or a retired rank.
// A catalog FACT and an authorization ANSWER are different questions, and this
// is where they part.
func (c *rbacCatalog) ladder() roleLadder {
	if c == nil {
		return roleLadder{ranks: map[string]int{}}
	}
	ranks := make(map[string]int, len(c.ranks))
	for slug, rank := range c.ranks {
		if !c.active[c.canonical[slug]] {
			continue
		}
		ranks[slug] = rank
	}
	return roleLadder{ranks: ranks}
}

// CanonicalSlug resolves a slug or alias to the slug that names its rung, or ""
// when the catalog carries neither.
//
// Exported for the role builtins' holder count: every ordinary principal's user
// row spells the member tier `writer`, so a count that compared the catalog
// slug `user` alone would report zero holders and let the rung be retired out
// from under everybody.
func (c *rbacCatalog) CanonicalSlug(slug string) string {
	if c == nil {
		return ""
	}
	return c.resolve(slug)
}

// Slugs lists every slug the catalog carries, ACTIVE OR NOT, sorted.
//
// The role builtins read it for the two "taken" guards: a retired role keeps
// its slug and its rung (D8 -- deactivate, never delete), so a create that
// reuses either is refused. Every other reader wants the authorization answer
// and must not use this.
func (c *rbacCatalog) Slugs() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.names))
	for slug := range c.names {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------
// building
// ---------------------------------------------------------------------

// buildRbacCatalog resolves role and capability rows into one snapshot.
//
// The rows arrive newest-first per id (the SQL does `DISTINCT ON (id) ORDER BY
// id, "createdAt" DESC`), and this ALSO collapses by id itself rather than
// trusting that: the same builder is handed hand-built rows by the tests, and a
// builder that took the last row it saw would invert the answer silently if the
// ORDER BY were ever edited.
func buildRbacCatalog(roles, capabilities []memorynodes.MemoryNode) *rbacCatalog {
	cat := &rbacCatalog{
		ranks:     map[string]int{},
		canonical: map[string]string{},
		names:     map[string]string{},
		scopes:    map[string]string{},
		active:    map[string]bool{},
		grants:    map[string]map[auth.VerbResource]bool{},
	}

	// Alias claims are applied in a SECOND pass, and that is the
	// security-relevant part rather than tidiness. Applied inline, "a slug
	// already taken wins" would depend on ITERATION ORDER: rows come back
	// newest-first, so a role created today could claim an alias whose base
	// role had not been read yet. `writer` and `reader` are alias-ONLY rungs --
	// no row carries either as a slug -- so nothing would ever reclaim them,
	// and a developer minting a rank-299 role aliased `reader` would promote
	// every reader in the cluster to 299. The rank-bound guard bounds only the
	// `rank` field and never looks at `aliases`.
	//
	// AND PREDEFINED CLAIMS ARE APPLIED BEFORE CUSTOM ONES, which the
	// deferral alone does not buy. Deferring makes a SLUG beat an alias; it
	// leaves alias-versus-alias decided by whatever order the claims are
	// visited in. `writer` and `reader` are alias-ONLY rungs -- no row carries
	// either as a slug -- so there is no slug to win, and a custom role
	// claiming `reader` takes it from `viewer` outright. That is the
	// rank-299 escalation spelled out above, and it survives the deferral.
	// Ordering predefined first closes it: the seeded catalog's identity is
	// not re-pointable by a row anybody can author.
	type aliasClaim struct {
		names      []string
		slug       string
		predefined bool
	}
	var pendingAliases []aliasClaim

	seenRole := map[string]struct{}{}
	for i := range roles {
		id := strings.TrimSpace(roles[i].ID)
		if _, dup := seenRole[id]; dup && id != "" {
			continue
		}
		payload := rankRowPayload(roles[i])
		if payload == nil {
			continue
		}
		slug := normalizeCatalogSlug(stringFromAny(payload["slug"]))
		if slug == "" {
			continue
		}
		if id != "" {
			seenRole[id] = struct{}{}
		}
		if _, dup := cat.canonical[slug]; dup {
			continue
		}
		rank, ok := intWithOkFromAny(payload["rank"])
		if !ok {
			continue
		}
		cat.roleCount++
		cat.ranks[slug] = rank
		cat.canonical[slug] = slug
		cat.names[slug] = strings.TrimSpace(stringFromAny(payload["name"]))
		cat.scopes[slug] = strings.TrimSpace(stringFromAny(payload["accountId"]))
		// ACTIVE DEFAULTS TRUE for a row that does not carry the field. The
		// concept declares @default("true") and a default is not applied on
		// insert, so a row written before the field existed carries no value --
		// and reading that as "deactivated" would take every grant away from a
		// role nobody deactivated.
		cat.active[slug] = true
		if v, present := payload["active"].(bool); present {
			cat.active[slug] = v
		}
		if aliases := rankAliasList(payload["aliases"]); len(aliases) > 0 {
			predefined, _ := payload["predefined"].(bool)
			pendingAliases = append(pendingAliases, aliasClaim{
				names: aliases, slug: slug, predefined: predefined,
			})
		}
	}

	// Predefined first, then by slug. The second key is what makes a cluster
	// where two CUSTOM roles claim one alias resolve the same way on every
	// replica: map order would make the winner a coin flip, and a coin-flipped
	// rank is two replicas disagreeing about who outranks whom.
	sort.SliceStable(pendingAliases, func(i, j int) bool {
		if pendingAliases[i].predefined != pendingAliases[j].predefined {
			return pendingAliases[i].predefined
		}
		return pendingAliases[i].slug < pendingAliases[j].slug
	})
	for _, claim := range pendingAliases {
		for _, alias := range claim.names {
			alias = normalizeCatalogSlug(alias)
			if alias == "" {
				continue
			}
			if _, taken := cat.canonical[alias]; taken {
				continue
			}
			cat.canonical[alias] = claim.slug
			cat.ranks[alias] = cat.ranks[claim.slug]
		}
	}

	// Capabilities: collect the active allows and the active denies, then
	// subtract. Two passes rather than one because a deny may be read before
	// the allow it overrides, and an allow arriving afterwards would otherwise
	// reinstate the pair.
	allows := map[string]map[auth.VerbResource]bool{}
	denies := map[string]map[auth.VerbResource]bool{}
	seenCap := map[string]struct{}{}
	for i := range capabilities {
		id := strings.TrimSpace(capabilities[i].ID)
		if _, dup := seenCap[id]; dup && id != "" {
			continue
		}
		payload := rankRowPayload(capabilities[i])
		if payload == nil {
			continue
		}
		if id != "" {
			seenCap[id] = struct{}{}
		}
		if v, present := payload["active"].(bool); present && !v {
			continue
		}
		slug := normalizeCatalogSlug(stringFromAny(payload["roleSlug"]))
		verb := strings.TrimSpace(stringFromAny(payload["verb"]))
		resource := strings.TrimSpace(stringFromAny(payload["resourceType"]))
		if slug == "" || verb == "" || resource == "" {
			continue
		}
		cat.capabilityCount++
		target := allows
		// EFFECT DEFAULTS ALLOW, matching the concept's @default and the
		// overwhelming common case. Only the literal "deny" revokes.
		if strings.TrimSpace(stringFromAny(payload["effect"])) == "deny" {
			target = denies
		}
		if target[slug] == nil {
			target[slug] = map[auth.VerbResource]bool{}
		}
		target[slug][auth.VerbResource{Verb: verb, Resource: resource}] = true
	}
	for slug, set := range allows {
		resolved := map[auth.VerbResource]bool{}
		for vr := range set {
			if denies[slug][vr] {
				continue
			}
			resolved[vr] = true
		}
		cat.grants[slug] = resolved
	}

	return cat
}

// normalizeCatalogSlug is the fold applied to every slug the rows carry.
// Trim and lowercase, matching component/auth's normalizeSlug -- the two must
// agree or a slug written one way would resolve here and not there.
func normalizeCatalogSlug(slug string) string {
	return strings.ToLower(strings.TrimSpace(slug))
}

// ---------------------------------------------------------------------
// loading and the reload
// ---------------------------------------------------------------------

// loadRbacCatalog reads both concepts and builds a snapshot.
//
// staged-data: MUST-NOT-GATE, for rankLadder's reason and one more. A staged
// v1:rbac:role row excluded here does not resolve, so every principal holding
// that role ranks BELOW everyone and holds NOTHING -- their colleagues stop
// seeing their rows, every rank floor refuses them, and every capability gate
// refuses them. Withholding a role from the CATALOG hides nothing (the catalog
// is public reference data by declaration) and breaks everything that reads it.
func (e *MemQLEngine) loadRbacCatalog(ctx context.Context) (*rbacCatalog, error) {
	db := e.database()
	if db == nil {
		return nil, errNoDatabaseForCatalog
	}
	var roles []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&roles).
		DistinctOn("id").
		Where("concept = ?", conceptRbacRole).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, err
	}
	var capabilities []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&capabilities).
		DistinctOn("id").
		Where("concept = ?", conceptRbacCapability).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, err
	}
	return buildRbacCatalog(roles, capabilities), nil
}

// errNoDatabaseForCatalog is what a node with no database reports. It is a
// legitimate state (a test engine, a node type built without one), not a
// failure, so the boot line says "mirror" rather than "error".
var errNoDatabaseForCatalog = catalogError("no database")

// errCatalogHasNoRoles is a catalog that READ CLEANLY and carried no role
// rows.
//
// Distinct from errNoDatabaseForCatalog because the two produce identical
// behaviour from different causes, and the boot line has to name which: no
// database is a node type built without one, whereas no roles is the seeds not
// having landed yet -- or having failed to land, which is the case an operator
// needs to go and look at.
var errCatalogHasNoRoles = catalogError("catalog carried no roles")

type catalogError string

func (e catalogError) Error() string { return string(e) }

// CapabilityCatalogSnapshot returns this engine's current snapshot, or nil
// before the first successful load. Exported for the role builtins, which need
// the taken slugs and ranks and cannot ask component/auth for them.
func (e *MemQLEngine) CapabilityCatalogSnapshot() *rbacCatalog {
	if e == nil {
		return nil
	}
	return e.rbacCatalog.Load()
}

// ReloadCapabilityCatalog re-reads both concepts and republishes the snapshot.
//
// On failure it keeps the snapshot it has -- see the header. Returns the error
// so a caller that wants to report it can; the event handler logs and continues.
func (e *MemQLEngine) ReloadCapabilityCatalog(ctx context.Context) error {
	cat, err := e.loadRbacCatalog(ctx)
	if err != nil {
		return err
	}
	// A CATALOG CARRYING NO ROLES IS NOT A CLUSTER WITH NO ROLES -- it is the
	// rows not being readable yet, and installing it is a cluster-wide
	// authoring lockout for as long as that window lasts.
	//
	// StartCapabilityCatalog runs from the engine's Start while the seed
	// materializer that WRITES v1:rbac:role is gated on <-engine.Ready()
	// (app/engine.go), so every fresh-database boot reaches here with zero
	// rows. component/auth's roleHasCapability short-circuits the compiled
	// mirror the moment a catalog is installed -- `if held, answered :=
	// catalogHolds(...); answered { return held }` -- so an empty catalog
	// ANSWERS false for every role, the OWNER INCLUDED, at every Capable call
	// site. That is six gates at once, the D9 DSL-deploy gate among them, all
	// refusing under messages that name a role the caller already holds.
	//
	// Refusing to install it is the same answer this file already gives a
	// failed read (see the header): keep the last good snapshot, or let the
	// mirror answer at boot. Only the sentence the operator is told differs,
	// which is why this is its own error rather than a silent skip.
	//
	// It deliberately does NOT cover a catalog that carries roles with `owner`
	// DEACTIVATED. "A deactivated role answers nothing, everywhere" is the
	// intended semantics component/auth's Active() states, and deactivating a
	// base role is a deliberate act rather than a boot-order accident.
	return e.installCapabilityCatalog(cat)
}

// installCapabilityCatalog publishes a snapshot, or refuses it.
//
// Split out of ReloadCapabilityCatalog so the refusal above is reachable in a
// test without a database: loadRbacCatalog needs one, and the guard is the
// half worth pinning.
func (e *MemQLEngine) installCapabilityCatalog(cat *rbacCatalog) error {
	// capabilityCount is guarded for the SAME reason as roleCount, and it is
	// the likelier of the two: the reload fires on FOUR topics -- role and
	// capability, created and updated -- so the first role row to land
	// triggers a reload that reads the roles it can see and NO capability
	// rows at all. A role-only snapshot installs cleanly and then answers
	// false for every (verb, resource) pair of every role, which is the
	// authoring lockout again wearing a different number.
	if cat == nil || cat.roleCount == 0 || cat.capabilityCount == 0 {
		return errCatalogHasNoRoles
	}
	e.rbacCatalog.Store(cat)
	auth.SetCapabilityCatalog(cat)
	return nil
}

// rbacCatalogTopics are the four events that change the catalog.
//
// There is no `deleted` topic, and its absence is not an oversight: a role is
// DEACTIVATED, never deleted (D8), so the change arrives as an update. A rule
// for an event nothing emits would be a rule nobody could tell was broken.
var rbacCatalogTopics = []string{
	"graph.node.created." + conceptRbacRole,
	"graph.node.updated." + conceptRbacRole,
	"graph.node.created." + conceptRbacCapability,
	"graph.node.updated." + conceptRbacCapability,
}

// StartCapabilityCatalog loads the catalog, installs it into component/auth,
// and keeps it current.
//
// WITHOUT THE RELOAD HALF THIS FEATURE IS WORSE THAN ABSENT, which is why it is
// wired here beside the cache-invalidation and providers-reload subscribers
// rather than left to a restart. A role created on replica A would hold its
// grants there and nothing on replica B, so the person who created it sees it
// work on one page and refuse on the next, with both replicas reporting healthy
// -- the same shape as the provider-auth split that rule exists for.
//
// Scoped to the engine lifecycle context: the subscriptions are torn down when
// ctx is cancelled.
func (e *MemQLEngine) StartCapabilityCatalog(ctx context.Context) {
	if e == nil {
		return
	}

	// The grant and membership sources beside the catalog (epic memql#5296).
	// No load and no reload: grants are read per request, so installing the
	// reader is the whole of it.
	e.InstallGrantResolution()

	if err := e.ReloadCapabilityCatalog(ctx); err != nil {
		// THE BOOT LINE, and it says which mode is in force rather than only
		// that something failed. "The catalog is unreadable" and "the catalog
		// is readable and empty" produce identical behaviour for a custom role
		// and completely different behaviour for a base one, so an operator
		// reading this needs to be told which they have.
		if e.Component != nil && e.Logger != nil {
			reason := "unreadable"
			if err == errCatalogHasNoRoles {
				reason = "empty"
			}
			e.Logger.Warn("rbac catalog "+reason+" -- base roles answer from the compiled mirror "+
				"and custom roles hold nothing until the rows load",
				"component", "memql.engine",
				"mode", "mirror",
				"reason", reason,
				"err", err.Error())
		}
	} else if e.Component != nil && e.Logger != nil {
		cat := e.rbacCatalog.Load()
		e.Logger.Info("rbac catalog installed -- every capability decision now resolves through the rows",
			"component", "memql.engine",
			"mode", "rows",
			"roles", cat.roleCount,
			"capabilities", cat.capabilityCount)
	}

	if e.eventBus == nil {
		return
	}
	unsubscribes := make([]func(), 0, len(rbacCatalogTopics))
	for _, topic := range rbacCatalogTopics {
		unsubscribe := e.eventBus.Subscribe(
			topic,
			func(events.Event) {
				if err := e.ReloadCapabilityCatalog(ctx); err != nil && e.Component != nil && e.Logger != nil {
					e.Logger.Warn("rbac catalog reload failed -- keeping the last good snapshot",
						"component", "memql.engine", "err", err.Error())
				}
			},
			events.WithSubscriberName("memql:rbacCatalog"),
		)
		if unsubscribe != nil {
			unsubscribes = append(unsubscribes, unsubscribe)
		}
	}
	if len(unsubscribes) == 0 {
		return
	}
	go func() {
		<-ctx.Done()
		for _, unsubscribe := range unsubscribes {
			unsubscribe()
		}
	}()
}
