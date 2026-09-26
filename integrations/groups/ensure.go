package groups

// ensure.go -- the account's own group (epic memql#5165, D5).
//
// Every account gets one, at a DERIVED id, and that is what makes the two
// callers safe to run repeatedly: the `ensureAccountGroup` automation fires on
// account creation, and the seed materializer sweeps every active account on
// every boot. A minted id would have made the second one write a new group per
// boot forever.
//
// Both builtins below are the ENGINE placing rows rather than a person, and
// both declare the capability that says so: `create` on group to mint an
// account's group, `update` on group to archive every group it has. They carry
// no `@sdk` either, but that is a generator marker and not a gate -- see each
// handler.

import (
	"context"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// AccountGroupID derives the account-kind group's id from its account's.
//
// `acct-` rather than a hash, because an operator reading a membership row in
// a log should be able to tell which account it grants without a second query.
func AccountGroupID(accountID string) string {
	return "acct-" + memql.BareShortId(strings.TrimSpace(accountID))
}

// handleGroupEnsureForAccount writes the account-kind group if none is active.
//
// THE CALLER GATE IS `@requiresCapability("create", "group")` ON THE BUILTIN,
// not a check in this function, and the difference is deliberate twice over.
//
// The engine's builtin executor runs it BEFORE this handler, so a caller who
// may not create a group is refused having read nothing -- which is what keeps
// the refusal from distinguishing an account that exists from one that does
// not. And the gate passes INTERNAL ORIGIN, which is what lets both callers
// through -- the ensureAccountGroup automation and the seed materializer's
// boot sweep -- along with any later trusted-Go caller that stamps it. A
// capability check written here would have to re-derive that exemption, and a
// second copy of it is a copy that drifts.
//
// Do NOT restore a `@sdk`-based argument for leaving this open. `@sdk` is a
// generator marker with no engine effect: it decides what sdk/go and sdk/ts
// emit and decides nothing about who may name this builtin over the wire.
func (i *Integration) handleGroupEnsureForAccount(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	accountID := memql.BareShortId(strings.TrimSpace(asString(args["accountId"])))
	if accountID == "" {
		return nil, refusal(CodeAccountNotFound, "an account id is required")
	}
	account, err := i.store.AccountByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, refusal(CodeAccountNotFound, accountID+" names no account")
	}
	// An ARCHIVED account gets no group. The cascade archives the group when
	// the account is archived, so writing one here would undo that on the
	// next boot sweep -- a backfill and a cascade fighting each other, with
	// the boot winning and nobody watching.
	if strings.TrimSpace(rowString(account, "status")) != StatusActive {
		return i.node("groupEnsureForAccount", map[string]any{
			"accountId": accountID, "groupId": "", "created": false,
		})
	}

	groupID := AccountGroupID(accountID)
	existing, err := i.store.GroupByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status == StatusActive {
		// The boot seed uses a placeholder name. Ownership setup names the
		// existing organization; keep its derived group in step without
		// replacing the group or disturbing any memberships.
		name := strings.TrimSpace(rowString(account, "name"))
		if name != "" && existing.Name != name && existing.Kind == KindAccount && existing.AccountID == accountID {
			existing.Name = name
			existing.Description = "Everyone who works on " + name + "."
			if err := i.store.WriteGroup(ctx, *existing); err != nil {
				return nil, err
			}
		}
		return i.node("groupEnsureForAccount", map[string]any{
			"accountId": accountID, "groupId": existing.ID, "created": false,
		})
	}

	name := strings.TrimSpace(rowString(account, "name"))
	if name == "" {
		// An account with no name is a row mid-configuration -- the self
		// account's own seed lands before the first-run card names it. The
		// group still has to exist, so it says what it is rather than
		// carrying an empty name a picker would render as a blank line.
		name = "Account " + accountID
	}
	g := Group{
		ID:          groupID,
		Name:        name,
		Description: "Everyone who works on " + name + ".",
		Kind:        KindAccount,
		AccountID:   accountID,
		Status:      StatusActive,
	}
	if err := i.store.WriteGroup(ctx, g); err != nil {
		return nil, err
	}
	return i.node("groupEnsureForAccount", map[string]any{
		"accountId": accountID, "groupId": g.ID, "created": true,
	})
}

// handleGroupArchiveForAccount is the archiveAccountGroup cascade's entry
// point.
//
// THE CALLER GATE IS `@requiresCapability("update", "group")` ON THE BUILTIN,
// for handleGroupEnsureForAccount's reasons and for one more. `update` on
// group is the verb groupArchive checks, and this is that archive taken over
// every group one account has -- so reaching a client's whole membership in a
// single call takes the same grant as reaching one group by hand.
//
// The authority the CASCADE runs on is still the archive of the ACCOUNT, which
// `@requiresRank("admin")` on archiveClientAccount gates. Note that the two do
// not name the same set: developer ranks 300, clears that floor, and holds no
// group grant at all -- so a developer archives an account and the cascade
// still runs, because it runs as the engine rather than as them.
func (i *Integration) handleGroupArchiveForAccount(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	accountID := memql.BareShortId(strings.TrimSpace(asString(args["accountId"])))
	groups, memberships, err := i.ArchiveGroupsForAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return i.node("groupArchiveForAccount", map[string]any{
		"accountId": accountID, "groupsArchived": groups, "membershipsRemoved": memberships,
	})
}

// ArchiveGroupsForAccount archives every group tied to one account and removes
// their memberships -- the `archiveAccountGroup` cascade (D5).
//
// BOTH KINDS, not just the account-kind one. A custom group tied to an
// archived account grants access to a client that is gone; leaving it active
// would mean the account's archive changed what its screens show and not what
// its people can reach.
func (i *Integration) ArchiveGroupsForAccount(ctx context.Context, accountID string) (groupsArchived, membershipsRemoved int, err error) {
	accountID = memql.BareShortId(strings.TrimSpace(accountID))
	if accountID == "" {
		return 0, 0, refusal(CodeAccountNotFound, "an account id is required")
	}
	tied, err := i.store.GroupsForAccount(ctx, accountID)
	if err != nil {
		return 0, 0, err
	}
	for _, g := range tied {
		if g.Status != StatusActive {
			continue
		}
		removed, err := i.archiveGroup(ctx, g, "")
		if err != nil {
			return groupsArchived, membershipsRemoved, err
		}
		groupsArchived++
		membershipsRemoved += removed
	}
	return groupsArchived, membershipsRemoved, nil
}
