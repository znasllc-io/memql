package groups

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// The six verbs, driven end to end against a stub graph.
//
// The stub ANSWERS reads rather than only recording writes, which is what
// makes a guard testable at all: every refusal below depends on what a row
// says. render_parse_test.go is the second level that keeps the statements
// this stub accepts honest against the real front end.

// The rows are []any of map[string]any, NOT []map[string]any, and that is a
// faithfulness requirement rather than a style choice: MaterializeRows'
// envelope walk matches `[]any` and `map[string]any` and falls through a
// `[]map[string]any`, so a stub returning the tidier type reads as an EMPTY
// RESULT and every guard below refuses for the wrong reason. The real engine
// produces `[]any`, so the stub does too.
type stubEngine struct {
	groups   map[string]map[string]any
	accounts map[string]map[string]any
	users    map[string]map[string]any
	members  map[string][]any
	// joinDomains maps a domain to the account that has proven it and
	// switched joining on -- the three conditions accountForDomainJoin
	// tests, held here as the answer the query would give.
	joinDomains map[string]string
	writes      []string
}

func newStub() *stubEngine {
	return &stubEngine{
		groups:      map[string]map[string]any{},
		accounts:    map[string]map[string]any{},
		users:       map[string]map[string]any{},
		members:     map[string][]any{},
		joinDomains: map[string]string{},
	}
}

func (s *stubEngine) Execute(_ context.Context, query string) (any, error) {
	switch {
	case strings.HasPrefix(query, "query groupById("):
		return rowsFor(s.groups[argOf(query, "groupId")]), nil
	case strings.HasPrefix(query, "query clientAccountById("):
		return rowsFor(s.accounts[argOf(query, "accountId")]), nil
	case strings.HasPrefix(query, "query userByIdSystem("):
		return rowsFor(s.users[argOf(query, "userId")]), nil
	case strings.HasPrefix(query, "query membersOfGroup("):
		return map[string]any{"rows": s.members[argOf(query, "groupId")]}, nil
	case strings.HasPrefix(query, "query accountForDomainJoin("):
		account := s.joinDomains[argOf(query, "domain")]
		if account == "" {
			return map[string]any{"rows": []any{}}, nil
		}
		return map[string]any{"rows": []any{map[string]any{"id": account}}}, nil
	case strings.HasPrefix(query, "query groupsForAccount("):
		var out []any
		for _, g := range s.groups {
			if g["accountId"] == argOf(query, "accountId") {
				out = append(out, g)
			}
		}
		return map[string]any{"rows": out}, nil
	default:
		s.writes = append(s.writes, query)
		return map[string]any{"rows": []map[string]any{}}, nil
	}
}

func rowsFor(row map[string]any) any {
	if row == nil {
		return map[string]any{"rows": []any{}}
	}
	return map[string]any{"rows": []any{row}}
}

// argOf reads one quoted argument out of a rendered call.
func argOf(query, name string) string {
	marker := name + ": \""
	i := strings.Index(query, marker)
	if i < 0 {
		return ""
	}
	rest := query[i+len(marker):]
	if j := strings.Index(rest, "\""); j >= 0 {
		return rest[:j]
	}
	return ""
}

func (s *stubEngine) writesNamed(fn string) []string {
	var out []string
	for _, w := range s.writes {
		if strings.HasPrefix(w, "mutation "+fn+"(") {
			out = append(out, w)
		}
	}
	return out
}

func newFixture(t *testing.T) (*Integration, *stubEngine) {
	t.Helper()
	stub := newStub()
	stub.accounts["acme"] = map[string]any{"id": "acme", "name": "Acme", "status": StatusActive}
	stub.accounts["gone"] = map[string]any{"id": "gone", "name": "Gone", "status": StatusArchived}
	stub.groups["g-custom"] = map[string]any{
		"id": "g-custom", "name": "Team", "kind": KindCustom, "accountId": "acme", "status": StatusActive,
	}
	stub.groups["acct-acme"] = map[string]any{
		"id": "acct-acme", "name": "Acme", "kind": KindAccount, "accountId": "acme", "status": StatusActive,
	}
	stub.groups["g-archived"] = map[string]any{
		"id": "g-archived", "name": "Old", "kind": KindCustom, "accountId": "", "status": StatusArchived,
	}
	stub.users["u-writer"] = map[string]any{"id": "u-writer", "role": "writer"}
	stub.users["u-admin2"] = map[string]any{"id": "u-admin2", "role": "admin"}
	stub.users["u-dev"] = map[string]any{"id": "u-dev", "role": "developer"}
	return New(stub, func(string, ...any) {}), stub
}

func adminCtx() context.Context  { return callerCtx("u-admin", auth.RoleAdmin) }
func memberCtx() context.Context { return callerCtx("u-writer", auth.RoleWriter) }

// ---------------------------------------------------------------------

func TestEveryVerbRefusesAMember(t *testing.T) {
	// A Member holds `read` on group and nothing else, so every one of the
	// five caller-facing verbs must refuse them -- including the two whose
	// guard is `create` rather than `update`.
	i, _ := newFixture(t)
	ctx := memberCtx()
	cases := map[string]map[string]any{
		"groupCreate":    {"name": "Mine"},
		"groupUpdate":    {"groupId": "g-custom", "name": "Renamed"},
		"groupArchive":   {"groupId": "g-custom"},
		"groupMemberAdd": {"groupId": "g-custom", "userId": "u-dev"},
	}
	for verb, args := range cases {
		t.Run(verb, func(t *testing.T) {
			var err error
			switch verb {
			case "groupCreate":
				_, err = i.handleGroupCreate(ctx, args, 0)
			case "groupUpdate":
				_, err = i.handleGroupUpdate(ctx, args, 0)
			case "groupArchive":
				_, err = i.handleGroupArchive(ctx, args, 0)
			case "groupMemberAdd":
				_, err = i.handleGroupMemberAdd(ctx, args, 0)
			}
			if err == nil {
				t.Fatalf("%s admitted a member", verb)
			}
			if RefusalCode(err) != CodeCapabilityMissing {
				t.Fatalf("%s: code %q, want %q", verb, RefusalCode(err), CodeCapabilityMissing)
			}
		})
	}
	// The fifth verb is the exception, and it is the one asymmetry in D7: a
	// person may always remove THEMSELVES, capability or not.
	if _, err := i.handleGroupMemberRemove(ctx, map[string]any{
		"groupId": "g-custom", "userId": "u-writer",
	}, 0); err != nil {
		t.Fatalf("a member removing themselves: want admit, got %v", err)
	}
}

func TestAnAdminMayDoEachVerb(t *testing.T) {
	// The positive control for the test above. Without it, every assertion
	// there would pass against verbs that refuse everybody.
	i, stub := newFixture(t)
	ctx := adminCtx()
	if _, err := i.handleGroupCreate(ctx, map[string]any{"name": "New", "accountId": "acme"}, 0); err != nil {
		t.Fatalf("groupCreate: %v", err)
	}
	if _, err := i.handleGroupUpdate(ctx, map[string]any{"groupId": "g-custom", "name": "Renamed"}, 0); err != nil {
		t.Fatalf("groupUpdate: %v", err)
	}
	if _, err := i.handleGroupMemberAdd(ctx, map[string]any{"groupId": "g-custom", "userId": "u-writer"}, 0); err != nil {
		t.Fatalf("groupMemberAdd: %v", err)
	}
	if _, err := i.handleGroupMemberRemove(ctx, map[string]any{"groupId": "g-custom", "userId": "u-writer"}, 0); err != nil {
		t.Fatalf("groupMemberRemove: %v", err)
	}
	if _, err := i.handleGroupArchive(ctx, map[string]any{"groupId": "g-custom"}, 0); err != nil {
		t.Fatalf("groupArchive: %v", err)
	}
	if len(stub.writesNamed("writeGroup")) == 0 || len(stub.writesNamed("writeGroupMembership")) == 0 {
		t.Fatal("no rows were written, so the admissions above proved nothing")
	}
}

func TestGroupCreateAlwaysWritesACustomKind(t *testing.T) {
	// A caller who could mint an ACCOUNT-kind group could split an account's
	// membership across two rows with no way to tell which one grants.
	i, stub := newFixture(t)
	if _, err := i.handleGroupCreate(adminCtx(), map[string]any{
		"name": "Sneaky", "accountId": "acme", "kind": KindAccount,
	}, 0); err != nil {
		t.Fatal(err)
	}
	writes := stub.writesNamed("writeGroup")
	if len(writes) != 1 {
		t.Fatalf("want one write, got %d", len(writes))
	}
	if !strings.Contains(writes[0], `kind: "custom"`) {
		t.Fatalf("groupCreate wrote %s, want kind custom regardless of what the caller asked for", writes[0])
	}
}

func TestGroupCreateRefusesAnAbsentOrArchivedAccount(t *testing.T) {
	i, _ := newFixture(t)
	if _, err := i.handleGroupCreate(adminCtx(), map[string]any{"name": "X", "accountId": "nope"}, 0); err == nil {
		t.Fatal("an unknown account: want a refusal")
	} else if RefusalCode(err) != CodeAccountNotFound {
		t.Fatalf("code %q, want %q", RefusalCode(err), CodeAccountNotFound)
	}
	if _, err := i.handleGroupCreate(adminCtx(), map[string]any{"name": "X", "accountId": "gone"}, 0); err == nil {
		t.Fatal("an archived account: want a refusal")
	} else if RefusalCode(err) != CodeAccountNotActive {
		t.Fatalf("code %q, want %q", RefusalCode(err), CodeAccountNotActive)
	}
	// Newly created groups always have an explicitly selected organization.
	if _, err := i.handleGroupCreate(adminCtx(), map[string]any{"name": "Just organising"}, 0); err == nil {
		t.Fatal("a group with no organization was accepted")
	}
}

func TestNobodyAddsThemselves(t *testing.T) {
	i, stub := newFixture(t)
	stub.users["u-admin"] = map[string]any{"id": "u-admin", "role": "admin"}
	_, err := i.handleGroupMemberAdd(adminCtx(), map[string]any{"groupId": "g-custom", "userId": "u-admin"}, 0)
	if err == nil {
		t.Fatal("self-add: want a refusal")
	}
	if RefusalCode(err) != CodeSelfAddRefused {
		t.Fatalf("code %q, want %q -- and the code matters: self-add refused by the RANK rule "+
			"would say \"does not rank below your own\", which is true and a confusing way to "+
			"say \"not this way\"", RefusalCode(err), CodeSelfAddRefused)
	}
	if len(stub.writesNamed("writeGroupMembership")) != 0 {
		t.Fatal("a refused self-add still wrote a membership")
	}
}

func TestAnAdminMayNotAddADeveloperOrAPeer(t *testing.T) {
	// developer ranks 300, ABOVE admin's 200 -- the ordering memql#4833
	// established. Every requirement written under the old OS ladder changed
	// meaning when that landed, and this is the one that would have read as
	// "an admin manages developers".
	i, _ := newFixture(t)
	for _, target := range []string{"u-dev", "u-admin2"} {
		_, err := i.handleGroupMemberAdd(adminCtx(), map[string]any{"groupId": "g-custom", "userId": target}, 0)
		if err == nil {
			t.Fatalf("an admin adding %s: want a refusal", target)
		}
		if RefusalCode(err) != CodeRankNotBelowCaller {
			t.Fatalf("%s: code %q, want %q", target, RefusalCode(err), CodeRankNotBelowCaller)
		}
	}
	// ...and somebody genuinely below is admitted, or the rule above is a
	// refusal of everything.
	if _, err := i.handleGroupMemberAdd(adminCtx(), map[string]any{"groupId": "g-custom", "userId": "u-writer"}, 0); err != nil {
		t.Fatalf("an admin adding a writer: want admit, got %v", err)
	}
}

func TestAddingAnUnknownUserIsRefused(t *testing.T) {
	i, _ := newFixture(t)
	_, err := i.handleGroupMemberAdd(adminCtx(), map[string]any{"groupId": "g-custom", "userId": "ghost"}, 0)
	if err == nil {
		t.Fatal("an unknown user: want a refusal")
	}
	if RefusalCode(err) != CodeTargetUserNotFound {
		t.Fatalf("code %q, want %q -- an unresolvable role ranks 0, which reads as the least "+
			"privileged person in the cluster and would admit every ghost id",
			RefusalCode(err), CodeTargetUserNotFound)
	}
}

func TestReAddingWritesTheSameDerivedId(t *testing.T) {
	// D11: history is the versions. Two rows for one (group, user) pair
	// would make "is this person a member" a question with two answers.
	i, stub := newFixture(t)
	args := map[string]any{"groupId": "g-custom", "userId": "u-writer"}
	if _, err := i.handleGroupMemberAdd(adminCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleGroupMemberRemove(adminCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := i.handleGroupMemberAdd(adminCtx(), args, 0); err != nil {
		t.Fatal(err)
	}
	writes := stub.writesNamed("writeGroupMembership")
	if len(writes) != 3 {
		t.Fatalf("want three writes, got %d", len(writes))
	}
	want := `membershipId: "g-custom-u-writer"`
	for n, w := range writes {
		if !strings.Contains(w, want) {
			t.Fatalf("write %d does not carry the derived id %s:\n  %s", n, want, w)
		}
	}
	if !strings.Contains(writes[1], `status: "removed"`) || !strings.Contains(writes[1], "removedBy") {
		t.Fatalf("the removal did not stamp its own fields: %s", writes[1])
	}
	if !strings.Contains(writes[2], `status: "active"`) {
		t.Fatalf("the re-add did not restore active: %s", writes[2])
	}
}

func TestAnAccountKindGroupIsNotArchivableWhileItsAccountIsActive(t *testing.T) {
	i, _ := newFixture(t)
	_, err := i.handleGroupArchive(adminCtx(), map[string]any{"groupId": "acct-acme"}, 0)
	if err == nil {
		t.Fatal("archiving an account's own group while the account is live: want a refusal")
	}
	if RefusalCode(err) != CodeGroupAccountActive {
		t.Fatalf("code %q, want %q", RefusalCode(err), CodeGroupAccountActive)
	}
}

func TestArchivingAGroupRemovesItsMemberships(t *testing.T) {
	i, stub := newFixture(t)
	stub.members["g-custom"] = []any{
		map[string]any{"id": "g-custom-u-1", "groupId": "g-custom", "userId": "u-1", "status": StatusActive},
		map[string]any{"id": "g-custom-u-2", "groupId": "g-custom", "userId": "u-2", "status": StatusActive},
		// Already removed: not touched again, or a second version says
		// somebody left twice.
		map[string]any{"id": "g-custom-u-3", "groupId": "g-custom", "userId": "u-3", "status": StatusRemoved},
	}
	if _, err := i.handleGroupArchive(adminCtx(), map[string]any{"groupId": "g-custom"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := len(stub.writesNamed("writeGroupMembership")); got != 2 {
		t.Fatalf("removed %d memberships, want 2", got)
	}
	groupWrites := stub.writesNamed("writeGroup")
	if len(groupWrites) != 1 || !strings.Contains(groupWrites[0], `status: "archived"`) {
		t.Fatalf("the group was not archived: %v", groupWrites)
	}
	if !strings.Contains(groupWrites[0], "archivedAt") {
		t.Fatalf("no archivedAt stamp: %s", groupWrites[0])
	}
}

func TestAnArchivedGroupRefusesEdits(t *testing.T) {
	i, _ := newFixture(t)
	for _, call := range []func() error{
		func() error {
			_, err := i.handleGroupUpdate(adminCtx(), map[string]any{"groupId": "g-archived", "name": "X"}, 0)
			return err
		},
		func() error {
			_, err := i.handleGroupArchive(adminCtx(), map[string]any{"groupId": "g-archived"}, 0)
			return err
		},
		func() error {
			_, err := i.handleGroupMemberAdd(adminCtx(), map[string]any{"groupId": "g-archived", "userId": "u-writer"}, 0)
			return err
		},
	} {
		if err := call(); err == nil {
			t.Fatal("an archived group admitted an edit")
		} else if RefusalCode(err) != CodeGroupNotActive {
			t.Fatalf("code %q, want %q", RefusalCode(err), CodeGroupNotActive)
		}
	}
	// Removing somebody FROM an archived group is allowed: it is tidying
	// history, and refusing it would strand memberships nobody can clear.
	if _, err := i.handleGroupMemberRemove(adminCtx(), map[string]any{
		"groupId": "g-archived", "userId": "u-writer",
	}, 0); err != nil {
		t.Fatalf("removing from an archived group: want admit, got %v", err)
	}
}

func TestEnsureForAccountIsIdempotent(t *testing.T) {
	i, stub := newFixture(t)
	// The account already has its group, so a re-run writes nothing.
	res, err := i.handleGroupEnsureForAccount(context.Background(), map[string]any{"accountId": "acme"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.writesNamed("writeGroup")) != 0 {
		t.Fatal("an account that already has its group was given a second one")
	}
	if len(res) != 1 {
		t.Fatalf("want one result node, got %d", len(res))
	}

	// An account with none gets exactly one.
	stub.accounts["beta"] = map[string]any{"id": "beta", "name": "Beta", "status": StatusActive}
	if _, err := i.handleGroupEnsureForAccount(context.Background(), map[string]any{"accountId": "beta"}, 0); err != nil {
		t.Fatal(err)
	}
	writes := stub.writesNamed("writeGroup")
	if len(writes) != 1 {
		t.Fatalf("want one write, got %d", len(writes))
	}
	if !strings.Contains(writes[0], `groupId: "acct-beta"`) {
		t.Fatalf("the group was not written at the derived id: %s", writes[0])
	}
	if !strings.Contains(writes[0], `kind: "account"`) {
		t.Fatalf("the group is not account-kind: %s", writes[0])
	}
	if !strings.Contains(writes[0], `name: "Beta"`) {
		t.Fatalf("the group is not named after its account: %s", writes[0])
	}
}

func TestEnsureForAccountSkipsAnArchivedAccount(t *testing.T) {
	// The cascade archives an account's groups; a backfill that re-created
	// one would undo that on the next boot, forever, with nobody watching.
	i, stub := newFixture(t)
	if _, err := i.handleGroupEnsureForAccount(context.Background(), map[string]any{"accountId": "gone"}, 0); err != nil {
		t.Fatal(err)
	}
	if len(stub.writesNamed("writeGroup")) != 0 {
		t.Fatal("an archived account was given a group")
	}
}

func TestArchiveGroupsForAccountTakesBothKinds(t *testing.T) {
	// A CUSTOM group tied to an archived account grants access to a client
	// that is gone. Leaving it active would mean the archive changed what
	// the screens show and not what the people can reach.
	i, stub := newFixture(t)
	stub.members["acct-acme"] = []any{
		map[string]any{"id": "acct-acme-u-1", "groupId": "acct-acme", "userId": "u-1", "status": StatusActive},
	}
	groups, memberships, err := i.ArchiveGroupsForAccount(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	if groups != 2 {
		t.Fatalf("archived %d groups, want 2 (the account-kind one AND the custom one tied to it)", groups)
	}
	if memberships != 1 {
		t.Fatalf("removed %d memberships, want 1", memberships)
	}
}
