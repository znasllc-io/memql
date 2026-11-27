package adminops

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// A client manager may invite a Viewer into their account. The ordinary
// credential issue/delivery/redeem lifecycle stays in charge; this narrower
// admission does not grant cluster principal administration. Roles are assigned
// after arrival through the account's transactional membership API.
func (s *Service) accountInvitationAuthority(ctx context.Context, role string, groupIDs []string) (actor, auth.AccountRole, bool) {
	act, ok := resolveActor(ctx)
	if !ok || act.userID == "" || (role != "" && role != "viewer" && role != "reader") || len(groupIDs) == 0 || len(groupIDs) > 100 {
		return actor{}, "", false
	}
	account := ""
	for _, raw := range groupIDs {
		gid := memql.BareShortId(raw)
		if strings.HasPrefix(gid, "acct-") {
			next := strings.TrimPrefix(gid, "acct-")
			if next == "self" || next == "" || (account != "" && account != next) {
				return actor{}, "", false
			}
			account = next
		}
	}
	if account == "" {
		return actor{}, "", false
	}
	// Deliberately the caller's own context, with no internal-origin or
	// synthetic elevation. The engine resolves current account authority.
	result, err := s.Engine.Execute(ctx, fmt.Sprintf("builtin accountAccess(accountId: %s)", langparser.QuoteString(account)))
	if err != nil {
		return actor{}, "", false
	}
	rows := memql.MaterializeRows(result)
	if len(rows) != 1 {
		return actor{}, "", false
	}
	state := rows[0]
	if payload, ok := state["payload"].(map[string]any); ok {
		state = payload
	}
	localRole := auth.AccountRole(rowString(state, "actorRole"))
	if auth.AccountRoleRank(localRole) < auth.AccountRoleRank(auth.AccountAdmin) || memql.BareShortId(rowString(state, "accountId")) != account {
		return actor{}, "", false
	}
	allowed := map[string]bool{}
	if groups, ok := state["groups"].([]any); ok {
		for _, raw := range groups {
			if g, ok := raw.(map[string]any); ok {
				allowed[memql.BareShortId(rowString(g, "id"))] = true
			}
		}
	}
	for _, gid := range groupIDs {
		if !allowed[memql.BareShortId(gid)] {
			return actor{}, "", false
		}
	}
	return act, localRole, true
}
