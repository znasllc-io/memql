package memql

// call_account.go -- THE ACCOUNT A CALL ACTS IN (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C).
//
// A row of an account-scoped concept records its account in `accountId`, and
// the design gives that value three possible sources, in order: the row's
// parent, an account the write itself names, and the account of the CALL that
// made the write. This file answers the third, and only answers: it writes
// nothing, and the reads it makes are of who the caller is.
//
// # The answer, in order
//
//  1. WHAT AN ENTRY POINT STATED (auth.ContextWithCallAccount). An automation a
//     row triggered, a shopper form, an access-code visit and a forwarded
//     request each know the account, and their actor cannot say it. Stated, it
//     is the answer whoever the actor is.
//  2. NOBODY: no actor, the anonymous actor, a connector, or the cluster acting
//     for itself. These act in no account. A scheduled sweep or a seed has no
//     account of its own, and its rows follow a parent or stay cluster-provided.
//  3. A CLUSTER OPERATOR acts in the primary account, `self`.
//  4. A PERSON WHOSE ROLE IS SCOPED TO ONE ACCOUNT acts in that account.
//  5. ANYBODY ELSE acts in the account they belong to: the one their active
//     memberships tie them to, the oldest of those when there are several, and
//     the primary account when there are none.
//
// # Why this is not the organization default
//
// validateOrganizationOwnership has a default of its own for the concepts it
// governs, and it REFUSES a person in several accounts ("select one of your
// authorized organizations"). That is right there and wrong here. Those rows are
// shared with their account's members, so a guessed account shows a campaign to
// the wrong people, and the caller is a person in a form who can be asked. This
// answer opens a row to nobody. It is asked for a note being saved or a run
// being journaled, where there is no one to ask and refusing the write would
// break something that works today. So it always answers for a person, and the
// rule it answers by is written down above rather than left to the row.
//
// # A wrong answer is kept
//
// The design has an update carry a row's account forward and nothing rewrite
// it, so an account recorded from this answer stays. This therefore never
// GUESSES: a person the principal table cannot resolve, and a read that failed,
// both answer "", which is no account from the call. Answering the primary
// account in their place would be an assertion nobody made, and "in no group" is
// only ever the result of a read that succeeded.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// CallAccount is the account the call on ctx acts in, as the BARE id, or "" when
// it acts in none or the account could not be established.
//
// "" IS TWO THINGS, and a caller treats them alike: the cluster acting for
// itself, which has no account, and a person whose account could not be read
// just now. In both the row this call writes takes no account from the call.
//
// It costs nothing for a stated account, for nobody, for an operator and for a
// scoped role. Anybody else costs one membership read, which their request has
// usually made already and the request's memo then answers (membershipsFor).
// Two callers cost a read of their own: an actor whose role is a stand-in, the
// principal row that says who they are, and a person in several accounts, the
// read that dates their memberships.
func (e *MemQLEngine) CallAccount(ctx context.Context) string {
	if account, stated := auth.CallAccountFromContext(ctx); stated {
		return account
	}
	ac, _ := auth.AccessFromContext(ctx)
	if e == nil || !actorIsAPerson(ac) {
		return ""
	}

	// THE ROLE THAT DECIDES IS THE PERSON'S, and on two actors the one on the
	// context is not it. Borrowed authority (auth.ContextWithUserActor) and work
	// restored for its owner without a captured grant
	// (auth.ContextWithPersistedOwner) both assert writer for whoever they act
	// for. Read as they stand, an operator's borrowed work would act in whichever
	// client group that operator happens to sit in while their own calls act in
	// the primary account: one person, two answers, decided by which code path
	// wrote the row. So the principal table is asked, the read the account scope
	// and a tool's @requiresRank already make for the same actors
	// (resolveAccountScope, toolFloorContext). A person it does not resolve holds
	// no role anybody can read, and the stand-in is never the fallback: it is
	// the answer this replaces.
	role, operator := ac.Role, organizationOperator(ctx)
	if ac.RoleStandIn || ac.Unranked {
		if role = e.personRole(ctx, ac.UserId); role == "" {
			return ""
		}
		operator = auth.IsClusterOperator(role)
	}
	account, err := e.personAccount(ctx, ac.UserId, role, operator)
	if err != nil {
		// Passed on to a caller that refuses on a failed read
		// (auth.ContextWithReadFailures), and said once here. A context that is
		// already done is the caller going away, not the database failing.
		auth.NoteReadFailure(ctx, err)
		if e.Component != nil && e.Logger != nil && ctx.Err() == nil {
			e.Logger.Warn("call account: the caller's account could not be read, so this call states none",
				"component", ComponentName, "user", BareShortId(ac.UserId), "error", err)
		}
		return ""
	}
	return account
}

// accountOfUser is the account a person acts in when nothing else says, worked
// out from their user id alone: the rule CallAccount applies to a person, for a
// caller that holds a user id and no call. The assignment of existing rows is
// that caller (account_assignment.go): a row written before accounts were
// recorded names its owner, and its account is the one that owner would act in
// today.
//
// The role is the principal table's, read through personRole, never one a
// context asserts. "" with no error is a person the table does not resolve (no
// row, inactive, an invalid role) or an engine with no database: an answer
// nobody can give, which a caller must not read as "in no group".
func (e *MemQLEngine) accountOfUser(ctx context.Context, userId string) (string, error) {
	if e == nil || strings.TrimSpace(userId) == "" {
		return "", nil
	}
	role := e.personRole(ctx, userId)
	if role == "" {
		return "", nil
	}
	return e.personAccount(ctx, userId, role, auth.IsClusterOperator(role))
}

// placeableAccountsOfUser narrows a list of accounts to the ones a person, named
// by id, may place a row in: every one of them for a cluster operator, the one
// account of a role scoped to it, and otherwise the accounts their memberships
// tie them to. It is the by-user-id form of the question the write path asks of
// a caller (callerMayWriteAccount), for the assignment of existing rows, which
// holds an owner's id and no call.
//
// resolved is false for a person the principal table does not resolve, or an
// engine with no database: nobody can say what that person may write to, and
// the caller decides what an unanswerable owner means. An error is a read that
// failed and is never read as "belongs to none".
func (e *MemQLEngine) placeableAccountsOfUser(ctx context.Context, userId string, listed []string) (placeable []string, resolved bool, err error) {
	if e == nil || strings.TrimSpace(userId) == "" {
		return nil, false, nil
	}
	role := e.personRole(ctx, userId)
	if role == "" {
		return nil, false, nil
	}
	if auth.IsClusterOperator(role) {
		return listed, true, nil
	}
	allowed := map[string]bool{}
	if scoped := BareShortId(auth.RoleAccountScope(role)); scoped != "" {
		allowed[scoped] = true
	} else {
		if e.database() == nil {
			return nil, false, nil
		}
		listening, failures := auth.ContextWithReadFailures(ctx)
		held := e.membershipsFor(listening, userId)
		if err := failures.Err(); err != nil {
			return nil, false, fmt.Errorf("reading the memberships: %w", err)
		}
		for _, id := range held.accountIds {
			if bare := BareShortId(id); bare != "" {
				allowed[bare] = true
			}
		}
	}
	placeable = make([]string, 0, len(listed))
	for _, account := range listed {
		if allowed[account] {
			placeable = append(placeable, account)
		}
	}
	return placeable, true, nil
}

// personAccount is rules 3 to 5 of the file header for one person whose role is
// already known: the primary account for a cluster operator, the account of a
// role scoped to one, else the account their memberships tie them to. The one
// copy of those rules, shared by CallAccount and accountOfUser so a person's
// call and the assignment of their old rows cannot disagree.
func (e *MemQLEngine) personAccount(ctx context.Context, userId string, role auth.Role, operator bool) (string, error) {
	if operator {
		return primaryAccountBareId(), nil
	}

	// A SCOPED ROLE IS ITS ACCOUNT'S, and it decides without the memberships. A
	// role scoped to one account bounds everything its holder reaches to that
	// account (resolveAccountScope drops their memberships anywhere else), so an
	// older membership in another account must not win here, or the row would
	// record an account its author is not admitted to through that scope. Nor
	// does a missing membership send them to the primary account: the role is
	// the account's whether or not a membership row says so today, and the
	// primary account is the operator's own organization, the one place a
	// client's person certainly does not belong.
	if scoped := BareShortId(auth.RoleAccountScope(role)); scoped != "" {
		return scoped, nil
	}

	if e.database() == nil {
		// "In no group" is an answer only a read can give, and this engine
		// cannot make one.
		return "", nil
	}
	return e.memberAccount(ctx, userId)
}

// actorIsAPerson reports whether an actor is somebody an account can be worked
// out for. It reads the constructors' own flags and never the shape of an id:
// a prefix exists so a stored value is legible, and is not a protocol
// (auth.IsAnonymousActor says the same of its own).
//
// Either half of a named actor is enough to say no. An anonymous flag without
// its role, or a connector name on some other role, is a malformed actor, and a
// malformed actor is still not a person.
func actorIsAPerson(ac *auth.AccessContext) bool {
	if ac == nil || strings.TrimSpace(ac.UserId) == "" {
		return false
	}
	// Synthetic is the cluster acting: a maintenance sweep, the seed
	// materializer, an automation's own actor. Several of them carry RoleOwner
	// for the cluster-owner escape, which is why this is asked before the
	// operator rule and not left to it.
	if ac.Synthetic {
		return false
	}
	if ac.IsAnonymous || ac.Role == auth.RoleAnonymous {
		return false
	}
	if strings.TrimSpace(ac.ConnectorName) != "" || ac.Role == auth.RoleConnector {
		return false
	}
	return true
}

// primaryAccountBareId is the cluster's own account, in the spelling CallAccount
// answers in.
func primaryAccountBareId() string { return BareShortId(selfAccountId) }

// memberAccount is the account a person belongs to by their memberships: none is
// the primary account, one is that one, and several is the oldest.
//
// AN ERROR MEANS THE ANSWER IS NOT KNOWN, and is never read as "in no group".
// The membership reader itself answers no groups when its read fails, which is
// the tolerant answer a gate wants: a row withheld for one request. Here the
// same answer would name the primary account, which withholds nothing and
// asserts something. So the read runs under a holder that hears a failure.
//
// What the holder cannot hear is a failure from EARLIER in the same request:
// the memo keeps the answer and not whether its read succeeded, so a membership
// answer memoised before this call is taken as it stands.
func (e *MemQLEngine) memberAccount(ctx context.Context, userId string) (string, error) {
	listening, failures := auth.ContextWithReadFailures(ctx)
	held := e.membershipsFor(listening, userId)
	if err := failures.Err(); err != nil {
		return "", fmt.Errorf("reading the memberships: %w", err)
	}

	// Bare, so the two spellings of one account are one account: counting
	// spellings would send a person with one account down the several-accounts
	// branch, and there a tie between the two would be broken by which spelling
	// sorts first.
	accounts := make(map[string]struct{}, len(held.accountIds))
	for _, id := range held.accountIds {
		if bare := BareShortId(id); bare != "" {
			accounts[bare] = struct{}{}
		}
	}
	switch len(accounts) {
	case 0:
		return primaryAccountBareId(), nil
	case 1:
		for only := range accounts {
			return only, nil
		}
	}
	return e.oldestMembershipAccount(ctx, userId, held, accounts)
}

// errNoDatedMembership is the several-accounts branch finding no membership to
// date: the memberships changed between the read that counted them and the read
// that dated them.
var errNoDatedMembership = errors.New("no active membership could be dated")

// oldestMembershipAccount picks, among several accounts, the one this person
// joined first, and breaks a tie on the account id so the answer never depends
// on the order rows came back in.
//
// "JOINED" IS THE FIRST VERSION OF THE MEMBERSHIP ROW. A membership has no
// joined-at field, and its id is derived from its group and its person, so there
// is one row for the life of that pair: leaving and returning, or any later
// edit, writes a new VERSION. The newest version's time is therefore the last
// time anything about the membership changed, and a reader that used it would
// move a person's work to a different account whenever one of their memberships
// was touched. The window below takes the oldest version's time and DISTINCT ON
// keeps the newest version's payload, which is the one that says whether the
// membership is active.
//
// The whole concept is read (membershipFirstVersions) and the person is matched
// here, in both spellings, exactly as activeGroupIdsForUserChecked matches them:
// the two reads have to agree about whose row a row is. It is one statement,
// and it runs only for a person in several accounts whose call stated none.
func (e *MemQLEngine) oldestMembershipAccount(ctx context.Context, userId string, held memberships, accounts map[string]struct{}) (string, error) {
	db := e.database()
	if db == nil {
		return "", errors.New("no database")
	}
	rows, err := membershipFirstVersions(ctx, db)
	if err != nil {
		return "", fmt.Errorf("reading when the memberships began: %w", err)
	}

	bareUser := BareShortId(userId)
	joined := make(map[string]time.Time, len(accounts))
	for _, row := range rows {
		var payload map[string]any
		if json.Unmarshal(row.Payload, &payload) != nil || payload == nil {
			continue
		}
		if strings.TrimSpace(stringFromAny(payload["status"])) != "active" {
			continue
		}
		rowUser := strings.TrimSpace(stringFromAny(payload["userId"]))
		if rowUser == "" || (rowUser != userId && BareShortId(rowUser) != bareUser) {
			continue
		}
		// Through the ACTIVE groups the membership read resolved, never the
		// account copied onto the membership row: a group is archived, and an
		// old membership row was written before the copy existed, without the
		// row being rewritten.
		account := held.groupAccounts[BareShortId(strings.TrimSpace(stringFromAny(payload["groupId"])))]
		if _, counted := accounts[account]; !counted {
			continue
		}
		if at, seen := joined[account]; !seen || row.Joined.Before(at) {
			joined[account] = row.Joined
		}
	}

	oldest := ""
	for account, at := range joined {
		if oldest == "" || at.Before(joined[oldest]) || (at.Equal(joined[oldest]) && account < oldest) {
			oldest = account
		}
	}
	if oldest == "" {
		return "", errNoDatedMembership
	}
	return oldest, nil
}

// membershipFirstVersion is one membership row: its newest version's payload,
// and the time of its first.
type membershipFirstVersion struct {
	Payload json.RawMessage `bun:"payload"`
	Joined  time.Time       `bun:"joined"`
}

// membershipFirstVersions reads every membership row once, newest payload and
// oldest time together. oldestMembershipAccount says why both are needed.
//
// staged-data: MUST-NOT-GATE -- membership is authorization evidence whether or
// not its concept is staged (activeGroupIdsForUserChecked's ruling), and the
// memberships that placed this person in several accounts were read ungated.
// Gating this read would date their accounts from fewer rows than placed them,
// and the account their rows record would be chosen from what was left.
func membershipFirstVersions(ctx context.Context, db *bun.DB) ([]membershipFirstVersion, error) {
	var rows []membershipFirstVersion
	err := db.NewRaw(
		`SELECT DISTINCT ON (id) payload, min("createdAt") OVER (PARTITION BY id) AS joined
		   FROM "MemoryNodes"
		  WHERE concept = ?
		  ORDER BY id, "createdAt" DESC`,
		conceptIdentityGroupMembership,
	).Scan(ctx, &rows)
	return rows, err
}
