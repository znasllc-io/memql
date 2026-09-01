package memql

// RANK-VISIBLE READS AND RANK-STRICT WRITES (epic memql#4832, task
// memql#4834 -- design decisions D2, D3, D4).
//
// The owned tier answers one question: "is this row the caller's?".
// The rank modifiers answer a second one the tiers could not previously
// express at all: "does this row belong to somebody this caller
// OUTRANKS?".
//
//	reads  (D2)  rank(roleOf(row.owner)) <= rank(actor)   peers included
//	writes (D3)  rank(roleOf(row.owner)) <  rank(actor)   peers read-only,
//	                                                      plus "your own
//	                                                      row", always
//
// THE COST, AND THE SHORTCUT THAT IS WRONG. This needs the ROW OWNER'S
// ROLE, which the gate never looked up before: the owned tier compiles
// to a string comparison pushed into SQL and sameRowAuthzOwner is a
// string compare in Go. The tempting fix is to denormalise the owner's
// rank onto the row at write time. It is wrong, and wrong SILENTLY:
// promoting a user retroactively changes who may see the rows they
// already own, so a stamped rank is stale from the moment anybody's role
// changes -- and stale in the direction of showing too much.
//
// So the rank is resolved per REQUEST instead, into one map shared by
// every consumer, which is the property that matters most here: the SQL
// term, the in-memory post-filter and the per-row gate all read the same
// resolved answer, so the three cannot disagree about a single row.
// A cluster's principal count is small and bounded -- this is an
// operator's cluster, not a consumer social graph -- so one map per
// request is affordable where a per-row join is not.
//
// THE SQL HALF MUST PUSH DOWN. A post-filter-only implementation breaks
// pagination: the scan window fills with rows the gate then drops, and a
// short page is read as EXHAUSTION by the cursor logic, which withdraws
// the cursor and makes everything past it unreachable. executor_filter.go
// documents that trap for the existing gate; this term inherits it, which
// is why the injected node lowers into an ordinary payload comparison
// both halves compile rather than into a Go-only check.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// RankScopeExpression is the injected placeholder for a rank predicate.
//
// It is SYMBOLIC on purpose, exactly as `actor.userId` is. The plan it
// lands in is cached and its cache signature is taken from plan.Root
// (memql#3172 finding 2), so a node carrying resolved user ids would bake
// one caller's answer into a shared plan. The node names the RULE; the
// ids are resolved at execution, per request, by rankScopeFor.
type RankScopeExpression struct {
	// OwnerField is the payload property holding the row's owner --
	// whatever the concept's `owner=` argument named.
	OwnerField string
	// Strict selects the write rule (rank(owner) < rank(actor)) over the
	// read rule (<=). Only the read form is ever injected into a plan;
	// the write form is evaluated per row by the write guard, and the
	// field exists so both spellings come from one place.
	Strict bool
	// UnownedFloor is the role slug from which a row with an EMPTY owner
	// becomes readable, or "" when unowned rows are not admitted by the
	// rank branch at all.
	UnownedFloor string
}

func (*RankScopeExpression) isExpressionNode() {}

// rankScope is the per-request resolution: who this actor outranks.
//
// The two id sets are stored rather than a rank map because both
// consumers need a SET -- the SQL term binds one as a parameter list and
// the row gate tests membership -- and deriving the sets once means the
// two can never be derived differently.
type rankScope struct {
	// actorRank is the caller's own rung, 0 when they hold none.
	actorRank int
	// unranked marks an actor the rank rules do not govern (D4).
	unranked bool
	// readOwners holds every owner id spelling admitted by the READ rule,
	// writeOwners every spelling admitted by the WRITE rule. Both carry
	// the canonical AND the bare spelling of each user, for the reason
	// sameRowAuthzOwner cannot be `==`: an owner field is an outgoing
	// @relationship and is stored canonical, while the actor envelope and
	// every client speak the bare id. Emitting both is explicit; relying
	// on the RHS canonicaliser to reach inside a list literal would be a
	// silent miss if it ever did not.
	readOwners  map[string]struct{}
	writeOwners map[string]struct{}
	// ladder is the resolved slug -> rank mapping this scope was built
	// from. Carried rather than re-read so the unowned floor is measured
	// against the same ladder the owner sets were, and so the row gate
	// needs no database of its own.
	ladder roleLadder
	// fingerprint identifies this resolution. It is a CACHE-KEY input:
	// the actor alone is not enough, because promoting somebody ELSE
	// changes what this actor may see without changing who this actor is.
	fingerprint string
}

// admitsRead / admitsWrite test one stored owner value against the scope.
func (s *rankScope) admitsRead(stored string) bool  { return s.admits(s.readOwners, stored) }
func (s *rankScope) admitsWrite(stored string) bool { return s.admits(s.writeOwners, stored) }

func (s *rankScope) admits(set map[string]struct{}, stored string) bool {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		// An UNOWNED row is not admitted by the rank branch. It has no
		// owner, therefore no rank, therefore nothing to compare -- and
		// admitting it here would make every deployment-owned row visible
		// to the whole cluster the moment a concept opted in. The
		// `unowned="<role>"` floor is how a concept says what such a row
		// is worth; it is applied by the caller, which is the only place
		// that knows the declaration.
		return false
	}
	if _, ok := set[stored]; ok {
		return true
	}
	_, ok := set[BareShortId(stored)]
	return ok
}

// rankScopeMemo is the per-request holder. Placed on the context by the
// read and write entry points so one request resolves the ladder once;
// absent, rankScopeFor still answers correctly, it just recomputes.
type rankScopeMemo struct {
	once   sync.Once
	engine *MemQLEngine
	scope  *rankScope
}

type rankScopeMemoKey struct{}

// contextWithRankScopeMemo installs the per-request holder, carrying the
// engine so a consumer that has no engine of its own -- rowAuthzAdmits is
// a package function, deliberately, so the read gate and the write guard
// share one -- can still reach the SAME resolution rather than a second
// one taken independently.
func contextWithRankScopeMemo(ctx context.Context, e *MemQLEngine) context.Context {
	if ctx == nil || e == nil {
		return ctx
	}
	if _, ok := ctx.Value(rankScopeMemoKey{}).(*rankScopeMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, rankScopeMemoKey{}, &rankScopeMemo{engine: e})
}

// rankScopeFromContext returns the request's resolved scope, or nil when
// no entry point installed one.
//
// NIL IS A REFUSAL TO WIDEN, NEVER A REFUSAL TO ANSWER. The rank branch
// is a DISJUNCT -- it only ever ADDS rows to what the owner and
// cluster-owner branches already admit -- so a caller that cannot resolve
// a scope falls through to exactly the pre-rank behaviour. The failure
// direction is a row withheld, never a row disclosed, which is the only
// direction a gate may fail in when the alternative is silence.
func rankScopeFromContext(ctx context.Context) *rankScope {
	memo, _ := ctx.Value(rankScopeMemoKey{}).(*rankScopeMemo)
	if memo == nil {
		return nil
	}
	// The engine check is INSIDE the once, not in front of it. A memo whose
	// scope is already resolved answers from it regardless -- the resolution
	// is the thing this returns, and refusing to hand back an answer already
	// computed because the resolver that computed it is not attached would
	// make the memo's own contents unreachable.
	memo.once.Do(func() {
		if memo.engine != nil {
			memo.scope = memo.engine.resolveRankScope(ctx)
		}
	})
	return memo.scope
}

// rankScopeFor resolves the caller's scope, once per request when the
// memo is installed.
//
// THE ANSWER IS THE SAME WITH OR WITHOUT THE MEMO. The memo is a cost
// optimisation and an anti-drift measure, never a correctness input: a
// gate that only worked when some earlier layer remembered to stamp a
// context is a gate that fails open on the path nobody stamped.
func (e *MemQLEngine) rankScopeFor(ctx context.Context) *rankScope {
	memo, _ := ctx.Value(rankScopeMemoKey{}).(*rankScopeMemo)
	if memo == nil {
		return e.resolveRankScope(ctx)
	}
	memo.once.Do(func() { memo.scope = e.resolveRankScope(ctx) })
	return memo.scope
}

// rankAdmitsRow is the ROW-GATE half of the rank rules -- the answer for
// one row already in hand, used where there is no filter to push down:
// a raw query string, graph expansion, a top-level builtin's results, the
// subscription fan-out, and the write guard.
//
// It reads the SAME resolved scope the filter term lowered from, so the
// two halves of a single request cannot disagree about a single row.
func rankAdmitsRow(ctx context.Context, decl *langparser.RowAuthzDecl, storedOwner string, ownerPresent bool, write bool) bool {
	if decl == nil || !decl.RankVisible {
		return false
	}
	if write && !decl.RankStrict {
		return false
	}
	scope := rankScopeFromContext(ctx)
	if scope == nil {
		return false
	}
	if !ownerPresent {
		// An ABSENT owner field, which the owned tier has always denied:
		// "a row that cannot say who owns it is not a row this caller can
		// be shown to own". Only a field that is PRESENT and empty is a
		// deliberate statement of cluster ownership.
		return false
	}
	if strings.TrimSpace(storedOwner) == "" {
		// UNOWNED -- the deployment's row. The floor is on the ACTOR's
		// rank, and it governs reads only: a row nobody owns has no owner
		// to be strictly below, so there is no rank-strict answer for it
		// and the ordinary escapes stay the only way to write one.
		if write || decl.Unowned == "" {
			return false
		}
		return rankFloorAdmits(scope.ladder, decl.Unowned, scope.actorRank)
	}
	if write {
		return scope.admitsWrite(storedOwner)
	}
	return scope.admitsRead(storedOwner)
}

func (e *MemQLEngine) resolveRankScope(ctx context.Context) *rankScope {
	scope := &rankScope{
		readOwners:  map[string]struct{}{},
		writeOwners: map[string]struct{}{},
	}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		// No caller: no rank, and the owned tier's "no identity, no rows"
		// rule has already refused the read upstream. Nothing is admitted
		// here either, so a path that somehow skipped that refusal still
		// yields no rows rather than everyone's.
		scope.fingerprint = "noactor"
		scope.ladder = roleLadder{ranks: map[string]int{}}
		return scope
	}
	scope.unranked = ac.Unranked
	ladder := e.rankLadder(ctx)
	scope.ladder = ladder
	scope.actorRank = ladder.rankOf(string(ac.Role))

	// The caller's OWN row is always in both sets. "Your own rows stay
	// writable unconditionally" is D3's own sentence, and it must not
	// depend on resolving the caller in the user table -- a principal
	// whose row is missing or not yet readable would otherwise lose access
	// to their own data.
	addOwnerSpellings(scope.readOwners, ac.UserId)
	addOwnerSpellings(scope.writeOwners, ac.UserId)

	for id, roleSlug := range e.principalRoles(ctx) {
		rank := ladder.rankOf(roleSlug)
		if rank <= scope.actorRank {
			addOwnerSpellings(scope.readOwners, id)
		}
		if rank < scope.actorRank {
			addOwnerSpellings(scope.writeOwners, id)
		}
	}
	scope.fingerprint = fingerprintOwnerSet(scope.actorRank, scope.readOwners)
	return scope
}

// addOwnerSpellings records both id spellings for one user.
func addOwnerSpellings(set map[string]struct{}, id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	set[id] = struct{}{}
	if bare := BareShortId(id); bare != "" {
		set[bare] = struct{}{}
	}
	set[conceptIdentityUser+":"+BareShortId(id)] = struct{}{}
}

func fingerprintOwnerSet(actorRank int, set map[string]struct{}) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	_, _ = h.Write([]byte(strings.Join(keys, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))[:16] + ":" + strconv.Itoa(actorRank)
}


// roleLadder is the resolved slug -> rank mapping.
type roleLadder struct {
	ranks map[string]int
}

// rankOf resolves a role slug, falling back to the engine's compiled
// ladder for the base roles.
//
// FAILS CLOSED. An unknown slug is rank 0 -- below every base rank -- so
// a read admits only rows whose owner is also unranked (none, since an
// unowned row is refused separately) and a write admits nothing. The
// alternative, treating unknown as "no opinion" and passing the row, is
// how an unrecognised role becomes a permission.
func (l roleLadder) rankOf(slug string) int {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return 0
	}
	if r, ok := l.ranks[slug]; ok {
		return r
	}
	return auth.RoleRank(auth.Role(slug))
}

// rankLadder reads the role catalog -- slugs, ranks and legacy aliases --
// so a CUSTOM role slots into the same ordering as a base one (D5).
//
// The catalog is the source and auth.RoleRank is the floor: a cluster
// whose catalog has not seeded yet, or whose database is unreachable,
// still ranks the five base slugs correctly rather than ranking every
// principal at 0 and refusing the whole cluster its own rows.
func (e *MemQLEngine) rankLadder(ctx context.Context) roleLadder {
	ladder := roleLadder{ranks: map[string]int{}}
	db := e.database()
	if db == nil {
		return ladder
	}
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptRbacRole).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx); err != nil {
		return ladder
	}
	seen := map[string]struct{}{}
	for i := range nodes {
		payload := rankRowPayload(nodes[i])
		if payload == nil {
			continue
		}
		slug := strings.ToLower(strings.TrimSpace(stringFromAny(payload["slug"])))
		if slug == "" {
			continue
		}
		// Rows are append-only, newest first: the first sighting of a slug
		// is its current definition.
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}
		if active, ok := payload["active"].(bool); ok && !active {
			continue
		}
		rank, ok := intWithOkFromAny(payload["rank"])
		if !ok {
			continue
		}
		ladder.ranks[slug] = rank
		// ALIASES are what let the shell and the engine speak one
		// vocabulary. The user row's role enum is the legacy five
		// (owner/admin/developer/writer/reader) while the catalog seeds
		// owner/developer/admin/user/viewer, and without the aliases as
		// DATA every consumer needs its own translation table -- which is
		// how the two ladders diverged in the first place.
		for _, alias := range rankAliasList(payload["aliases"]) {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias == "" {
				continue
			}
			if _, taken := ladder.ranks[alias]; !taken {
				ladder.ranks[alias] = rank
			}
		}
	}
	return ladder
}

// principalRoles reads every principal's current role slug, keyed by user
// id. The map is small and bounded by design (B.2 of the design record).
func (e *MemQLEngine) principalRoles(ctx context.Context) map[string]string {
	out := map[string]string{}
	db := e.database()
	if db == nil {
		return out
	}
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		Where("concept = ?", conceptIdentityUser).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx); err != nil {
		return out
	}
	for i := range nodes {
		id := strings.TrimSpace(nodes[i].ID)
		if id == "" {
			continue
		}
		if _, dup := out[id]; dup {
			// Append-only, newest first: keep the current version.
			continue
		}
		payload := rankRowPayload(nodes[i])
		if payload == nil {
			out[id] = ""
			continue
		}
		out[id] = strings.TrimSpace(stringFromAny(payload["role"]))
	}
	return out
}

// rankRowPayload decodes one stored row. An unreadable payload yields nil
// rather than an empty map, so a caller can tell "no fields" from "could
// not read" -- and both callers here treat the latter as "this row tells
// me nothing", never as "this row grants nothing to worry about".
func rankRowPayload(n memorynodes.MemoryNode) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil
	}
	return payload
}

// rankAliasList reads a role row's `aliases` list, tolerating both the
// JSON []any a decoded payload carries and a plain []string.
func rankAliasList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ---------------------------------------------------------------------
// Lowering: the symbolic node becomes an ordinary payload comparison.
// ---------------------------------------------------------------------

// lowerRankScope replaces every RankScopeExpression in a tree with the
// ordinary comparison it stands for, resolved against this request's
// scope.
//
// IT LOWERS RATHER THAN EVALUATING, and that is the whole point. An
// ordinary `payload.<owner> in [...]` comparison is a node BOTH halves
// already handle -- the SQL compiler pushes it into the WHERE clause, and
// the in-memory post-filter evaluates it -- so the rank term inherits
// pushdown, and with it the pagination behaviour executor_filter.go
// documents. A Go-only check would fill the scan window with rows the
// gate then drops, and a short page reads as exhaustion: the cursor is
// withdrawn and everything past it becomes unreachable.
//
// Returns a NEW tree. The input may be a cached plan root and must never
// be mutated (#1659) -- the same rule resolveActorComparisonsToConstants
// follows, and the reason cloneRowAuthzPredicate recurses.
func (e *MemQLEngine) lowerRankScope(ctx context.Context, expr ExpressionNode) ExpressionNode {
	switch n := expr.(type) {
	case *RankScopeExpression:
		return e.rankScopeComparison(ctx, n)
	case *LogicalExpression:
		left := e.lowerRankScope(ctx, n.Left)
		right := e.lowerRankScope(ctx, n.Right)
		if left == n.Left && right == n.Right {
			return n
		}
		copied := *n
		copied.Left, copied.Right = left, right
		return &copied
	default:
		return expr
	}
}

// treeHasRankScope reports whether a tree carries a rank term, so the
// callers that must resolve one can skip the walk entirely when it does
// not -- every read in the tree that does not declare a rank modifier
// pays nothing.
func treeHasRankScope(expr ExpressionNode) bool {
	switch n := expr.(type) {
	case *RankScopeExpression:
		return true
	case *LogicalExpression:
		return treeHasRankScope(n.Left) || treeHasRankScope(n.Right)
	default:
		return false
	}
}

// rankScopeComparison builds the concrete term for one rank node.
func (e *MemQLEngine) rankScopeComparison(ctx context.Context, n *RankScopeExpression) ExpressionNode {
	scope := e.rankScopeFor(ctx)
	owners := scope.readOwners
	if n.Strict {
		owners = scope.writeOwners
	}

	values := make([]any, 0, len(owners)+1)
	for id := range owners {
		values = append(values, id)
	}
	// UNOWNED ROWS. A row with an empty owner field belongs to the
	// DEPLOYMENT, not to a principal -- the `self` account is the case --
	// so it has no rank to compare and the rank branch refuses it by
	// default. `unowned="<role>"` is how a concept says what such a row is
	// worth, and the floor is the ACTOR's rank, not the row's.
	//
	// An ABSENT owner key is a different thing and stays denied, matching
	// what the owned tier has always done with one: "a row that cannot say
	// who owns it is not a row this caller can be shown to own". Only a
	// field that is PRESENT and empty is a deliberate statement of
	// cluster ownership.
	if n.UnownedFloor != "" && !n.Strict {
		if rankFloorAdmits(scope.ladder, n.UnownedFloor, scope.actorRank) {
			values = append(values, "")
		}
	}
	// Sorted so the compiled parameter list -- and therefore the plan
	// cache signature and every test assertion over it -- is stable rather
	// than following Go's map iteration order.
	sortAnyStrings(values)

	if len(values) == 0 {
		// Nothing is admitted. Folding to a false constant rather than
		// emitting `in []` keeps the SQL compiler off a degenerate list
		// whose semantics vary by backend, and says the same thing.
		return &constantBoolExpression{value: false}
	}
	return &ComparisonExpression{
		Field: FieldReference{
			Raw:   "payload." + n.OwnerField,
			Parts: []string{"payload", n.OwnerField},
		},
		Operator: OpIn,
		Value:    values,
	}
}

func sortAnyStrings(values []any) {
	sort.Slice(values, func(i, j int) bool {
		a, _ := values[i].(string)
		b, _ := values[j].(string)
		return a < b
	})
}

// rankFloorAdmits tests an actor's rank against a declared floor.
//
// AN UNRESOLVABLE FLOOR DENIES EVERYONE, and this function exists because the
// obvious spelling does the opposite. `actorRank >= ladder.rankOf(slug)` reads
// correctly and fails OPEN: rankOf answers 0 for a slug it does not know, and
// every rank is >= 0, so a single typo in `unowned="devleoper"` turns a floor
// into a pass-through that hands every deployment-owned row to every caller --
// silently, with the declaration still reading like a gate.
//
// The load-time check (validateRowAuthzUnownedSlugs) is what stops such a
// declaration reaching a booted engine at all. This is the runtime backstop,
// and the two are deliberately not one: a gate whose only enforcement runs at
// boot is a gate that a code path bypassing boot does not have.
func rankFloorAdmits(ladder roleLadder, slug string, actorRank int) bool {
	floor := ladder.rankOf(slug)
	if floor <= 0 {
		return false
	}
	return actorRank >= floor
}
