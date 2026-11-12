package memql

// account_stamp.go -- the engine fills in WHICH ACCOUNT a row belongs to (design
// record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md,
// sections C and "Refinements made while planning").
//
// # What it is for
//
// Complete separation between accounts, and an export of one account, both ask
// one question of every row: whose is it? About forty concepts answer in a
// field their own writers fill. The rest -- a note, a file's bytes, a run and
// its steps, a mirrored catalog row -- said nothing. This is where they start
// saying it: a concept declared `@belongsTo(account)` records its account in
// `accountId`, and the engine writes that field so that no mutation has to.
//
// # Which concepts it touches, and which it leaves alone
//
// ENGINE-MANAGED concepts only: account-scoped and not `stated`
// (BelongsToDecl.EngineManaged). A `stated` concept's own writers state the
// account, and rules older than this file decide what an absent value means and
// whether it may change -- a role with no account is a cluster-wide role, a
// campaign moves between accounts through validateOrganizationTransfer. Filling
// or fixing the field there would change what those rows MEAN, so this returns
// before reading anything. The 22 organization-owned concepts are all stated.
//
// # Where the account comes from, in order
//
//  1. THE ROW'S OWN LIST, for the five concepts that still carry `accountIds`
//     (design D9). While the list exists it is what a person edits, so the
//     single field follows it: on a create, on a row with no account yet, and
//     on a write that CHANGES the list. This is the one way a recorded account
//     changes, chosen deliberately and only while the list exists (refinement
//     R12). A write that restates the list it found keeps the stored account.
//     For a person's own write the choice is made among the tags that person
//     may write to (pickPlaceableAccount); a tag naming an account they do not
//     belong to files nothing there, and with no tag left the list decides
//     nothing.
//  2. A VALUE ON THE PAYLOAD. Kept. It is the stored account the read-merge
//     carried, or an explicit choice.
//  3. THE STORED ACCOUNT, when the payload names none: a write that replaces
//     the whole row (a mirror re-sync, a template re-inserting under the same
//     id) carries it forward exactly as stored. A recorded account is never
//     lost by a later write.
//  4. THE PARENT (`via`). A child belongs wherever its parent does.
//  5. THE CALL'S ACCOUNT (CallAccount), on a create only.
//  6. NOTHING. The row is written without an account and the assignment of
//     existing rows gives it one on its next pass.
//
// `accountId: ""` counts as no key in every one of these (refinement R6),
// including on a `shared` concept: a cluster-provided row is one that no
// source decides, not one whose writer typed the empty string.
//
// # Why there is no default
//
// A stamp never changes, so a wrong one is wrong for good. Two cases would be
// wrong if this guessed. A child written before its parent has been assigned
// would take the caller's account and then disagree with the parent once the
// parent got its own: so a parent that exists with no account makes the child
// WAIT. And an update of an old, unassigned row by somebody other than its owner
// would hand the row to the editor: so the call's account applies to creates
// only, and the job, which has the owner rule, assigns the rest.
//
// # What it refuses
//
// Only what was forgeable: an account NAMED in the payload by the writer
// themselves. That is the raw surface, a write that did not come from a
// mutation template and does not carry internal origin, where the payload is
// written as supplied (rowauthz_insert_stamp.go states the same problem for the
// owner). And it is a person's write through a template, because a template
// that accepts `accountId` passes its caller's argument through: the author
// decided an account may be named, the person decided which. On those writes an
// account may not be changed, an explicit account must be one the caller may
// write to, and it must agree with the parent's. Server code that names an
// account (internal origin, or a template the cluster or a connector runs) is
// an author's decision and is left to the author.
//
// # What it narrows without refusing
//
// Two things a person supplies decide an account without naming one, and each
// is held to the accounts that person may write to, quietly: the row is
// recorded under their own account instead of being refused, because both
// writes were legal before this file existed. The list of tags, on every
// surface, because a template passes the caller's tags straight through
// (pickPlaceableAccount). And the parent, on the raw surface only, because a
// template that takes a parent id is an author's decision about who may attach
// a row to what, and the raw literal has no author (source 4).
//
// # The switch
//
// Nothing is stamped until AccountStampingEnabled. A node that stamped while an
// older node was still serving would write a key the older node's schema does
// not declare, and the older node would refuse to update that row. For the same
// reason, and so that no value can be planted ahead of the checks above, an
// account NAMED in a write before the switch is dropped for everyone: the field
// is the engine's, and before the switch the engine writes it for no one.
//
// # Spelling
//
// The stamp writes the BARE id, exactly as the owner stamp does, and runs before
// canonicalizeRelationshipFields for the same reason: a concept that declares a
// relationship on the field gets the canonical spelling from that step, and one
// that does not stores it the way it stores every other id. Readers compare by
// bare id.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	coreid "github.com/znasllc-io/memql/core/id"
)

const (
	accountField     = langparser.BelongsToAccountField
	accountListField = "accountIds"

	// The two refusals this file adds. The third, organization_forbidden, is
	// the code the organization rules already use for the same thing.
	codeAccountChangeRefused = "account_change_refused"
	codeAccountMismatch      = "account_mismatch"
)

// accountDelta is what the caller's delta said about the account, captured
// before the read-merge replaces the payload with the merged prior row. After
// that point a stored account and a restated one are the same map entry, and
// the refusals below turn on which it was (deltaNamedOwner is the same capture
// for the same reason).
type accountDelta struct {
	namedAccount bool
	account      any
	namedList    bool
}

func captureAccountDelta(payload map[string]any) accountDelta {
	var d accountDelta
	d.account, d.namedAccount = payload[accountField]
	_, d.namedList = payload[accountListField]
	return d
}

// stampAccount fills `accountId` on a row of an engine-managed concept, or
// refuses a raw write that tries to state one it may not.
//
// prior is the row as stored before this write, nil on a create. payload is the
// merged row about to be validated and stored, and is mutated in place.
func (e *MemQLEngine) stampAccount(ctx context.Context, conceptMeta *memorynodes.Concept, payload, prior map[string]any, delta accountDelta, fromTemplate bool) error {
	if e == nil || conceptMeta == nil || payload == nil {
		return nil
	}
	decl := conceptMeta.BelongsTo
	if !decl.EngineManaged() {
		return nil
	}

	// BEFORE THE SWITCH, NOBODY WRITES THE FIELD. On an engine-managed concept
	// `accountId` is the engine's to write, and while the switch is off the
	// engine writes it for no one. So an account NAMED in a write then is
	// dropped, whoever named it: the row keeps what it had stored, or has no
	// key. Two things depend on that, and neither waits for the switch.
	//
	// The rows of this release must be ones the previous release can still
	// update, which is the whole reason the switch exists, and a row carrying
	// the key is not one. And a value planted now would be believed later: once
	// the switch is on the stamp keeps an account it finds on a row, the
	// assignment keeps it, and a run acts in it (TriggerAccountTrusted). Every
	// check below would have refused that value; a write made while they were
	// not running must not get past them by coming early.
	//
	// A held node is in this state for good, including after another replica
	// has turned the switch on. The stored value it keeps may then be a real
	// stamp, which is why the stored row is restored and not blanked.
	if !e.AccountStampingEnabled() {
		if delta.namedAccount {
			if v, had := prior[accountField]; had {
				payload[accountField] = v
			} else {
				delete(payload, accountField)
			}
		}
		return nil
	}

	stored := bareAccount(prior[accountField])
	// The raw surface is the one with no author behind it: no template stated
	// an answer, and no trusted Go stamped internal origin for this write.
	internal := auth.OriginFromContext(ctx).IsInternal()
	raw := !fromTemplate && !internal
	// held: the writes on which an account NAMED in the payload is the writer's
	// own input, and so is held to the accounts that writer may write to. The
	// raw surface is one. A person's write through a template is the other: a
	// template that accepts `accountId` passes its caller's argument straight
	// through, so the author decided that an account may be named and the
	// person decided which. Server code is not held (internal origin, or a
	// template run by the cluster or a connector): there the author named it.
	held := raw || (!internal && e.callerIsAPerson(ctx))

	// AN EMPTY ACCOUNT IS NO ACCOUNT (refinement R6). Ten mutations write
	// `accountId: ""` where others write no key, and the two must store the same
	// row. Left as it is, the empty string would overwrite a stored account in
	// the read-merge, and on the raw surface read as a move to "another"
	// account. So the delta is put back to what a write that named nothing
	// would have produced: the stored value where there is one, no key where
	// there is none.
	if delta.namedAccount && bareAccount(delta.account) == "" {
		delta.namedAccount = false
		if v, had := prior[accountField]; had {
			payload[accountField] = v
		} else {
			delete(payload, accountField)
		}
	}

	if held && delta.namedAccount && stored != "" && bareAccount(delta.account) != stored {
		return fmt.Errorf("%s: %s row belongs to an account already, and a write cannot move it to another. "+
			"Moving a row between accounts is a governed act, not an edit of this field",
			codeAccountChangeRefused, conceptMeta.Name)
	}

	// 1. The list decides, where the concept still carries one: on a create,
	// on a row that has no account yet, and when this write CHANGES the list.
	// A write that restates the list it found (a whole-row re-insert does)
	// changes nothing about who the row is for, so it falls through and the
	// stored account is kept; re-deriving there would move the row whenever
	// the call's account differed from the first writer's.
	if conceptDeclaresField(conceptMeta, accountListField) {
		if listed := accountList(payload[accountListField]); len(listed) > 0 {
			changed := delta.namedList && (prior == nil || !sameAccountList(listed, accountList(prior[accountListField])))
			if changed || bareAccount(payload[accountField]) == "" && stored == "" {
				if picked := e.pickPlaceableAccount(ctx, listed); picked != "" {
					payload[accountField] = picked
					return nil
				}
				// None of the tags is an account this caller may place a row in.
				// The list then decides nothing, and the row falls through to the
				// sources below: the account it already records, its parent's, or
				// the caller's own on a create.
			}
		}
	}

	// 2. A value already on the row is kept.
	if current := bareAccount(payload[accountField]); current != "" {
		if held && delta.namedAccount && current != stored {
			if !e.callerMayWriteAccount(ctx, current) {
				return fmt.Errorf("organization_forbidden: you are not a member of the account this %s row names",
					conceptMeta.Name)
			}
			if decl.Via != "" {
				parent, state, err := e.parentAccount(ctx, conceptMeta, payload)
				if err != nil {
					return err
				}
				if state == parentAssigned && parent != current {
					return fmt.Errorf("%s: %s row names a different account than the row its %s field points at. "+
						"A row belongs to the account its parent belongs to",
						codeAccountMismatch, conceptMeta.Name, decl.Via)
				}
			}
		}
		return nil
	}

	// 3. THE STORED ACCOUNT, CARRIED FORWARD. A write to a row that already
	// records an account and whose payload names none keeps that account. The
	// read-merge usually does this already; this is for the writes that replace
	// the whole row instead (a mirror re-sync, a template that re-inserts under
	// the same id), which would otherwise store the new version without it.
	// Copied as stored, so the spelling does not change either.
	if stored != "" {
		payload[accountField] = prior[accountField]
		return nil
	}

	// 4. The parent.
	if decl.Via != "" {
		parent, state, err := e.parentAccount(ctx, conceptMeta, payload)
		if err != nil {
			return err
		}
		switch state {
		case parentAssigned:
			// A PERSON'S RAW WRITE NAMES ITS PARENT WITH NOTHING BUT AN ID. A
			// mutation template that takes a parent id is an author saying who
			// may attach a row to what; the raw literal has no author, and the
			// read above asks nobody's permission. So on that surface a person's
			// row takes its parent's account only when the person may write to
			// that account, the question an explicit account is held to. One who
			// may not is not refused: the row falls through and is recorded
			// under their own account, where an id they guessed files nothing
			// under somebody else's. A connector or the cluster writing a raw
			// row is server code and follows the parent as before.
			if raw && e.callerIsAPerson(ctx) && !e.callerMayWriteAccount(ctx, parent) {
				break
			}
			payload[accountField] = parent
			return nil
		case parentUnassigned:
			// The parent exists and has no account yet. Wait for it: the
			// assignment gives the parent one and this row follows.
			return nil
		}
	}

	// 5. The call's account, on a create only (see the file header).
	if prior == nil {
		if account := e.CallAccount(ctx); account != "" {
			payload[accountField] = account
		}
	}
	return nil
}

type parentAccountState int

const (
	// parentAbsent: the row names no parent, or the row it names does not
	// exist, or its concept is not one whose rows belong to accounts.
	parentAbsent parentAccountState = iota
	// parentUnassigned: the parent exists and has no account.
	parentUnassigned
	// parentAssigned: the parent's account is returned with it.
	parentAssigned
)

// parentAccount reads the account of the row a `via` field names.
//
// The read goes straight to the store (loadPriorPayload): no row authorization,
// because this is the engine asking what a row it is about to write belongs to,
// and no result cache, because that lives in Execute.
func (e *MemQLEngine) parentAccount(ctx context.Context, conceptMeta *memorynodes.Concept, payload map[string]any) (string, parentAccountState, error) {
	decl := conceptMeta.BelongsTo
	raw, _ := payload[decl.Via].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", parentAbsent, nil
	}
	parentConcept := accountParentConcept(conceptMeta)
	if parentConcept == "" {
		// A field with no relationship and no declared parent holds canonical
		// ids of several concepts (an index row's reference). The id names its
		// own concept; a bare id names none and is no parent.
		concept, _, err := coreid.ParseNodeId(raw)
		if err != nil || concept == "" {
			return "", parentAbsent, nil
		}
		parentConcept = concept
	}
	parentMeta, err := memorynodes.Get(parentConcept)
	if err != nil || parentMeta == nil || !parentMeta.AccountScoped() {
		return "", parentAbsent, nil
	}
	parentID := parentConcept + ":" + BareShortId(raw)

	// An engine-managed account never changes, so it is safe to remember. Two
	// kinds of parent can change theirs and are read each time: a stated one (an
	// organization transfer), and one that still carries the list of accounts,
	// whose single account follows the list when the list changes (source 1).
	remember := parentMeta.BelongsTo.EngineManaged() && !conceptDeclaresField(parentMeta, accountListField)
	if remember {
		if account, ok := e.accountParents.get(parentID); ok {
			return account, parentAssigned, nil
		}
	}
	row, exists, err := e.loadPriorPayload(ctx, parentMeta, parentID)
	if err != nil {
		return "", parentAbsent, fmt.Errorf("read the account of %s: %w", parentID, err)
	}
	if !exists {
		return "", parentAbsent, nil
	}
	account := bareAccount(row[accountField])
	if account == "" {
		return "", parentUnassigned, nil
	}
	if remember {
		e.accountParents.put(parentID, account)
	}
	return account, parentAssigned, nil
}

// callerIsAPerson reports whether the actor on ctx is somebody an account can
// be worked out for, as opposed to the cluster, a connector or nobody
// (actorIsAPerson).
func (e *MemQLEngine) callerIsAPerson(ctx context.Context) bool {
	ac, _ := auth.AccessFromContext(ctx)
	return actorIsAPerson(ac)
}

// callerMayWriteAccount is the question the organization rules ask of an
// explicit account: a cluster operator may name any, anyone else only one they
// belong to.
func (e *MemQLEngine) callerMayWriteAccount(ctx context.Context, account string) bool {
	if organizationOperator(ctx) {
		return true
	}
	if scope := e.accountScopeFor(ctx); scope != nil && scope.admits(account) {
		return true
	}
	return false
}

// bareAccount reads an account id off a payload value as its bare short id. An
// absent key, a value that is not a string and an empty string are all "no
// account": ten mutations write the empty string where others write no key.
func bareAccount(v any) string {
	s, _ := v.(string)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return BareShortId(s)
}

// accountList reads the bare ids out of an `accountIds` value.
func accountList(v any) []string {
	var out []string
	add := func(s string) {
		if b := bareAccount(s); b != "" {
			out = append(out, b)
		}
	}
	switch list := v.(type) {
	case []any:
		for _, item := range list {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	case []string:
		for _, s := range list {
			add(s)
		}
	}
	return out
}

// sameAccountList reports whether two lists name the same accounts in the same
// order. Order counts because the first tag is the fallback choice.
func sameAccountList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pickPlaceableAccount chooses, among the accounts a row is tagged with, the one
// the row is recorded under, or "" when none of them is the caller's to use.
//
// THE LIST IS THE CALLER'S OWN INPUT ON EVERY SURFACE. A mutation template for
// one of these concepts passes the caller's `accountIds` straight through, so
// "a template names it, the author decided" does not hold for the list the way
// it does for an explicit `accountId`: the person chose the tags, not the
// author. Recorded as it came, a tag would let a person file a row under an
// account they do not belong to, and that account's inventory and export would
// then carry a row its people never made. So for a person's own write the
// choice is made among the tags the person may write to, by the same question
// an explicit account is held to (callerMayWriteAccount): a cluster operator
// may use any, anybody else only an account they belong to.
//
// What the list itself may contain is not this function's: tagging is older
// than the recorded account, and the tags still decide who the row is shared
// with exactly as before. Only the recorded account is held here.
//
// Server-side writes are not narrowed: internal origin, and an actor that is
// not a person (a run acting as the cluster, a connector, a sweep), write for
// the system, and the call's account already says where they act.
func (e *MemQLEngine) pickPlaceableAccount(ctx context.Context, listed []string) string {
	call := e.CallAccount(ctx)
	ac, _ := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx).IsInternal() || !actorIsAPerson(ac) {
		return pickListedAccount(listed, call)
	}
	placeable := make([]string, 0, len(listed))
	for _, account := range listed {
		if e.callerMayWriteAccount(ctx, account) {
			placeable = append(placeable, account)
		}
	}
	if len(placeable) == 0 {
		return ""
	}
	return pickListedAccount(placeable, call)
}

// pickListedAccount chooses the one account of a row tagged for several: the
// call's own when it is among them, otherwise the first as written.
func pickListedAccount(listed []string, callAccount string) string {
	for _, account := range listed {
		if callAccount != "" && account == callAccount {
			return account
		}
	}
	return listed[0]
}

// conceptDeclaresField reports whether the concept's current definition has a
// top-level property of that name.
func conceptDeclaresField(c *memorynodes.Concept, field string) bool {
	if c == nil {
		return false
	}
	for _, declared := range c.DeclaredFields() {
		if declared == field {
			return true
		}
	}
	return false
}

// accountParentCache remembers the account of parents whose account cannot
// change. It is bounded by forgetting everything when it fills: the entries are
// cheap to read again and a least-recently-used list would cost more than it
// saved on a path every child write takes.
type accountParentCache struct {
	mu sync.Mutex
	m  map[string]string
}

const accountParentCacheLimit = 50000

func (c *accountParentCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	account, ok := c.m[id]
	return account, ok
}

func (c *accountParentCache) put(id, account string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= accountParentCacheLimit {
		c.m = make(map[string]string)
	}
	c.m[id] = account
}
