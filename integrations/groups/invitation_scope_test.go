package groups

import (
	"context"
	"errors"
	"testing"
)

// GroupJoinsOrganization is D10 asked again when an invitation is accepted:
// does a membership of this group make somebody a member of that
// organization? Only an ACTIVE group TIED TO that organization does -- the
// same groups a person's membership is read from once they exist.
func TestGroupJoinsOrganizationOnlyThroughAnActiveGroupOfIt(t *testing.T) {
	i, _ := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, group, account string
		want                 bool
	}{
		{"the organization's own group", "acct-acme", "acme", true},
		{"a custom group tied to it", "g-custom", "acme", true},
		{"a group of another organization", "acct-acme", "other", false},
		{"an archived group", "g-archived", "acme", false},
		{"a group that does not exist", "missing", "acme", false},
		{"no organization named", "acct-acme", "", false},
		{"no group named", "", "acme", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := i.GroupJoinsOrganization(ctx, tc.group, tc.account)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("GroupJoinsOrganization(%q, %q) = %v, want %v", tc.group, tc.account, got, tc.want)
			}
		})
	}
}

// An unreadable group is an ERROR, not a quiet "no": the caller decides, and
// it refuses the acceptance rather than guessing.
func TestGroupJoinsOrganizationReportsAnUnreadableGroup(t *testing.T) {
	failing := New(failingEngine{}, func(string, ...any) {})
	if _, err := failing.GroupJoinsOrganization(context.Background(), "acct-acme", "acme"); err == nil {
		t.Fatal("an unreadable group read as an answer")
	}
}

type failingEngine struct{}

func (failingEngine) Execute(context.Context, string) (any, error) {
	return nil, errors.New("database unavailable")
}
