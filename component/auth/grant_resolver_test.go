package auth

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// THE RESOLUTION RULE (app access grants record, section 1; epic memql#5295).
//
// Every case below is a statement about ONE function over VALUES: a subject, a
// catalog answer, and the subject's active grants. No engine, no database. The
// database-gated enforcement tests in component/memql prove the rows reach this
// function; these prove the function answers what the record says.

// countingGrantSource is a GrantSource that answers from two maps and counts
// every read, so the per-request memo can be measured rather than assumed.
type countingGrantSource struct {
	user  map[string][]Grant
	group map[string][]Grant
	reads int
}

func (s *countingGrantSource) ActiveGrantsForUser(_ context.Context, userId string) ([]Grant, error) {
	s.reads++
	return s.user[userId], nil
}

func (s *countingGrantSource) ActiveGrantsForGroups(_ context.Context, groupIds []string) ([]Grant, error) {
	s.reads++
	var out []Grant
	for _, g := range groupIds {
		out = append(out, s.group[g]...)
	}
	return out, nil
}

// installGrantFake installs a source for one test and clears it afterwards,
// for installFake's reason: the holder is package state.
func installGrantFake(t *testing.T, s GrantSource) {
	t.Helper()
	SetGrantSource(s)
	t.Cleanup(func() { SetGrantSource(nil) })
}

// grantTestCatalog is the role level of every case: `user` holds read on
// app:deployables and nothing else; `viewer` holds nothing; `ghost` is unknown.
func grantTestCatalog(t *testing.T) {
	t.Helper()
	installFake(t, &fakeCatalog{
		ranks: map[string]int{"user": 100, "viewer": 50},
		names: map[string]string{"user": "Member", "viewer": "Viewer"},
		grants: map[string]map[VerbResource]bool{
			"user":   {{Verb: VerbRead, Resource: "app:deployables"}: true},
			"viewer": {},
		},
	})
}

func allow(verb, resource string) Grant {
	return Grant{Verb: verb, Resource: resource, Effect: GrantAllow}
}
func deny(verb, resource string) Grant {
	return Grant{Verb: verb, Resource: resource, Effect: GrantDeny}
}

func TestCapableForResolvesMostSpecificWinsAndDenyWinsWithinALevel(t *testing.T) {
	grantTestCatalog(t)

	const app = "app:deployables"
	const part = "app:deployables/publish"

	for _, tc := range []struct {
		name    string
		subject Subject
		user    []Grant
		group   map[string][]Grant
		verb    string
		res     string
		want    bool
	}{
		{
			name:    "role allow + group deny = deny",
			subject: Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}},
			group:   map[string][]Grant{"g1": {deny(VerbRead, app)}},
			verb:    VerbRead, res: app, want: false,
		},
		{
			name:    "role deny + user allow = allow",
			subject: Subject{Role: "user", UserId: "u1"},
			user:    []Grant{allow(VerbExecute, part)},
			verb:    VerbExecute, res: part, want: true,
		},
		{
			name:    "two groups disagree = deny",
			subject: Subject{Role: "viewer", UserId: "u1", GroupIds: []string{"g1", "g2"}},
			group:   map[string][]Grant{"g1": {allow(VerbRead, app)}, "g2": {deny(VerbRead, app)}},
			verb:    VerbRead, res: app, want: false,
		},
		{
			name:    "user beats group: group deny, user allow = allow",
			subject: Subject{Role: "viewer", UserId: "u1", GroupIds: []string{"g1"}},
			group:   map[string][]Grant{"g1": {deny(VerbRead, app)}},
			user:    []Grant{allow(VerbRead, app)},
			verb:    VerbRead, res: app, want: true,
		},
		{
			name:    "user beats group: group allow, user deny = deny",
			subject: Subject{Role: "viewer", UserId: "u1", GroupIds: []string{"g1"}},
			group:   map[string][]Grant{"g1": {allow(VerbRead, app)}},
			user:    []Grant{deny(VerbRead, app)},
			verb:    VerbRead, res: app, want: false,
		},
		{
			name:    "no grants = the catalog's answer (held)",
			subject: Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}},
			verb:    VerbRead, res: app, want: true,
		},
		{
			name:    "no grants = the catalog's answer (not held)",
			subject: Subject{Role: "user", UserId: "u1"},
			verb:    VerbExecute, res: part, want: false,
		},
		{
			name:    "unknown role with no grants holds nothing",
			subject: Subject{Role: "ghost", UserId: "u1", GroupIds: []string{"g1"}},
			verb:    VerbRead, res: app, want: false,
		},
		{
			name:    "a grant on a different pair decides nothing",
			subject: Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}},
			group:   map[string][]Grant{"g1": {deny(VerbExecute, part)}},
			user:    []Grant{deny(VerbRead, "app:other")},
			verb:    VerbRead, res: app, want: true,
		},
		{
			name:    "a group allow widens an unknown role",
			subject: Subject{Role: "ghost", UserId: "u1", GroupIds: []string{"g1"}},
			group:   map[string][]Grant{"g1": {allow(VerbRead, app)}},
			verb:    VerbRead, res: app, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &countingGrantSource{user: map[string][]Grant{"u1": tc.user}, group: tc.group}
			installGrantFake(t, src)
			if got := CapableFor(context.Background(), tc.subject, tc.verb, tc.res); got != tc.want {
				t.Fatalf("CapableFor(%+v, %s, %s) = %v, want %v", tc.subject, tc.verb, tc.res, got, tc.want)
			}
		})
	}
}

// TestCapableForIgnoresInactiveRows: a source REPORTS a row's lifecycle flag
// and the resolver is the one place that decides what it means -- an
// inactive grant decides nothing, whichever way it points. Fail-closed for an
// allow and fail-open for a deny are both exactly what "revoked" means.
func TestCapableForIgnoresInactiveRows(t *testing.T) {
	grantTestCatalog(t)
	const app = "app:deployables"

	inactiveAllow := Grant{Verb: VerbRead, Resource: app, Effect: GrantAllow, Inactive: true}
	inactiveDeny := Grant{Verb: VerbRead, Resource: app, Effect: GrantDeny, Inactive: true}

	installGrantFake(t, &countingGrantSource{
		user:  map[string][]Grant{"u1": {inactiveAllow}},
		group: map[string][]Grant{"g1": {inactiveDeny}},
	})
	// viewer holds nothing; an inactive allow must not widen it.
	if CapableFor(context.Background(), Subject{Role: "viewer", UserId: "u1"}, VerbRead, app) {
		t.Fatal("an inactive user allow widened a viewer")
	}
	// user holds read; an inactive group deny must not narrow it.
	if !CapableFor(context.Background(), Subject{Role: "user", UserId: "u2", GroupIds: []string{"g1"}}, VerbRead, app) {
		t.Fatal("an inactive group deny narrowed a member")
	}
}

// TestCapableForDoesNotConsultGrantsForANonPrincipal: MaintenanceActor, the
// seed materializer, an automation's system actor and borrowed authority carry
// AccessContext.Unranked, and the rank rules do not govern them (memql#4832,
// D4). Neither do grants: a deny naming the person a worker borrows must not
// stop the worker's own write, and a grant is a statement about a PRINCIPAL.
func TestCapableForDoesNotConsultGrantsForANonPrincipal(t *testing.T) {
	grantTestCatalog(t)
	const app = "app:deployables"
	src := &countingGrantSource{
		user:  map[string][]Grant{"u1": {deny(VerbRead, app)}},
		group: map[string][]Grant{"g1": {deny(VerbRead, app)}},
	}
	installGrantFake(t, src)

	subject := Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}, Unranked: true}
	if !CapableFor(context.Background(), subject, VerbRead, app) {
		t.Fatal("an Unranked actor was narrowed by a grant; a non-principal resolves as the catalog alone")
	}
	if src.reads != 0 {
		t.Fatalf("an Unranked actor cost %d grant reads; a non-principal is not consulted", src.reads)
	}

	// And the canonical constructor agrees: borrowed authority resolves as
	// today, which for RoleWriter under this catalog is "nothing".
	ctx := ContextWithUserActor(context.Background(), "u1")
	borrowed, ok := SubjectFromContext(ctx)
	if !ok || !borrowed.Unranked {
		t.Fatalf("SubjectFromContext(borrowed) = %+v, %v; want an Unranked subject", borrowed, ok)
	}
}

// TestCapableForWithNoSourceIsTheCatalogAnswer: before the engine installs a
// source -- a test engine, a node type built without a database -- the
// resolver is exactly the role-shaped one it replaced.
func TestCapableForWithNoSourceIsTheCatalogAnswer(t *testing.T) {
	grantTestCatalog(t)
	SetGrantSource(nil)
	const app = "app:deployables"
	if !CapableFor(context.Background(), Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}}, VerbRead, app) {
		t.Fatal("with no source installed a member lost read on the app its role holds")
	}
	if CapableFor(context.Background(), Subject{Role: "viewer", UserId: "u1"}, VerbRead, app) {
		t.Fatal("with no source installed a viewer gained read")
	}
}

// TestCapableForMemoisesGrantReadsPerRequest (D9): two questions in one request
// cost one read of each level, and a request with no memo installed still
// answers -- it just pays per call.
func TestCapableForMemoisesGrantReadsPerRequest(t *testing.T) {
	grantTestCatalog(t)
	const app = "app:deployables"
	src := &countingGrantSource{
		user:  map[string][]Grant{"u1": {allow(VerbExecute, "app:deployables/publish")}},
		group: map[string][]Grant{"g1": {deny(VerbRead, app)}},
	}
	installGrantFake(t, src)
	subject := Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}}

	ctx := ContextWithGrantMemo(context.Background())
	CapableFor(ctx, subject, VerbRead, app)
	after1 := src.reads
	CapableFor(ctx, subject, VerbExecute, "app:deployables/publish")
	CapableFor(ctx, subject, VerbRead, app)
	if src.reads != after1 {
		t.Fatalf("three questions in one request cost %d reads; the first cost %d and the rest must be free", src.reads, after1)
	}
	if after1 != 2 {
		t.Fatalf("the first question cost %d reads; want exactly one user read and one group read", after1)
	}

	// A different subject on the same request is its own entry -- a nested
	// call under borrowed or system authority must not read the outer
	// caller's grants.
	other := Subject{Role: "user", UserId: "u2", GroupIds: []string{"g1"}}
	CapableFor(ctx, other, VerbRead, app)
	if src.reads != after1+2 {
		t.Fatalf("a second subject on the same request cost %d reads, want 2 more than %d", src.reads-after1, after1)
	}

	// No memo: correct, and paid per call.
	bare := context.Background()
	before := src.reads
	CapableFor(bare, subject, VerbRead, app)
	CapableFor(bare, subject, VerbRead, app)
	if src.reads != before+4 {
		t.Fatalf("without a memo two questions cost %d reads, want 4", src.reads-before)
	}

	// Installing twice keeps the one memo.
	if ContextWithGrantMemo(ctx) != ctx {
		t.Fatal("ContextWithGrantMemo on a context that already carries one must return it unchanged")
	}
}

// TestCapableForSkipsTheGroupReadForAMemberOfNothing: a subject with no group
// ids costs no group read at all.
func TestCapableForSkipsTheGroupReadForAMemberOfNothing(t *testing.T) {
	grantTestCatalog(t)
	src := &countingGrantSource{user: map[string][]Grant{}, group: map[string][]Grant{}}
	installGrantFake(t, src)
	CapableFor(context.Background(), Subject{Role: "user", UserId: "u1"}, VerbRead, "app:deployables")
	if src.reads != 1 {
		t.Fatalf("a member of no group cost %d reads, want 1 (the user read alone)", src.reads)
	}
}

// TestSubjectFromContextBuildsFromTheVerifiedCaller: role and user id off the
// AccessContext, group ids from the installed membership source, nothing from
// anywhere a request could forge.
func TestSubjectFromContextBuildsFromTheVerifiedCaller(t *testing.T) {
	if _, ok := SubjectFromContext(context.Background()); ok {
		t.Fatal("a context with no AccessContext produced a subject")
	}

	memberships := membershipFake{"u1": {"g2", "g1"}}
	SetMembershipSource(memberships)
	t.Cleanup(func() { SetMembershipSource(nil) })

	ctx := ContextWithAccess(context.Background(), &AccessContext{UserId: "u1", Role: Role(" Admin ")})
	s, ok := SubjectFromContext(ctx)
	if !ok {
		t.Fatal("a signed-in caller produced no subject")
	}
	if s.Role != RoleAdmin {
		t.Fatalf("Role = %q, want it folded to %q", s.Role, RoleAdmin)
	}
	if s.UserId != "u1" || s.Unranked {
		t.Fatalf("subject = %+v", s)
	}
	got := append([]string(nil), s.GroupIds...)
	sort.Strings(got)
	if strings.Join(got, ",") != "g1,g2" {
		t.Fatalf("GroupIds = %v, want g1,g2 from the membership source", s.GroupIds)
	}

	// Anonymous and connector actors are not people and hold no groups.
	for name, ac := range map[string]*AccessContext{
		"anonymous": AnonymousActor(),
		"connector": ConnectorActor("shopify"),
	} {
		s, _ := SubjectFromContext(ContextWithAccess(context.Background(), ac))
		if len(s.GroupIds) != 0 {
			t.Fatalf("%s actor resolved groups %v", name, s.GroupIds)
		}
	}
}

type membershipFake map[string][]string

func (m membershipFake) ActiveGroupIdsForUser(_ context.Context, userId string) []string {
	return m[userId]
}

// TestGrantRowIDIsDerivedAndInjective: the same four parts always name the
// same row (re-granting is a new VERSION, never a second row), and no two
// distinct tuples collide -- resourceType carries ':' and '/', so the parts are
// digested individually rather than joined by a separator (memql#3009).
func TestGrantRowIDIsDerivedAndInjective(t *testing.T) {
	a := GrantRowID(SubjectKindUser, "u1", VerbRead, "app:deployables")
	if a != GrantRowID(SubjectKindUser, " u1 ", VerbRead, "app:deployables") {
		t.Fatal("padding on a part changed the id; the derivation must trim")
	}
	if a == GrantRowID(SubjectKindGroup, "u1", VerbRead, "app:deployables") {
		t.Fatal("a user subject and a group subject with the same id collapsed onto one row")
	}
	if a == GrantRowID(SubjectKindUser, "u1", VerbExecute, "app:deployables") {
		t.Fatal("two verbs collapsed onto one row")
	}
	// The separator-collision shape memql#2980 is about: moving a boundary
	// between parts must not produce the same id.
	if GrantRowID(SubjectKindUser, "u1:x", VerbRead, "y") == GrantRowID(SubjectKindUser, "u1", VerbRead, "x:y") {
		t.Fatal("a moved part boundary produced the same id; the parts are not digested individually")
	}
	if len(a) != 64 || strings.ContainsAny(a, ":/") {
		t.Fatalf("GrantRowID = %q; want a 64-hex digest a canonical id can carry as its short id", a)
	}
}

// TestEffectiveCapabilitiesCarriesProvenance (epic memql#5298): the whole
// set, one entry per pair the cluster knows, each saying which level answered
// -- and a pair only a grant names is IN the set, so a deny on something the
// role never held stays visible.
func TestEffectiveCapabilitiesCarriesProvenance(t *testing.T) {
	grantTestCatalog(t)
	const app = "app:deployables"
	const part = "app:deployables/publish"
	src := &countingGrantSource{
		user: map[string][]Grant{"u1": {
			deny(VerbRead, app),
			allow(VerbExecute, part),
			{Verb: VerbCreate, Resource: "app:other", Effect: GrantAllow, Inactive: true},
		}},
		group: map[string][]Grant{"g1": {allow(VerbRead, "app:campaigns")}},
	}
	installGrantFake(t, src)
	subject := Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}}

	got := map[string]Decision{}
	for _, d := range EffectiveCapabilities(ContextWithGrantMemo(context.Background()), subject) {
		got[d.Verb+" "+d.Resource] = d
	}
	// The role held it; the user deny overlays it.
	if d := got[VerbRead+" "+app]; d.Held || d.Source != SourceUser {
		t.Fatalf("read app = %+v, want denied from user", d)
	}
	// Not in the catalog at all; only the user grant names it.
	if d := got[VerbExecute+" "+part]; !d.Held || d.Source != SourceUser {
		t.Fatalf("execute part = %+v, want held from user", d)
	}
	// Only a group grant names it.
	if d := got[VerbRead+" app:campaigns"]; !d.Held || d.Source != SourceGroup {
		t.Fatalf("read app:campaigns = %+v, want held from group", d)
	}
	// An inactive grant neither decides nor widens the universe.
	if _, present := got[VerbCreate+" app:other"]; present {
		t.Fatal("an inactive grant's pair entered the effective set")
	}
	// One read of each level for the whole set.
	if src.reads != 2 {
		t.Fatalf("the effective set cost %d grant reads, want 2", src.reads)
	}

	// Sorted by resource then verb, so a screen draws the same rows twice.
	list := EffectiveCapabilities(context.Background(), subject)
	for i := 1; i < len(list); i++ {
		a, b := list[i-1], list[i]
		if a.Resource > b.Resource || (a.Resource == b.Resource && a.Verb > b.Verb) {
			t.Fatalf("effective set is not sorted at %d: %+v then %+v", i, a, b)
		}
	}

	// An Unranked subject is the catalog alone and costs no read.
	before := src.reads
	for _, d := range EffectiveCapabilities(context.Background(), Subject{Role: "user", UserId: "u1", Unranked: true}) {
		if d.Source != SourceRole {
			t.Fatalf("an Unranked subject's entry came from %s", d.Source)
		}
	}
	if src.reads != before {
		t.Fatal("an Unranked subject cost a grant read")
	}
}

// TestDecideForIsCapableForWithProvenance: the two cannot disagree, because
// one is the other's Held field.
func TestDecideForIsCapableForWithProvenance(t *testing.T) {
	grantTestCatalog(t)
	const app = "app:deployables"
	installGrantFake(t, &countingGrantSource{
		user:  map[string][]Grant{"u1": {allow(VerbRead, app)}},
		group: map[string][]Grant{"g1": {deny(VerbRead, app)}},
	})
	for _, subject := range []Subject{
		{Role: "user", UserId: "u1", GroupIds: []string{"g1"}},
		{Role: "user", UserId: "u2", GroupIds: []string{"g1"}},
		{Role: "user", UserId: "u2"},
		{Role: "viewer", UserId: "u2"},
	} {
		d := DecideFor(context.Background(), subject, VerbRead, app)
		if d.Held != CapableFor(context.Background(), subject, VerbRead, app) {
			t.Fatalf("DecideFor and CapableFor disagree for %+v", subject)
		}
	}
	if d := DecideFor(context.Background(), Subject{Role: "user", UserId: "u1", GroupIds: []string{"g1"}}, VerbRead, app); !d.Held || d.Source != SourceUser {
		t.Fatalf("user over group: %+v", d)
	}
	if d := DecideFor(context.Background(), Subject{Role: "user", UserId: "u2", GroupIds: []string{"g1"}}, VerbRead, app); d.Held || d.Source != SourceGroup {
		t.Fatalf("group over role: %+v", d)
	}
	if d := DecideFor(context.Background(), Subject{Role: "user", UserId: "u2"}, VerbRead, app); !d.Held || d.Source != SourceRole {
		t.Fatalf("role alone: %+v", d)
	}
}
