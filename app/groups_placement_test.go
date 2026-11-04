package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/groups"
)

// groupRowsEngine answers groupById from fixed rows, in the envelope the
// groups store reads, and fails every read when `err` is set.
type groupRowsEngine struct {
	rows map[string]map[string]any
	err  error
}

func (e groupRowsEngine) Execute(_ context.Context, q string) (any, error) {
	if e.err != nil {
		return nil, e.err
	}
	for id, row := range e.rows {
		if strings.HasPrefix(q, "query groupById(") && strings.Contains(q, `"`+id+`"`) {
			return map[string]any{"rows": []any{row}}, nil
		}
	}
	return map[string]any{"rows": []any{}}, nil
}

// The acceptance seam's D10 question, through the placer the node wires in:
// one carried group that is an active group of the organization is enough,
// and anything unreadable is a no.
func TestInvitationJoinsOrganizationThroughOneActiveGroupOfIt(t *testing.T) {
	rows := map[string]map[string]any{
		"acct-org-a": {"id": "acct-org-a", "name": "A", "kind": "account", "accountId": "org-a", "status": "active"},
		"old-org-a":  {"id": "old-org-a", "name": "Old", "kind": "custom", "accountId": "org-a", "status": "archived"},
		"acct-org-b": {"id": "acct-org-b", "name": "B", "kind": "account", "accountId": "org-b", "status": "active"},
	}
	for _, tc := range []struct {
		name   string
		groups []string
		err    error
		want   bool
	}{
		{name: "it carries the organization's active group", groups: []string{"acct-org-b", "acct-org-a"}, want: true},
		{name: "its only group of the organization is archived", groups: []string{"old-org-a"}},
		{name: "it carries another organization's group", groups: []string{"acct-org-b"}},
		{name: "it carries no group"},
		{name: "the groups cannot be read", groups: []string{"acct-org-a"}, err: errors.New("database unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &groupPlacer{groups: groups.New(groupRowsEngine{rows: rows, err: tc.err}, func(string, ...any) {})}
			if got := p.invitationJoinsOrganization(context.Background(), tc.groups, "org-a"); got != tc.want {
				t.Fatalf("invitationJoinsOrganization(%v, org-a) = %v, want %v", tc.groups, got, tc.want)
			}
		})
	}
	var nilPlacer *groupPlacer
	if nilPlacer.invitationJoinsOrganization(context.Background(), []string{"acct-org-a"}, "org-a") {
		t.Fatal("a node with no groups integration answered yes")
	}
}
