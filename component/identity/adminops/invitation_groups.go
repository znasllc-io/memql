package adminops

// invitation_groups.go -- the groups an invitation carries (epic memql#5165,
// section G).
//
// An invitation may name the groups its recipient JOINS on acceptance, and
// every one of them is validated HERE, at issue, rather than at acceptance.
//
// # Why at issue
//
// Two reasons, and the second is the one that matters. The obvious one is that
// a refusal at issue reaches the person who can act on it -- the inviter, who
// chose the groups -- while a refusal at acceptance reaches the invitee, who
// cannot. The load-bearing one is the RANK rule: an invitation must not place
// somebody where the inviter could not have placed them by hand, and the
// inviter is only present at issue. Checked at acceptance it would be checked
// against the invitee, which is a different question with a different answer.
//
// # The rank rule is the same one groupMemberAdd applies
//
// The invitee's role must rank strictly below the inviter's. That is not a
// second policy: an invitation carrying a group is a deferred groupMemberAdd,
// and letting it skip the rule would make "invite them into the group" the way
// round a guard that refuses "add them to the group".

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// invitationGroups is the validated result: the ids to store, and the account
// tie derived from them.
type invitationGroups struct {
	ids       []string
	accountID string
}

// validateInvitationGroups checks every named group and derives the tie.
//
// Refuses on the FIRST problem rather than collecting them, because the
// caller is a picker in a UI: it offered these groups, so a refusal means its
// list is stale, and naming one stale entry is as actionable as naming five.
func (s *Service) validateInvitationGroups(
	ctx context.Context,
	groupIDs []string,
	inviterRank int,
	inviteeRole string,
) (invitationGroups, string, error) {
	var out invitationGroups
	if len(groupIDs) == 0 {
		return out, "", nil
	}

	// THE RANK RULE, checked ONCE for the whole list rather than per group:
	// it is a fact about the two people, not about any group.
	inviteeSlug := strings.ToLower(strings.TrimSpace(inviteeRole))
	if auth.RoleRank(auth.Role(inviteeSlug)) >= inviterRank {
		return out, "invitee_rank_not_below_inviter", fmt.Errorf(
			"identity admin: an invitation cannot place somebody into a group unless they rank "+
				"strictly below you -- %q does not rank below your own role. This is the same rule "+
				"groupMemberAdd applies, and an invitation carrying a group is a deferred one",
			inviteeSlug)
	}

	seen := make(map[string]struct{}, len(groupIDs))
	for _, raw := range groupIDs {
		id := memql.BareShortId(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			// A duplicate is DROPPED rather than refused. The membership id
			// is derived from (group, user), so joining twice writes one row
			// either way -- refusing would be a rule with no consequence
			// behind it.
			continue
		}
		seen[id] = struct{}{}

		group, err := s.groupByID(ctx, id)
		if err != nil {
			return out, "", err
		}
		if group == nil {
			return out, "group_not_found", fmt.Errorf(
				"identity admin: %s names no group", id)
		}
		if strings.TrimSpace(rowString(group, "status")) != "active" {
			return out, "group_not_active", fmt.Errorf(
				"identity admin: %s is archived, and an archived group grants nothing", id)
		}
		out.ids = append(out.ids, id)

		// The account tie comes from the FIRST account-kind group named, and
		// from nothing else. A custom group tied to an account does not set
		// it: the tie on an invitation answers "which client is this person
		// being invited on behalf of", and a custom group is an arrangement
		// somebody made rather than the client relationship itself.
		if out.accountID == "" && strings.TrimSpace(rowString(group, "kind")) == "account" {
			out.accountID = memql.BareShortId(strings.TrimSpace(rowString(group, "accountId")))
		}
	}
	return out, "", nil
}

// invitationJoinsAccount answers D10 for an invitation: does accepting it make
// the recipient a member of `accountID`? It does when one of the groups it
// carries is an ACTIVE group of that organization -- an account group or a
// custom group tied to it, the same groups a person's membership is read from
// once they exist (targetIsAccountMember).
//
// A group that cannot be read answers "no" for the whole invitation: a scoped
// role issued on an unchecked group is the failure D10 exists to prevent, and
// validateInvitationGroups refuses the unreadable group a moment later anyway.
func (s *Service) invitationJoinsAccount(ctx context.Context, groupIDs []string, accountID string) bool {
	accountID = memql.BareShortId(strings.TrimSpace(accountID))
	if accountID == "" {
		return false
	}
	joins := false
	for _, raw := range groupIDs {
		id := memql.BareShortId(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		group, err := s.groupByID(ctx, id)
		if err != nil {
			return false
		}
		if group != nil && rowString(group, "status") == "active" &&
			memql.BareShortId(rowString(group, "accountId")) == accountID {
			joins = true
		}
	}
	return joins
}

// groupByID reads one group under internal origin.
//
// groupById is @requiresRank("admin") and this path has already refused
// anybody below that (authorizeAdmission runs first), so the floor is
// satisfied by the caller rather than bypassed here.
func (s *Service) groupByID(ctx context.Context, groupID string) (map[string]any, error) {
	res, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx),
		fmt.Sprintf(`query groupById(groupId: %s)`, langparser.QuoteString(groupID)))
	if err != nil {
		return nil, fmt.Errorf("identity admin: reading group %s: %w", groupID, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

func rowString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// inviterRankOf resolves the acting inviter's rung, case-folded.
//
// FOLDED for the reason principalOf folds: AccessContext.Role is stamped
// straight off the user row, and an unfolded value ranks 0 -- which for the
// INVITER is the fail-closed direction, and the same slip on the invitee side
// fails open, so both sides fold.
func inviterRankOf(act actor) int {
	return auth.RoleRank(auth.Role(strings.ToLower(strings.TrimSpace(string(act.role)))))
}

// renderStringList renders a MemQL list literal of quoted strings.
//
// Hand-built rather than JSON-marshalled so every element goes through
// langparser.QuoteString: Go's own quoting and MemQL's lexer diverge on four
// control bytes, and a group id is an engine-minted short id today but a
// renderer must not depend on that staying true.
func renderStringList(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, quote(v))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
