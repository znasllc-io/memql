package worker

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestClusterWorkRequiresTheOwnersClusterShare(t *testing.T) {
	for _, mode := range []string{"", "owner", "people", "invalid", "cluster"} {
		if got := ServesTheCluster(mode); got != (mode == "cluster") {
			t.Fatalf("mode %q: %v", mode, got)
		}
	}
}

func TestGlobalSharingPausePreservesAudienceAndStopsEveryBorrowedAdmission(t *testing.T) {
	p := NewPerson(context.Background(), "ana", func(context.Context, string) []string { return []string{"team"} })
	for _, mode := range []string{SharingModePeople, SharingModeCluster} {
		s := Sharing{Mode: mode, Paused: true, UserIds: []string{"ana"}, GroupIds: []string{"team"}}
		if s.Admits(p) || s.ServesCluster() || len(s.SubjectsFor(p).UserIds) > 0 {
			t.Fatalf("paused %s admitted work", mode)
		}
		s = SharingFromRow(s.Row())
		if !s.Paused {
			t.Fatal("storage lost pause")
		}
		s.Paused = false
		if !s.Admits(p) {
			t.Fatal("resume lost the audience")
		}
	}
	if !SharingFromRow(map[string]any{"mode": "cluster", "paused": "false"}).Paused {
		t.Fatal("malformed pause enabled sharing")
	}
}

func TestAnythingThatIsNotClusterIsOwner(t *testing.T) {
	// A typo, a value from a future engine, a half-written row: all read as NOT
	// shared, because the failure direction here is a stranger's prompt running
	// on somebody's machine.
	for _, mode := range []string{"", "Cluster", "CLUSTER", "shared", "true", "cluster "} {
		got := SharingFromRow(map[string]any{"mode": mode})
		if mode == "cluster " {
			// Trimmed by the reader, so this one IS cluster -- worth pinning,
			// because the trim is what stops a stray space silently revoking a
			// machine somebody deliberately shared.
			if got.Mode != SharingModeCluster {
				t.Fatalf("a trailing space must not revoke a consent, got %q", got.Mode)
			}
			continue
		}
		if got.Mode != SharingModeOwner {
			t.Fatalf("mode %q must read as owner, got %q", mode, got.Mode)
		}
	}
}

func TestAMissingSharingObjectIsOwner(t *testing.T) {
	// Every registration written before this field existed. The default has to
	// be the one that changes nothing about how those machines behave.
	if SharingFromRow(nil).Mode != SharingModeOwner {
		t.Fatal("a row with no sharing object must read as owner")
	}
	if SharingFromRow(map[string]any{}).Mode != SharingModeOwner {
		t.Fatal("an empty sharing object must read as owner")
	}
}

func TestSharingRefusalPointsToTheOwnerInFleet(t *testing.T) {
	if got := SharingRefusal(SharingModeCluster); got != "" {
		t.Fatal(got)
	}
	if got := SharingRefusal(SharingModeOwner); !strings.Contains(got, "Fleet") || strings.Contains(got, "policy.yaml") {
		t.Fatal(got)
	}
}

func TestSharingRoundTripsThroughTheStoredShape(t *testing.T) {
	s := Sharing{Mode: SharingModeCluster, SharedAt: "2026-09-07T12:00:00Z", SharedBy: "v1:identity:user:u1"}
	back := SharingFromRow(s.Row())
	if !reflect.DeepEqual(back, s) {
		t.Fatalf("%+v vs %+v", back, s)
	}
	people := Sharing{Mode: SharingModePeople, UserIds: []string{"v1:identity:user:ana"}, GroupIds: []string{"design"}, SharedAt: "2026-09-23T12:00:00Z", SharedBy: "v1:identity:user:olivia"}
	if back := SharingFromRow(people.Row()); !reflect.DeepEqual(back, people) {
		t.Fatalf("a people share must round-trip: %+v vs %+v", back, people)
	}
}

// ---------------------------------------------------------------------------
// Sharing with PEOPLE (epic memql#5344, design G1, G3, G6, G7)
// ---------------------------------------------------------------------------

func TestAPeopleShareAdmitsExactlyItsSubjects(t *testing.T) {
	// G7. The listed people, and the members of the listed groups -- in either
	// spelling of an id, because a row stores what the caller sent and a
	// token's subject may be bare.
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"v1:identity:user:ana"}, GroupIds: []string{"design"}}
	groupsOf := func(_ context.Context, userId string) []string {
		if userId == "bo" {
			return []string{"v1:identity:group:design"}
		}
		return nil
	}
	cases := []struct {
		name string
		user string
		want bool
	}{
		{"listed person, bare id", "ana", true},
		{"listed person, canonical id", "v1:identity:user:ana", true},
		{"member of a listed group", "bo", true},
		{"somebody else", "cy", false},
		{"no acting person", "", false},
		{"a synthetic actor", "system:fleet-inference", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPerson(context.Background(), tc.user, groupsOf)
			if got := s.Admits(p); got != tc.want {
				t.Fatalf("Admits(%q) = %v, want %v", tc.user, got, tc.want)
			}
		})
	}
}

func TestClusterAndOwnerModesAdmitAsBefore(t *testing.T) {
	// The two modes that existed before this epic mean exactly what they meant:
	// everyone, and nobody but the owner (whose own machines never reach this
	// question at all).
	ana := NewPerson(context.Background(), "ana", nil)
	if !(Sharing{Mode: SharingModeCluster}).Admits(ana) {
		t.Fatal("a cluster share admits any person")
	}
	if (Sharing{Mode: SharingModeOwner}).Admits(ana) {
		t.Fatal("an owner-only machine admits nobody else")
	}
	if (Sharing{Mode: SharingModeCluster}).Admits(NewPerson(context.Background(), "", nil)) {
		t.Fatal("no acting person is system work, and system work is not a person")
	}
}

func TestGroupsAreResolvedOnlyWhenAGroupShareNeedsThem(t *testing.T) {
	// G6. A membership read per candidate per call would be a cost paid by
	// every fleet whether or not anybody shares with a group -- so it happens
	// only for a share that names one, and at most once per person.
	calls := 0
	count := func(context.Context, string) []string { calls++; return nil }
	for _, s := range []Sharing{
		{Mode: SharingModeCluster},
		{Mode: SharingModeOwner},
		{Mode: SharingModePeople, UserIds: []string{"ana"}},
	} {
		_ = s.Admits(NewPerson(context.Background(), "ana", count))
	}
	if calls != 0 {
		t.Fatalf("resolved groups %d times for shares that name no group", calls)
	}
	p := NewPerson(context.Background(), "bo", count)
	s := Sharing{Mode: SharingModePeople, GroupIds: []string{"g1"}}
	_ = s.Admits(p)
	_ = s.Admits(p)
	if calls != 1 {
		t.Fatalf("resolved groups %d times for one person, want exactly 1", calls)
	}
}

func TestANilGroupSourceNarrowsAndNeverWidens(t *testing.T) {
	// A membership read that answers nothing -- no source installed, or a read
	// that failed -- must cost the group arm and nothing else.
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}, GroupIds: []string{"design"}}
	nobody := func(context.Context, string) []string { return nil }
	if !s.Admits(NewPerson(context.Background(), "ana", nobody)) {
		t.Fatal("a listed person must still be admitted when groups cannot be read")
	}
	if s.Admits(NewPerson(context.Background(), "bo", nobody)) {
		t.Fatal("an unresolvable membership must admit nobody through a group")
	}
}

func TestAnEmptySharingListAdmitsNobody(t *testing.T) {
	got := SharingFromRow(map[string]any{"mode": "people"})
	if got.Mode != SharingModePeople || got.Admits(NewPerson(context.Background(), "ana", nil)) {
		t.Fatalf("an enabled, empty list must remain editable and admit nobody: %+v", got)
	}
}

func TestTheListsAreReadOnlyUnderPeople(t *testing.T) {
	// G10. A list stored beside another mode is residue, and a reader that
	// honoured it would lend a machine its owner had taken back.
	got := SharingFromRow(map[string]any{"mode": "cluster", "userIds": []any{"ana"}, "groupIds": []any{"g"}})
	if got.Mode != SharingModeCluster || len(got.UserIds) != 0 || len(got.GroupIds) != 0 {
		t.Fatalf("residue lists must be ignored outside people, got %+v", got)
	}
	got = SharingFromRow(map[string]any{"mode": "people", "userIds": []any{" ana ", "", "ana", "v1:identity:user:ana"}})
	if got.Mode != SharingModePeople || !reflect.DeepEqual(got.UserIds, []string{"ana"}) {
		t.Fatalf("ids must be trimmed and de-duplicated across spellings, got %+v", got.UserIds)
	}
}

func TestSavedAudienceIsEnoughForModelSharing(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	if !ServesPerson(s, NewPerson(context.Background(), "ana", nil)) {
		t.Fatal("a listed person must be admitted without machine setup")
	}
	if ServesTheCluster(s.Mode) {
		t.Fatal("a people share must not admit system work")
	}
}

func TestRefusalsRespectPeopleShares(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	if got := PersonRefusal(s, NewPerson(context.Background(), "bo", nil)); !strings.Contains(got, "specific people") {
		t.Fatal(got)
	}
	if got := PersonRefusal(s, NewPerson(context.Background(), "ana", nil)); got != "" {
		t.Fatal(got)
	}
	if got := SystemRefusal(s); !strings.Contains(got, "cluster's own work") {
		t.Fatal(got)
	}
	if got := PersonRefusal(Sharing{Mode: SharingModeOwner}, NewPerson(context.Background(), "ana", nil)); got != SharingRefusal(SharingModeOwner) {
		t.Fatal(got)
	}
}

func TestASyntheticActorIsNeverOnAPeopleList(t *testing.T) {
	// Review finding (epic memql#5344): automations run as
	// `system:automation:<name>` and maintenance as `system:maintenance:<name>`,
	// and a last-colon comparison matched `system:automation:ana` to a listed
	// `ana`. A people share is consent for PEOPLE; the cluster's own work
	// reaches only a machine lent to everyone (design G3). By construction,
	// not by the odds of a name colliding with an id.
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"indextodo", "v1:identity:user:indextodo"}}
	for _, actor := range []string{"system:automation:indextodo", "system:maintenance:indextodo", "system:indextodo"} {
		if s.Admits(NewPerson(context.Background(), actor, nil)) {
			t.Fatalf("%q must never be admitted by a people share", actor)
		}
	}
	if !(Sharing{Mode: SharingModeCluster}).Admits(NewPerson(context.Background(), "system:automation:indextodo", nil)) {
		t.Fatal("a machine lent to everyone serves the cluster's own work")
	}
	if SameSubjectId("system:automation:x", "x") {
		t.Fatal("a system actor id is one opaque id, never the text after its last colon")
	}
	if !SameSubjectId("v1:identity:user:ana", "ana") {
		t.Fatal("a canonical user id and its bare form are one person")
	}
}

func TestPausingARecipientKeepsItListedAndClosesOnlyThatGrant(t *testing.T) {
	ctx := context.Background()
	s := SharingFromRow(map[string]any{"mode": "people", "userIds": []string{"ana"}, "groupIds": []string{"design"}, "pausedUserIds": []string{"ana"}, "pausedGroupIds": []string{"design"}})
	person := NewPerson(ctx, "v1:identity:user:ana", func(context.Context, string) []string { return []string{"v1:identity:group:design"} })
	if s.Admits(person) {
		t.Fatal("paused direct and group grants must refuse new calls")
	}
	if len(s.UserIds) != 1 || len(s.GroupIds) != 1 {
		t.Fatal("pause must retain the recipient")
	}
	s.PausedGroupIds = nil
	if !s.Admits(person) {
		t.Fatal("another active grant still admits this user")
	}
	subjects := s.SubjectsFor(person)
	if !subjects.Borrowed || len(subjects.UserIds) != 0 || len(subjects.GroupIds) != 1 {
		t.Fatalf("only the admitting grant is counted: %+v", subjects)
	}
	s.PausedUserIds = nil
	subjects = s.SubjectsFor(person)
	if len(subjects.UserIds) != 1 || len(subjects.GroupIds) != 1 {
		t.Fatalf("overlapping active grants each count the call: %+v", subjects)
	}
	roundtrip := SharingFromRow(s.Row())
	if !roundtrip.Admits(person) {
		t.Fatal("resuming must survive a stored-row roundtrip")
	}
}

func TestClusterShareAttributionDoesNotNeedNamedGrants(t *testing.T) {
	for _, user := range []string{"ana", "system:automation:task"} {
		p := NewPerson(context.Background(), user, nil)
		subjects := (Sharing{Mode: SharingModeCluster}).SubjectsFor(p)
		if !subjects.Borrowed || len(subjects.UserIds)+len(subjects.GroupIds) != 0 {
			t.Fatalf("cluster share lost borrowed-call attribution: %+v", subjects)
		}
	}
	if (Sharing{Mode: SharingModeOwner}).SubjectsFor(NewPerson(context.Background(), "ana", nil)).Borrowed {
		t.Fatal("private machine invented a shared call")
	}
}
