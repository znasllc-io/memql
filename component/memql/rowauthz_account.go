package memql

// The ACCOUNT GRANT of the owned tier (epic memql#5165, task memql#5169).
//
// `@rowAuthz(owner="<field>", account="accountId")` says: this row belongs to
// an account, and anyone whose group ties them to that account may reach it.
// It is the mechanism the whole groups program rests on, and it is deliberately
// the SAME SHAPE as the rank scope beside it (rowauthz_account.go is a sibling
// of rowauthz_rank.go, function for function) rather than a second
// authorization system:
//
//   - a symbolic node (AccountScopeExpression) is OR-ed onto the concept's
//     rendered predicate at plan time, when the actor is not yet known;
//   - the actor's scope is resolved ONCE PER REQUEST and memoised on the
//     context;
//   - the node lowers to SQL against that resolution at execution;
//   - the row gate evaluates the same three cases against one row's payload,
//     so subscriptions inherit the branch with no work of their own
//     (AdmitSubscriptionRow calls rowAuthzAdmits, which is where this lives).
//
// # Why the grant widens WRITES too
//
// The rank record's D3 made peer rows read-only. This argument widens writes
// (design D4): a member may write a tied row whatever the owner's rank, if
// their ROLE holds the verb. That is not a softening of D3 -- it is the point
// of the program. A client's people are on the cluster to work on the client's
// things, and a grant that admitted reads alone would let them see the
// campaign they may not edit. The VERB is decided upstream, by the data-plane
// capability gate and the mutation's own annotations; this gate answers "which
// rows", never "may this actor write at all".
//
// # Staff are a RULE, not rows (design D6)
//
// Everyone at developer rank and above is a standing member of every
// account-kind group. Rows drift -- a developer hired next month is in no group
// until something adds them -- and a rule does not. Their scope resolves to
// `everyAccount`, which lowers to "the account field is present and non-empty"
// rather than to a list, so it costs no membership read and never goes stale.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The two concepts the resolution reads. Written out rather than derived for
// the reason conceptIdentityUser is: a constant a reader can grep for beats a
// construction they have to reassemble.
const (
	conceptIdentityGroup           = "v1:identity:group"
	conceptIdentityGroupMembership = "v1:identity:groupMembership"
)

// accountStaffFloorSlug is the rung from which an actor is STAFF (design D6).
//
// A SLUG rather than a number, resolved through the same ladder every other
// rank decision reads, so a cluster that re-ranks its roles re-ranks this too.
// A custom role ranked at or above developer is staff as well -- that is what
// the ladder means, and the role editor says so on the rank stop.
const accountStaffFloorSlug = "developer"

// AccountScopeExpression is the symbolic account-grant node.
//
// It carries the FIELD and nothing else. Whether that field holds one account
// id or a list of them is a property of the CONCEPT, and the lowering answers
// both with one jsonb expression, so there is nothing here to keep in step
// with the declaration. (RankScopeExpression carries `Strict` because the read
// and write rules genuinely differ; here they do not -- design D4 is that a
// member who may read a tied row may write it if their role holds the verb.)
type AccountScopeExpression struct {
	// Field is the payload property naming the row's account, as declared
	// in `@rowAuthz(..., account="<field>")`.
	Field string
}

func (*AccountScopeExpression) isExpressionNode() {}

// accountScopeMatch is the LOWERED form: the same question with this
// request's answer substituted in.
//
// Separate from the symbolic node above because the symbolic one lives in a
// CACHED plan shared by every caller, and a lowered one is true for exactly
// one of them. Collapsing the two would put one actor's account set into a
// plan another actor reuses -- the failure the rank scope's own two-node split
// exists to prevent.
type accountScopeMatch struct {
	field        string
	accounts     []string
	everyAccount bool
}

func (*accountScopeMatch) isExpressionNode() {}

// accountScope is one request's resolved answer.
type accountScope struct {
	// accounts holds every account id this actor's memberships reach, in
	// both spellings (canonical and bare) for the reason the rank scope
	// carries both: an account field may be an outgoing @relationship and
	// is then stored canonical, while every client speaks the bare id.
	accounts map[string]struct{}
	// everyAccount is the STAFF rule (D6). When set, `accounts` is empty
	// and meaningless -- the actor reaches every TIED row, and no untied
	// one.
	everyAccount bool
	// fingerprint identifies this resolution for the plan cache. The actor
	// alone is not enough: adding somebody to a group changes what they
	// may see without changing who they are.
	fingerprint string
}

// admits tests one row's stored account value against the scope.
func (s *accountScope) admits(stored string) bool {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		// A row with no account is not a tied row, so the account branch
		// has nothing to say about it. The owner and cluster-owner
		// branches still decide it -- this is a DISJUNCT, and returning
		// false here withholds nothing that was otherwise admitted.
		return false
	}
	if s.everyAccount {
		return true
	}
	if _, ok := s.accounts[stored]; ok {
		return true
	}
	_, ok := s.accounts[BareShortId(stored)]
	return ok
}

// admitsAny tests a row's account value, which may be a single id or a list.
func (s *accountScope) admitsAny(value any) bool {
	switch v := value.(type) {
	case string:
		return s.admits(v)
	case []any:
		for _, item := range v {
			if str, ok := item.(string); ok && s.admits(str) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range v {
			if s.admits(item) {
				return true
			}
		}
		return false
	default:
		// Any other shape is a field the load-time type check should have
		// refused (concept_parser.go's validateRowAuthzAccount). Denying
		// is the fail-closed answer and the one that matches the SQL: a
		// number or a nested object never matches a text containment test
		// either.
		return false
	}
}

// accountScopeMemo is the per-request holder, the rankScopeMemo pattern.
//
// IT ALSO CARRIES THE MEMBERSHIP READ (epic memql#5295, D9). The account scope
// and the grant resolver both start from "which groups is this person in",
// and a second membership query per request is exactly what the record
// refuses: "the grant resolver reads the same memo rather than issuing a
// second membership query, so a request costs one membership read whichever
// gates it passes". The memberships are keyed BY USER rather than held as one
// value because a nested call under borrowed or system authority shares the
// request's context, and a memo that remembered the outer caller's groups
// would hand them to the inner one.
type accountScopeMemo struct {
	once   sync.Once
	engine *MemQLEngine
	scope  *accountScope

	mu      sync.Mutex
	members map[string]*membershipsEntry
}

// memberships is one actor's resolved membership facts: the ACTIVE groups an
// active membership places them in, and the accounts those groups are tied
// to. Both come from the same two reads, which is the point of resolving them
// together.
type memberships struct {
	groupIds   []string
	accountIds []string
}

type membershipsEntry struct {
	once sync.Once
	m    memberships
}

func (m *accountScopeMemo) membershipsEntry(userId string) *membershipsEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members == nil {
		m.members = map[string]*membershipsEntry{}
	}
	entry, ok := m.members[userId]
	if !ok {
		entry = &membershipsEntry{}
		m.members[userId] = entry
	}
	return entry
}

// membershipsFor resolves one actor's memberships, once per request per user
// when the memo is installed, and correctly without it -- the rankScopeFor
// rule: the memo is a cost measure, never a correctness input.
func (e *MemQLEngine) membershipsFor(ctx context.Context, userId string) memberships {
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return memberships{}
	}
	memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo)
	if memo == nil {
		return e.resolveMemberships(ctx, userId)
	}
	entry := memo.membershipsEntry(userId)
	entry.once.Do(func() { entry.m = e.resolveMemberships(ctx, userId) })
	return entry.m
}

// resolveMemberships is the two reads: the actor's ACTIVE memberships, then
// the ACTIVE groups those name. An archived group places nobody (design
// section K) and grants nobody anything, so it is dropped from BOTH answers
// here rather than trusted from the membership row.
func (e *MemQLEngine) resolveMemberships(ctx context.Context, userId string) memberships {
	db := e.database()
	if db == nil {
		return memberships{}
	}
	memberOf := activeGroupIdsForUser(ctx, db, userId)
	if len(memberOf) == 0 {
		return memberships{}
	}
	groupIds, accountIds := activeGroupsAndAccounts(ctx, db, memberOf)
	return memberships{groupIds: groupIds, accountIds: accountIds}
}

type accountScopeMemoKey struct{}

// contextWithAccountScopeMemo installs the per-request holder, carrying the
// engine so the package-level row gate can reach the SAME resolution rather
// than taking a second one independently.
func contextWithAccountScopeMemo(ctx context.Context, e *MemQLEngine) context.Context {
	if ctx == nil || e == nil {
		return ctx
	}
	if _, ok := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, accountScopeMemoKey{}, &accountScopeMemo{engine: e})
}

// accountScopeFromContext returns the request's resolved scope, or nil when no
// entry point installed one.
//
// NIL IS A REFUSAL TO WIDEN, NEVER A REFUSAL TO ANSWER -- the rank scope's own
// rule, and it holds here for the same reason: the account branch is a
// DISJUNCT, so a caller that cannot resolve a scope falls through to exactly
// the pre-grant behaviour. The failure direction is a row withheld, never a
// row disclosed.
func accountScopeFromContext(ctx context.Context) *accountScope {
	memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo)
	if memo == nil {
		return nil
	}
	memo.once.Do(func() {
		if memo.engine != nil {
			memo.scope = memo.engine.resolveAccountScope(ctx)
		}
	})
	return memo.scope
}

// accountScopeFor resolves this request's scope, memoised.
func (e *MemQLEngine) accountScopeFor(ctx context.Context) *accountScope {
	if scope := accountScopeFromContext(ctx); scope != nil {
		return scope
	}
	return e.resolveAccountScope(ctx)
}

// resolveAccountScope reads the actor's account set (design C, "Resolution,
// per request").
//
// Two small reads at most, and usually fewer: no actor reads nothing, and a
// staff actor reads only the ladder.
func (e *MemQLEngine) resolveAccountScope(ctx context.Context) *accountScope {
	scope := &accountScope{accounts: map[string]struct{}{}}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		// No caller: the owned tier's "no identity, no rows" rule has
		// already refused the read upstream. Admitting nothing here means a
		// path that somehow skipped that refusal still yields no rows.
		scope.fingerprint = "noactor"
		return scope
	}

	ladder := e.rankLadder(ctx)
	if floor := ladder.rankOf(accountStaffFloorSlug); floor > 0 && ladder.rankOf(string(ac.Role)) >= floor {
		// STAFF (D6). Note the `floor > 0` guard, which is rankFloorAdmits'
		// lesson in one line: rankOf answers 0 for a slug it does not know,
		// and every rank clears 0, so the natural spelling would make every
		// caller staff on a cluster whose role catalog had not seeded.
		scope.everyAccount = true
		scope.fingerprint = "everyAccount"
		return scope
	}

	for _, accountId := range e.membershipsFor(ctx, ac.UserId).accountIds {
		addAccountSpellings(scope.accounts, accountId)
	}
	scope.fingerprint = fingerprintAccountSet(ac.UserId, scope.accounts)
	return scope
}

// staged-data: MUST-NOT-GATE -- an AUTHORIZATION read, and gating it produces
// a false DENIAL rather than a false disclosure. A staged membership row hidden
// here does not leak anything; it removes a grant the operator believes they
// made, which presents as "I cannot see my client's work" and reads as a
// membership problem rather than a staging one. The read is not a disclosure
// surface -- it is what CREATES the grant.
//
// activeGroupIdsForUser collapses v1:identity:groupMembership to the newest
// version per id -- the `DISTINCT ON (id)` principalRoles performs, and for
// the same reason: MemQL rows are append-only, so removing somebody writes a
// NEW version rather than deleting the old one. Without the collapse a removed
// member's original `active` row is still there and still grants.
func activeGroupIdsForUser(ctx context.Context, db *bun.DB, userId string) []string {
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		DistinctOn("id").
		Where("concept = ?", conceptIdentityGroupMembership).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil
	}
	bareUser := BareShortId(userId)
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for i := range nodes {
		payload := accountRowPayload(nodes[i])
		if payload == nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["status"])) != "active" {
			continue
		}
		rowUser := strings.TrimSpace(stringFromAny(payload["userId"]))
		if rowUser == "" {
			continue
		}
		if rowUser != userId && BareShortId(rowUser) != bareUser {
			continue
		}
		groupId := strings.TrimSpace(stringFromAny(payload["groupId"]))
		if groupId == "" {
			continue
		}
		if _, dup := seen[groupId]; dup {
			continue
		}
		seen[groupId] = struct{}{}
		out = append(out, groupId)
	}
	return out
}

// staged-data: MUST-NOT-GATE -- the second half of the same authorization
// read, and the same verdict for the same reason. Worse here, in fact: hiding a
// staged GROUP silently drops every membership pointing at it, so a whole
// client's people lose their access at once rather than one person losing
// theirs.
//
// activeGroupsAndAccounts reads the named groups and returns, of each ACTIVE
// one, its id and (when non-empty) its accountId.
//
// An archived group grants nothing (design section K): the membership row is
// history, and a group under an archived account was archived by the cascade.
// Both facts are read here rather than trusted from the membership, because a
// group's status changes without its memberships being rewritten.
//
// TWO ANSWERS FROM ONE READ. The account scope wants the accounts; the grant
// resolver wants the groups themselves (a custom group with no account can be
// a grant subject, D13, "it grants apps and still grants no rows"). Reading
// the groups once and answering both is what keeps a request at one
// membership read whichever gates it passes.
func activeGroupsAndAccounts(ctx context.Context, db *bun.DB, groupIds []string) (activeGroupIds, accountIds []string) {
	wanted := make(map[string]struct{}, len(groupIds)*2)
	for _, g := range groupIds {
		wanted[g] = struct{}{}
		if bare := BareShortId(g); bare != "" {
			wanted[bare] = struct{}{}
		}
	}
	var nodes []memorynodes.MemoryNode
	if err := db.NewSelect().
		Model(&nodes).
		DistinctOn("id").
		Where("concept = ?", conceptIdentityGroup).
		OrderExpr(`id ASC, "createdAt" DESC`).
		Scan(ctx); err != nil {
		return nil, nil
	}
	seenAccount := map[string]struct{}{}
	activeGroupIds = make([]string, 0, len(groupIds))
	accountIds = make([]string, 0, len(groupIds))
	for i := range nodes {
		id := strings.TrimSpace(nodes[i].ID)
		if id == "" {
			continue
		}
		if _, want := wanted[id]; !want {
			if _, wantBare := wanted[BareShortId(id)]; !wantBare {
				continue
			}
		}
		payload := accountRowPayload(nodes[i])
		if payload == nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["status"])) != "active" {
			continue
		}
		activeGroupIds = append(activeGroupIds, id)
		accountId := strings.TrimSpace(stringFromAny(payload["accountId"]))
		if accountId == "" {
			// A group with no account grants nothing and exists to
			// organize (design D8). Not an error, and not a warning.
			continue
		}
		if _, dup := seenAccount[accountId]; dup {
			continue
		}
		seenAccount[accountId] = struct{}{}
		accountIds = append(accountIds, accountId)
	}
	return activeGroupIds, accountIds
}

// accountRowPayload unmarshals a row's payload, answering nil on failure.
func accountRowPayload(node memorynodes.MemoryNode) map[string]any {
	if len(node.Payload) == 0 {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(node.Payload, &payload); err != nil {
		return nil
	}
	return payload
}

// addAccountSpellings records both spellings of one account id, the
// addOwnerSpellings discipline: an account field may be an outgoing
// @relationship and is then stored canonical, while every client speaks bare.
func addAccountSpellings(set map[string]struct{}, id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	set[id] = struct{}{}
	if bare := BareShortId(id); bare != "" {
		set[bare] = struct{}{}
		set[conceptAccountsAccount+":"+bare] = struct{}{}
	}
}

// fingerprintAccountSet identifies one resolution for the plan cache.
//
// THE ACTOR IS IN IT as well as the set, because two actors can resolve the
// same account set through different groups, and a plan keyed on the set alone
// would be correct today and wrong the moment the resolution grows a term that
// is not the set.
// SHA-256 RATHER THAN A CHEAP HASH, and the reason is a security property
// rather than habit: this string IS part of the plan cache key, so two actors
// whose fingerprints collide share a cached plan -- one caller's resolved row
// set served to another. Collision resistance is exactly what is wanted, and
// the "use bcrypt" advice for password hashing does not apply to a cache key,
// which must be deterministic and fast by construction.
//
// The ACTOR IS FOLDED IN AS A LIST ENTRY rather than written as its own bare
// argument, which is the shape fingerprintOwnerSet already has. Two reasons,
// and only the first is about behaviour: the entry carries an "actor:" tag so
// it can never be confused with an account id that happened to equal a user
// id, which a positional write could not distinguish. The second is that a
// bare identifier written straight into a digest reads to a scanner as
// credential hashing, and this is not that.
func fingerprintAccountSet(userId string, set map[string]struct{}) string {
	keys := make([]string, 0, len(set)+1)
	keys = append(keys, "actor:"+strings.TrimSpace(userId))
	for k := range set {
		keys = append(keys, "account:"+k)
	}
	sort.Strings(keys)
	h := sha256.New()
	_, _ = h.Write([]byte(strings.Join(keys, "\x1f")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// accountAdmitsRow is the row-gate half: does this actor's account scope reach
// the row whose payload is `payload`?
//
// Mirrors rankAdmitsRow, including its shape: a nil declaration or an absent
// argument is not an error, it is "this branch has nothing to say".
func accountAdmitsRow(ctx context.Context, decl *langparser.RowAuthzDecl, payload []byte) bool {
	if decl == nil || strings.TrimSpace(decl.Account) == "" {
		return false
	}
	scope := accountScopeFromContext(ctx)
	if scope == nil {
		return false
	}
	if len(payload) == 0 {
		return false
	}
	var row map[string]any
	if err := json.Unmarshal(payload, &row); err != nil {
		return false
	}
	value, present := row[strings.TrimSpace(decl.Account)]
	if !present {
		return false
	}
	return scope.admitsAny(value)
}

// lowerAccountScope replaces every symbolic account node in a tree with this
// request's answer, returning a NEW tree.
//
// Never mutates in place: the tree it walks belongs to a cached plan shared
// with every other caller, and substituting one actor's accounts into it would
// hand them to the next.
func (e *MemQLEngine) lowerAccountScope(ctx context.Context, expr ExpressionNode) ExpressionNode {
	switch n := expr.(type) {
	case *AccountScopeExpression:
		return e.accountScopeComparison(ctx, n)
	case *LogicalExpression:
		left := e.lowerAccountScope(ctx, n.Left)
		right := e.lowerAccountScope(ctx, n.Right)
		if left == n.Left && right == n.Right {
			return n
		}
		return &LogicalExpression{Op: n.Op, Left: left, Right: right}
	default:
		return expr
	}
}

// treeHasAccountScope reports whether a tree carries a symbolic account node,
// so the plan-key term and the lowering pass are paid for only by the reads
// that actually have one.
func treeHasAccountScope(expr ExpressionNode) bool {
	switch n := expr.(type) {
	case *AccountScopeExpression:
		return true
	case *LogicalExpression:
		return treeHasAccountScope(n.Left) || treeHasAccountScope(n.Right)
	default:
		return false
	}
}

// accountScopeComparison renders one request's answer.
//
// Three cases, exactly the record's table:
//
//	empty scope        -> false (matches nothing)
//	a set of accounts  -> the field contains any of them
//	every account      -> the field is present and non-empty
func (e *MemQLEngine) accountScopeComparison(ctx context.Context, n *AccountScopeExpression) ExpressionNode {
	field := strings.TrimSpace(n.Field)
	if field == "" {
		return &constantBoolExpression{value: false}
	}
	scope := e.accountScopeFor(ctx)
	if scope == nil {
		return &constantBoolExpression{value: false}
	}
	if scope.everyAccount {
		return &accountScopeMatch{field: field, everyAccount: true}
	}
	values := make([]string, 0, len(scope.accounts))
	for account := range scope.accounts {
		values = append(values, account)
	}
	if len(values) == 0 {
		// A member of nothing. Folding to a false constant rather than
		// emitting an empty containment list keeps the SQL compiler off a
		// degenerate array whose semantics vary by backend, and says the
		// same thing.
		return &constantBoolExpression{value: false}
	}
	// Sorted so the compiled parameter list -- and therefore the plan cache
	// signature and every test assertion over it -- is stable rather than
	// following Go's map iteration order.
	sort.Strings(values)
	return &accountScopeMatch{field: field, accounts: values}
}

// accountScopeMatchesNode is the post-filter evaluation of a lowered account
// node -- the in-process twin of the `jsonb_exists_any` arm in
// compileExpression.
//
// IT MUST AGREE WITH THE SQL, and the failure mode if it does not is quiet: a
// paginated read whose SQL admitted a row and whose post-filter dropped it
// looks exactly like exhaustion, so the page comes back short and correct-
// looking. The three cases are therefore stated the same way in both places --
// untied rows never match, staff match every tied row, and a set matches by
// containment across a scalar or a list.
func accountScopeMatchesNode(node memorynodes.MemoryNode, n *accountScopeMatch, cache map[string]map[string]any) bool {
	if n == nil || strings.TrimSpace(n.field) == "" {
		return false
	}
	payload, err := cachedPayloadMap(node, cache)
	if err != nil || payload == nil {
		return false
	}
	value, present := payload[strings.TrimSpace(n.field)]
	if !present {
		return false
	}
	if n.everyAccount {
		return accountValueIsTied(value)
	}
	set := make(map[string]struct{}, len(n.accounts))
	for _, a := range n.accounts {
		set[a] = struct{}{}
	}
	scope := &accountScope{accounts: set}
	return scope.admitsAny(value)
}

// accountValueIsTied reports whether a row's account field names any account
// at all -- the staff rule's "present and non-empty".
//
// An EMPTY string and an EMPTY list are untied, not tied-to-nothing. A row
// that names no client is the operator's own work, and handing it to every
// developer because they are staff of every client would widen the staff rule
// past what it says.
func accountValueIsTied(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		for _, item := range v {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				return true
			}
		}
		return false
	case []string:
		for _, item := range v {
			if strings.TrimSpace(item) != "" {
				return true
			}
		}
		return false
	default:
		return false
	}
}
