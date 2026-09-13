package rbac

// ROLE AUTHORING (epic memql#5166, sections D, F and H).
//
// The three builtins that make a custom role real. They are Go rather than
// mutations because every guard they apply is relational or set-valued, and a
// mutation body can express neither: "strictly below the CALLER'S rank",
// "equal to no rung this cluster already has", "every grant one the CALLER
// holds". `createRole` and `createCapability` are @serverOnly precisely so this
// is the only way in.
//
// THE ORDER OF THE GUARDS IS PART OF THE CONTRACT. Each returns its own code,
// and a caller renders the refusal it got rather than parsing a sentence; a
// guard that ran later would report a different reason for the same call, and
// a client offering the operator a fix would offer the wrong one.
//
// A NIL CATALOG REFUSES EVERY CREATE. Authoring a role against the compiled
// mirror would let a slug the rows already hold be minted again, and would
// resolve the caller's grant set from five hardcoded profiles rather than from
// what the cluster says they hold. "The catalog has not loaded" is a state that
// passes in seconds; a duplicate role is forever.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// Refusal codes, exactly as the design record's table names them.
const (
	codeSlugTaken           = "role_slug_taken"
	codeRankNotBelowCaller  = "role_rank_not_below_caller"
	codeRankTaken           = "role_rank_taken"
	codeGrantNotHeld        = "role_grant_not_held"
	codePredefinedImmutable = "role_predefined_immutable"
	codeHeldAboveCaller     = "role_held_above_caller"
	codeHeld                = "role_held"
	codeNotAuthorized       = "role_not_authorized"
	codeCatalogUnavailable  = "role_catalog_unavailable"
	codeInvalidSlug         = "role_slug_invalid"
	codeUnknownRole         = "role_not_found"
	codeInvalidGrant        = "role_grant_invalid"
)

// roleSlugPattern is the record's shape: lowercase, starts with a letter, 2 to
// 40 characters. Narrow on purpose -- a slug is an identity that lands on user
// rows, in aliases and in every client's ladder, and one that differs from
// another only by case or punctuation is a role people will confuse.
var roleSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)

// roleCatalogReader is the slice of the engine's catalog these handlers need.
// Declared here rather than taken as *memql.MemQLEngine so the handlers are
// testable against a hand-built catalog -- the guards are the whole product of
// this file and a test that cannot reach them tests nothing.
type roleCatalogReader interface {
	Rank(slug string) (int, bool)
	Name(slug string) string
	Holds(slug, verb, resource string) bool
	Grants(slug string) []componentAuth.VerbResource
	Scope(slug string) string
	Active(slug string) bool
	// Slugs lists every slug the catalog carries, active or not -- the
	// rank-taken and slug-taken guards both read it, and both must see a
	// RETIRED role, whose name and rung stay claimed (D8).
	Slugs() []string
	// CanonicalSlug resolves an alias to the slug naming its rung. The holder
	// count needs it: a user row spells the member tier `writer` while the
	// catalog seeds it as `user`.
	CanonicalSlug(slug string) string
}

// roleDecision is what every handler returns, before it becomes a node.
type roleDecision struct {
	ok   bool
	slug string
	code string
	// detail is appended to the operator-facing message: the pair a grant
	// refusal names, the count a held refusal reports. Empty otherwise.
	detail string
}

func refuse(slug, code, detail string) roleDecision {
	return roleDecision{slug: slug, code: code, detail: detail}
}

// ---------------------------------------------------------------------
// roleCreate
// ---------------------------------------------------------------------

func (i *Integration) handleRoleCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return decisionNodes(refuse("", codeNotAuthorized, "this call carries no caller identity")), nil
	}
	cat := i.catalog()
	if cat == nil {
		return decisionNodes(refuse("", codeCatalogUnavailable,
			"the role catalog has not loaded on this node yet")), nil
	}

	slug := strings.ToLower(strings.TrimSpace(stringArg(args, "slug")))
	name := strings.TrimSpace(stringArg(args, "name"))
	rank := intArg(args, "rank")
	accountId := strings.TrimSpace(stringArg(args, "accountId"))

	// 1. The capability. A builtin carries no @requiresRank, so the floor is
	//    repeated here -- and it is `create` on `role` rather than a rank,
	//    because "may you author roles at all" is a grant and not a rung.
	//    Asked of the ACTOR, not the role (epic memql#5296): the subject is
	//    the verified caller plus their groups, so a grant naming them
	//    widens or narrows this exactly as it does at every other gate.
	subject, _ := componentAuth.SubjectFromContext(ctx)
	if !componentAuth.CapableFor(ctx, subject, componentAuth.VerbCreate, componentAuth.ResourceRole) {
		return decisionNodes(refuse(slug, codeNotAuthorized,
			"your role does not hold create on role")), nil
	}
	// 2. The slug: shape first, then taken.
	if !roleSlugPattern.MatchString(slug) {
		return decisionNodes(refuse(slug, codeInvalidSlug,
			"a slug is lowercase, starts with a letter and is 2 to 40 characters of a-z, 0-9 and -")), nil
	}
	if _, taken := cat.Rank(slug); taken {
		// Taken by a SLUG or by an ALIAS, and by a deactivated role as well as
		// a live one: D8 keeps a retired role as history, and reusing its name
		// would silently re-point every user row that still carries it.
		return decisionNodes(refuse(slug, codeSlugTaken,
			"another role already uses that name or claims it as an alias")), nil
	}
	// 3. The rank: below the caller, and not already a rung.
	if d := checkRank(cat, access, rank, slug); d.code != "" {
		return decisionNodes(d), nil
	}
	// 4. The grants: every pair one the caller holds.
	grants, d := parseGrants(args["grants"])
	if d.code != "" {
		d.slug = slug
		return decisionNodes(d), nil
	}
	if len(grants) == 0 {
		return decisionNodes(refuse(slug, codeInvalidGrant,
			"a role with no grants holds nothing; give it at least one permission")), nil
	}
	if d := checkGrantsHeld(cat, access, grants, slug); d.code != "" {
		return decisionNodes(d), nil
	}

	if err := i.writeRole(ctx, roleWrite{
		slug: slug, name: name, rank: rank,
		description: strings.TrimSpace(stringArg(args, "description")),
		accountId:   accountId,
		grants:      grants,
	}); err != nil {
		return nil, err
	}
	i.auditRole(ctx, access, "role_created", slug, map[string]any{
		"rank":      rank,
		"name":      name,
		"accountId": accountId,
		"grants":    grantStrings(grants),
	})
	return decisionNodes(roleDecision{ok: true, slug: slug}), nil
}

// ---------------------------------------------------------------------
// roleUpdate
// ---------------------------------------------------------------------

func (i *Integration) handleRoleUpdate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return decisionNodes(refuse("", codeNotAuthorized, "this call carries no caller identity")), nil
	}
	cat := i.catalog()
	if cat == nil {
		return decisionNodes(refuse("", codeCatalogUnavailable,
			"the role catalog has not loaded on this node yet")), nil
	}

	slug := strings.ToLower(strings.TrimSpace(stringArg(args, "slug")))
	subject, _ := componentAuth.SubjectFromContext(ctx)
	if !componentAuth.CapableFor(ctx, subject, componentAuth.VerbUpdate, componentAuth.ResourceRole) {
		return decisionNodes(refuse(slug, codeNotAuthorized,
			"your role does not hold update on role")), nil
	}
	current, err := i.readRole(ctx, slug)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return decisionNodes(refuse(slug, codeUnknownRole, "no role by that name")), nil
	}
	if current.predefined {
		// D7. A base role's name, rank, grants and aliases refuse every change:
		// they are authored in dsl/rbac/seeds.memql and re-materialized on
		// every boot, so an edit here would be undone at the next restart even
		// if it were allowed -- which is the smaller of the two problems.
		return decisionNodes(refuse(slug, codePredefinedImmutable,
			"a predefined role is authored in the engine's seeds and cannot be edited")), nil
	}

	changed := []string{}
	next := *current

	if v, present := presentString(args, "name"); present {
		next.name, changed = v, append(changed, "name")
	}
	if v, present := presentString(args, "description"); present {
		next.description, changed = v, append(changed, "description")
	}
	if v, present := presentString(args, "accountId"); present {
		next.accountId, changed = v, append(changed, "accountId")
	}
	if raw, present := args["rank"]; present && raw != nil {
		rank := intArg(args, "rank")
		if rank != current.rank {
			if d := checkRank(cat, access, rank, slug); d.code != "" {
				return decisionNodes(d), nil
			}
			// A RANK CHANGE RE-RANKS THE PEOPLE ON THE ROLE, which is why it
			// carries a guard a create does not need. Moving a role above the
			// caller's own rung would promote every holder past them, through
			// an edit that never names a person.
			holders, err := i.holdersOf(ctx, slug)
			if err != nil {
				return nil, err
			}
			callerRank := componentAuth.RoleRank(access.Role)
			if len(holders) > 0 && rank >= callerRank {
				return decisionNodes(refuse(slug, codeHeldAboveCaller, fmt.Sprintf(
					"%d %s currently hold this role, and the new rank is not below your own",
					len(holders), plural(len(holders), "person", "people")))), nil
			}
			next.rank, changed = rank, append(changed, "rank")
		}
	}

	grants := current.grants
	if raw, present := args["grants"]; present && raw != nil {
		parsed, d := parseGrants(raw)
		if d.code != "" {
			d.slug = slug
			return decisionNodes(d), nil
		}
		if d := checkGrantsHeld(cat, access, parsed, slug); d.code != "" {
			return decisionNodes(d), nil
		}
		grants, changed = parsed, append(changed, "grants")
	}

	if len(changed) == 0 {
		return decisionNodes(roleDecision{ok: true, slug: slug}), nil
	}

	if err := i.writeRoleUpdate(ctx, next, grants, current.grants); err != nil {
		return nil, err
	}
	sort.Strings(changed)
	i.auditRole(ctx, access, "role_updated", slug, map[string]any{
		"changed": changed,
		"rank":    next.rank,
		"grants":  grantStrings(grants),
	})
	return decisionNodes(roleDecision{ok: true, slug: slug}), nil
}

// ---------------------------------------------------------------------
// roleDeactivate
// ---------------------------------------------------------------------

func (i *Integration) handleRoleDeactivate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	access, _ := componentAuth.AccessFromContext(ctx)
	if access == nil {
		return decisionNodes(refuse("", codeNotAuthorized, "this call carries no caller identity")), nil
	}
	slug := strings.ToLower(strings.TrimSpace(stringArg(args, "slug")))
	subject, _ := componentAuth.SubjectFromContext(ctx)
	if !componentAuth.CapableFor(ctx, subject, componentAuth.VerbUpdate, componentAuth.ResourceRole) {
		return decisionNodes(refuse(slug, codeNotAuthorized,
			"your role does not hold update on role")), nil
	}
	current, err := i.readRole(ctx, slug)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return decisionNodes(refuse(slug, codeUnknownRole, "no role by that name")), nil
	}
	if current.predefined {
		return decisionNodes(refuse(slug, codePredefinedImmutable,
			"a predefined role is authored in the engine's seeds and cannot be retired")), nil
	}

	// D8. A role held by anybody refuses retirement, and the refusal reports
	// the COUNT rather than only the fact: the operator's next step is to
	// re-role that many people, and a refusal that does not say how many sends
	// them looking through the whole user list.
	holders, err := i.holdersOf(ctx, slug)
	if err != nil {
		return nil, err
	}
	pending, err := i.pendingInvitationsFor(ctx, slug)
	if err != nil {
		return nil, err
	}
	if total := len(holders) + pending; total > 0 {
		return decisionNodes(refuse(slug, codeHeld, fmt.Sprintf(
			"%d %s still hold it or are invited to it; re-role them first",
			total, plural(total, "person", "people")))), nil
	}

	if err := i.writeRoleDeactivation(ctx, *current); err != nil {
		return nil, err
	}
	i.auditRole(ctx, access, "role_deactivated", slug, map[string]any{"rank": current.rank})
	return decisionNodes(roleDecision{ok: true, slug: slug}), nil
}

// ---------------------------------------------------------------------
// the guards, extracted so create and update apply the same ones
// ---------------------------------------------------------------------

// checkRank applies the two rank rules: strictly below the caller (D5's first
// half) and equal to no existing rung (D5's second).
func checkRank(cat roleCatalogReader, access *componentAuth.AccessContext, rank int, slug string) roleDecision {
	callerRank := componentAuth.RoleRank(access.Role)
	if rank >= callerRank {
		return refuse(slug, codeRankNotBelowCaller, fmt.Sprintf(
			"a role you author must rank below your own (%d)", callerRank))
	}
	if rank <= 0 {
		// Rank 0 is what an UNKNOWN role resolves to, so a role authored there
		// would be indistinguishable from one the catalog does not carry.
		return refuse(slug, codeRankTaken, "rank 0 is what an unrecognised role resolves to")
	}
	for _, existing := range cat.Slugs() {
		if existing == slug {
			continue
		}
		if r, ok := cat.Rank(existing); ok && r == rank {
			// D5. Two roles at one rank are peers, and the rank rules -- "read
			// what your rank covers", "write strictly below you" -- have
			// nothing to say about which of two peers outranks the other.
			return refuse(slug, codeRankTaken, fmt.Sprintf("%q already ranks %d", existing, rank))
		}
	}
	return roleDecision{}
}

// checkGrantsHeld applies D6: you cannot hand out what you do not hold.
//
// It names the FIRST pair the caller lacks, sorted, so the same call refuses
// with the same message every time -- an operator fixing a five-grant role one
// refusal at a time needs the order to be stable or they cannot tell progress
// from churn.
func checkGrantsHeld(cat roleCatalogReader, access *componentAuth.AccessContext, grants []componentAuth.VerbResource, slug string) roleDecision {
	sorted := append([]componentAuth.VerbResource(nil), grants...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Resource != sorted[b].Resource {
			return sorted[a].Resource < sorted[b].Resource
		}
		return sorted[a].Verb < sorted[b].Verb
	})
	for _, g := range sorted {
		if !cat.Holds(string(access.Role), g.Verb, g.Resource) {
			return refuse(slug, codeGrantNotHeld, fmt.Sprintf(
				"you do not hold %s on %s, so you cannot grant it", g.Verb, g.Resource))
		}
	}
	return roleDecision{}
}

// parseGrants reads the `[]object` grants argument into pairs.
//
// An unreadable entry is REFUSED rather than skipped. A silently dropped grant
// is a role that holds less than the person who authored it believes, which
// they discover the first time somebody on that role is refused something.
func parseGrants(raw any) ([]componentAuth.VerbResource, roleDecision) {
	list, ok := raw.([]any)
	if !ok {
		if raw == nil {
			return nil, roleDecision{}
		}
		return nil, refuse("", codeInvalidGrant, "grants must be a list of { verb, resource }")
	}
	seen := map[componentAuth.VerbResource]bool{}
	out := make([]componentAuth.VerbResource, 0, len(list))
	for idx, entry := range list {
		obj, ok := entry.(map[string]any)
		if !ok {
			return nil, refuse("", codeInvalidGrant, fmt.Sprintf("grant %d is not an object", idx+1))
		}
		verb := strings.TrimSpace(stringArg(obj, "verb"))
		resource := strings.TrimSpace(stringArg(obj, "resource"))
		if verb == "" || resource == "" {
			return nil, refuse("", codeInvalidGrant, fmt.Sprintf(
				"grant %d needs both a verb and a resource", idx+1))
		}
		if !knownVerb(verb) {
			return nil, refuse("", codeInvalidGrant, fmt.Sprintf(
				"%q is not one of the five verbs (read, create, update, delete, execute)", verb))
		}
		pair := componentAuth.VerbResource{Verb: verb, Resource: resource}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		out = append(out, pair)
	}
	return out, roleDecision{}
}

func knownVerb(v string) bool {
	switch v {
	case componentAuth.VerbRead, componentAuth.VerbCreate, componentAuth.VerbUpdate,
		componentAuth.VerbDelete, componentAuth.VerbExecute:
		return true
	}
	return false
}

func grantStrings(grants []componentAuth.VerbResource) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, g.Verb+":"+g.Resource)
	}
	sort.Strings(out)
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// presentString reports whether a string argument was SUPPLIED, distinguishing
// an explicit empty string from an absent key. roleUpdate needs the difference:
// an absent accountId leaves the scope alone, an empty one clears it.
func presentString(args map[string]any, key string) (string, bool) {
	raw, present := args[key]
	if !present || raw == nil {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// ---------------------------------------------------------------------
// the writes
// ---------------------------------------------------------------------

type roleWrite struct {
	slug        string
	name        string
	rank        int
	description string
	accountId   string
	grants      []componentAuth.VerbResource
}

// roleRow is the read-back of a role, for the update and deactivate paths.
type roleRow struct {
	slug        string
	name        string
	rank        int
	description string
	accountId   string
	predefined  bool
	aliases     []string
	grants      []componentAuth.VerbResource
}

// writeRole writes the role row and one capability row per grant, in one
// internal-origin write.
//
// INTERNAL ORIGIN, because createRole and createCapability are @serverOnly:
// this handler IS the guarded path they are reserved for. The caller's access
// context is kept, so `createdBy` on the row is the person who authored the
// role rather than the engine.
func (i *Integration) writeRole(ctx context.Context, w roleWrite) error {
	writeCtx := componentAuth.ContextWithInternalOrigin(ctx)
	if err := i.exec(writeCtx, roleInsert(w.slug, w.name, w.rank, w.description, w.accountId, true)); err != nil {
		return err
	}
	for _, g := range w.grants {
		if err := i.writeGrant(writeCtx, w.slug, g, true); err != nil {
			return err
		}
	}
	return nil
}

func (i *Integration) writeRoleUpdate(ctx context.Context, next roleRow, grants, previous []componentAuth.VerbResource) error {
	writeCtx := componentAuth.ContextWithInternalOrigin(ctx)
	if err := i.exec(writeCtx, roleInsert(next.slug, next.name, next.rank, next.description, next.accountId, true)); err != nil {
		return err
	}
	want := map[componentAuth.VerbResource]bool{}
	for _, g := range grants {
		want[g] = true
	}
	for _, g := range grants {
		if err := i.writeGrant(writeCtx, next.slug, g, true); err != nil {
			return err
		}
	}
	// A REMOVED GRANT IS WRITTEN INACTIVE, not deleted. The row is the record
	// of what this role could do at a point in time, and an audit that can only
	// see the current grant set cannot answer what somebody was allowed to do
	// last month.
	for _, g := range previous {
		if want[g] {
			continue
		}
		if err := i.writeGrant(writeCtx, next.slug, g, false); err != nil {
			return err
		}
	}
	return nil
}

func (i *Integration) writeRoleDeactivation(ctx context.Context, current roleRow) error {
	return i.exec(componentAuth.ContextWithInternalOrigin(ctx),
		roleInsert(current.slug, current.name, current.rank, current.description, current.accountId, false))
}

// roleInsert renders the role write.
//
// ONE RENDERER FOR ALL THREE PATHS -- create, update and deactivate -- because
// rows are append-only: an update and a retirement are both a new VERSION of
// the same id, written whole. Three renderers would be three places for a field
// to go missing, and a field missing from a whole-row write is a field CLEARED.
//
// QuoteString rather than %q: the two diverge on four control characters, and a
// role's description is free text somebody typed.
func roleInsert(slug, name string, rank int, description, accountId string, active bool) string {
	return fmt.Sprintf(
		`mutation createRole(roleId: %s, slug: %s, name: %s, rank: %d, description: %s, accountId: %s, predefined: false, active: %t)`,
		langparser.QuoteString(slug), langparser.QuoteString(slug),
		langparser.QuoteString(name), rank,
		langparser.QuoteString(description), langparser.QuoteString(accountId), active)
}

func (i *Integration) writeGrant(ctx context.Context, slug string, g componentAuth.VerbResource, active bool) error {
	return i.exec(ctx, fmt.Sprintf(
		`mutation createCapability(capabilityId: %s, roleSlug: %s, verb: %s, resourceType: %s, effect: "allow", predefined: false, active: %t)`,
		langparser.QuoteString(grantRowId(slug, g)), langparser.QuoteString(slug),
		langparser.QuoteString(g.Verb), langparser.QuoteString(g.Resource), active))
}

// grantRowId is DERIVED from the triple rather than random, so re-granting a
// pair a role already holds appends a version of one row instead of producing a
// second row saying the same thing -- and so retiring a grant can find the row
// it is retiring.
func grantRowId(slug string, g componentAuth.VerbResource) string {
	return "cap-" + slug + "-" + g.Verb + "-" + g.Resource
}

// ---------------------------------------------------------------------
// the audit line
// ---------------------------------------------------------------------

// auditRole writes one v1:identity:auditEvent per decision.
//
// NO INTERNAL-ORIGIN STAMP, deliberately: createAuditEvent is not @serverOnly,
// and v1:identity:auditEvent declares @rowAuthz(owner="actorUserId",
// clusterOwner) -- the actor recorded IS this caller, so the ordinary write
// path admits it. Stamping anyway would widen a call that does not need
// widening, which is how an escape becomes ambient.
func (i *Integration) auditRole(ctx context.Context, access *componentAuth.AccessContext, action, slug string, detail map[string]any) {
	if i.engine == nil {
		return
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["slug"] = slug
	body, err := json.Marshal(detail)
	if err != nil {
		return
	}
	query := fmt.Sprintf(
		`mutation createAuditEvent(eventId:%s, occurredAt:%s, category:%s, action:%s, actorUserId:%s, actorEmail:%s, actorRole:%s, targetType:%s, targetId:%s, outcome:%s, detail:%s)`,
		langparser.QuoteString(fmt.Sprintf("audit-role-%s-%d", slug, time.Now().UTC().UnixNano())),
		langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)),
		langparser.QuoteString("authorization"),
		langparser.QuoteString(action),
		langparser.QuoteString(access.UserId),
		langparser.QuoteString(access.PrimaryEmail),
		langparser.QuoteString(string(access.Role)),
		langparser.QuoteString("role"),
		langparser.QuoteString(slug),
		langparser.QuoteString("success"),
		string(body))
	_, _ = i.engine.Execute(ctx, query)
}

// decisionNodes wraps a decision in the single-node shape a builtin returns.
func decisionNodes(d roleDecision) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(map[string]any{
		"ok":      d.ok,
		"slug":    d.slug,
		"code":    d.code,
		"message": d.detail,
	})
	return []memorynodes.MemoryNode{{
		ID:        "integration:rbac:roleDecision",
		Concept:   "integration:rbac:decision",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}
