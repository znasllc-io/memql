package adminops

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type accountInvitationEngine struct {
	issuingEngine
	role             string
	account          string
	authorityContext *auth.AccessContext
}

func (e *accountInvitationEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if strings.HasPrefix(query, "builtin accountAccess(") {
		e.authorityContext, _ = auth.AccessFromContext(ctx)
		return shapedRows(map[string]any{"accountId": e.account, "actorRole": e.role, "groups": []any{map[string]any{"id": "acct-client"}, map[string]any{"id": "reviewers"}}}), nil
	}
	if strings.HasPrefix(query, "query groupById(") {
		return shapedRows(map[string]any{"id": "acct-client", "kind": "account", "status": "active", "accountId": "client"}), nil
	}
	return e.issuingEngine.Execute(ctx, query)
}

func TestAccountManagerInvitationIsBoundedByCurrentAccountAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, actorRole, grant, account string
		groups                          []string
		allowed                         bool
	}{
		{"owner", "owner", "viewer", "client", []string{"acct-client"}, true},
		{"developer", "developer", "viewer", "client", []string{"acct-client", "reviewers"}, true},
		{"admin", "admin", "viewer", "client", []string{"acct-client"}, true},
		{"member", "member", "viewer", "client", []string{"acct-client"}, false},
		{"wrong account answer", "owner", "viewer", "other", []string{"acct-client"}, false},
		{"foreign group", "owner", "viewer", "client", []string{"acct-client", "foreign"}, false},
		{"another canonical group", "owner", "viewer", "client", []string{"acct-client", "acct-other"}, false},
		{"primary account", "owner", "viewer", "self", []string{"acct-self"}, false},
		{"cluster elevation", "owner", "developer", "client", []string{"acct-client"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _ := newIssuingService(t, nil)
			eng := &accountInvitationEngine{role: tc.actorRole, account: tc.account}
			svc.Engine = eng
			result := svc.IssueUserInvitation(ctxAs(auth.Role("viewer")), UserInvitation{Email: "new@example.test", Role: tc.grant, GroupIds: tc.groups})
			require.Equal(t, tc.allowed, result.OK, result.ErrorMessage)
			if tc.allowed {
				require.NotNil(t, eng.authorityContext)
				require.False(t, eng.authorityContext.Synthetic)
				require.False(t, eng.authorityContext.Unranked)
				require.Equal(t, auth.Role("viewer"), eng.authorityContext.Role, "local ownership must not become a global owner")
				require.NotEmpty(t, eng.queries)
			} else {
				require.Empty(t, eng.queries, "a refused delegation must not mint an invitation")
			}
		})
	}
}
