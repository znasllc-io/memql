package worker

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestBothConsentsAreRequired(t *testing.T) {
	// The whole of D6. Either half alone is not consent, and the table is
	// written out rather than looped because each row is a different decision
	// somebody made about a different thing.
	cases := []struct {
		name    string
		owner   string
		cockpit string
		want    bool
	}{
		{"neither", SharingModeOwner, InferenceServeOwner, false},
		{"owner only", SharingModeCluster, InferenceServeOwner, false},
		{"cockpit only", SharingModeOwner, InferenceServeCluster, false},
		{"both", SharingModeCluster, InferenceServeCluster, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServesTheCluster(tc.owner, tc.cockpit); got != tc.want {
				t.Fatalf("owner=%q cockpit=%q -> %v, want %v", tc.owner, tc.cockpit, got, tc.want)
			}
		})
	}
}

func TestAnAbsentCockpitConsentIsNotAConsent(t *testing.T) {
	// A cockpit that predates the field has said nothing, and silence is not
	// agreement to run other people's work on somebody's laptop. This is the
	// one place in the epic where the reading of silence is the SAFE value
	// rather than merely the honest one -- and the two coincide.
	if ServesTheCluster(SharingModeCluster, "") {
		t.Fatal("an unset inferenceServe must not count as consent")
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

func TestTheRefusalNamesWhichConsentIsMissing(t *testing.T) {
	// THE REASON THIS IS A FUNCTION AND NOT A BOOLEAN. The two repairs are in
	// different places and are performed by different people: the owner's is an
	// act on a web page, the cockpit's is a line in a file on that machine's own
	// disk. A single "not shared" sentence sends half the operators to the wrong
	// machine, and the laptop's owner goes looking on a page for a setting that
	// is not there.
	if got := SharingRefusal(SharingModeCluster, InferenceServeCluster); got != "" {
		t.Fatalf("a shared machine has no refusal, got %q", got)
	}

	ownerMissing := SharingRefusal(SharingModeOwner, InferenceServeCluster)
	if !strings.Contains(ownerMissing, "Fleet") {
		t.Fatalf("the owner's half must point at the Fleet page, got %q", ownerMissing)
	}
	if strings.Contains(ownerMissing, "policy.yaml") {
		t.Fatalf("the owner's half must not send them to a file on the machine, got %q", ownerMissing)
	}

	cockpitMissing := SharingRefusal(SharingModeCluster, InferenceServeOwner)
	if !strings.Contains(cockpitMissing, "policy.yaml") {
		t.Fatalf("the cockpit's half must name the file, got %q", cockpitMissing)
	}
	if strings.Contains(cockpitMissing, "Fleet") {
		t.Fatalf("the cockpit's half must not send them to a web page, got %q", cockpitMissing)
	}

	neither := SharingRefusal(SharingModeOwner, InferenceServeOwner)
	if !strings.Contains(neither, "Both") {
		t.Fatalf("with neither given, the sentence must say both are needed, got %q", neither)
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

func TestAMalformedInferenceServeRefusesTheRegistration(t *testing.T) {
	// A consent value the engine does not understand must not be silently read
	// as either answer. Refusing at register is the same direction the hardware
	// inventory takes, and for a sharper reason: the two readings differ by
	// whether a stranger's prompt runs on this machine.
	_, err := ParseCapabilityDescriptor(`{"platform":"darwin","displayServer":"quartz","inferenceServe":"everyone","schemaVersion":1}`)
	if err == nil {
		t.Fatal("an unknown inferenceServe must refuse the registration")
	}
	if !strings.Contains(err.Error(), "everyone") {
		t.Fatalf("the error must name the offending value, got %q", err)
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

func TestAPeopleShareNamingNobodyIsOwner(t *testing.T) {
	// A people share with an empty list names nobody, and the honest reading of
	// "lent to nobody" is "not lent". fleetSetSharing refuses to write one; this
	// is what a hand-edited or half-written row reads as.
	got := SharingFromRow(map[string]any{"mode": "people"})
	if got.Mode != SharingModeOwner || len(got.UserIds) != 0 || len(got.GroupIds) != 0 {
		t.Fatalf("an empty people share must read as owner, got %+v", got)
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

func TestServesPersonNeedsTheCockpitToo(t *testing.T) {
	// The machine's own half is still required (G5): it is a decision about
	// where the machine is, and who the owner lends it to does not change that.
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	ana := NewPerson(context.Background(), "ana", nil)
	if ServesPerson(s, InferenceServeOwner, ana) {
		t.Fatal("the cockpit's half is still required for a people share")
	}
	if !ServesPerson(s, InferenceServeCluster, ana) {
		t.Fatal("both halves given must serve the listed person")
	}
	if ServesTheCluster(SharingModePeople, InferenceServeCluster) {
		t.Fatal("a people share is never a cluster share: system work must not ride it")
	}
}

func TestRefusalsNameTheMissingHalfForPeopleShares(t *testing.T) {
	s := Sharing{Mode: SharingModePeople, UserIds: []string{"ana"}}
	if got := PersonRefusal(s, InferenceServeCluster, NewPerson(context.Background(), "bo", nil)); !strings.Contains(got, "specific people") {
		t.Fatalf("a person not on the list: %q", got)
	}
	if got := PersonRefusal(s, InferenceServeOwner, NewPerson(context.Background(), "ana", nil)); !strings.Contains(got, "policy.yaml") {
		t.Fatalf("a listed person on a machine that has not agreed: %q", got)
	}
	if got := SystemRefusal(s, InferenceServeCluster); !strings.Contains(got, "cluster's own work") {
		t.Fatalf("system work on a people share: %q", got)
	}
	if got := PersonRefusal(s, InferenceServeCluster, NewPerson(context.Background(), "ana", nil)); got != "" {
		t.Fatalf("a served person has no refusal, got %q", got)
	}
	if got := PersonRefusal(Sharing{Mode: SharingModeOwner}, InferenceServeCluster, NewPerson(context.Background(), "ana", nil)); got != SharingRefusal(SharingModeOwner, InferenceServeCluster) {
		t.Fatalf("an owner-only machine keeps the two-consent sentence, got %q", got)
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
