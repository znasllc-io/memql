package reviewspack

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// settings_test.go -- ONE ROW PER STORE, AND A WRITE NEVER LOSES A FIELD.
//
// Every settings write is a new version of the store's one row, and a version
// is a whole payload. A writer that sent only the field it changed would
// silently clear every other one -- the display toggle used to do exactly
// that. These tests pin the merge, the defaults, who may name the owner who
// approves removals, and whom they may name: one of the store's own people who
// can moderate it (shopRoster, reads_test.go).

func settingsReader(row map[string]any) *decideReader {
	return &decideReader{settings: row}
}

func writeSettings(t *testing.T, ctx context.Context, row map[string]any, args map[string]any, display bool) (map[string]any, error) {
	t.Helper()
	return writeSettingsOn(t, ctx, shopRoster(), row, args, display)
}

// writeSettingsOn is writeSettings over the roster given, so a test can read
// back what the write read.
func writeSettingsOn(t *testing.T, ctx context.Context, roster *orgRoster, row map[string]any, args map[string]any, display bool) (map[string]any, error) {
	t.Helper()
	w := &fakeWriter{}
	p := &Provider{reader: withRoster{rowReader: settingsReader(row), roster: roster}, organizations: roster, writer: w}
	var err error
	if display {
		_, err = p.setPublicDisplay(ctx, args, 0)
	} else {
		_, err = p.updateSettings(ctx, args, 0)
	}
	if err != nil {
		w.none(t)
		return nil, err
	}
	out := w.only(t, "putReviewSettings")
	if out["settingsId"] != SettingsRowID(asString(args["storeId"])) {
		t.Fatalf("settingsId = %v, want the store's one settings row", out["settingsId"])
	}
	return out, nil
}

func existingSettings() map[string]any {
	return map[string]any{
		"storeId": "store-live", "ownerUserId": "merchant-1", "accountId": "acct-shop",
		"publicDisplay": false, "notifyEmail": "support@example.com", "linkLifetimeDays": 14,
		"requestDelayDays": 14, "removalApproverUserId": "owner-1",
	}
}

func TestTheDisplayToggleKeepsEveryOtherSetting(t *testing.T) {
	out, err := writeSettings(t, moderatorCtx("mod-1"), existingSettings(),
		map[string]any{"siteId": "site-1", "storeId": "store-live", "publicDisplay": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if out["publicDisplay"] != true {
		t.Fatalf("publicDisplay = %v, want true", out["publicDisplay"])
	}
	for _, k := range []string{"notifyEmail", "removalApproverUserId", "accountId", "ownerUserId"} {
		if out[k] != existingSettings()[k] {
			t.Fatalf("%s = %v after flipping the display, want %v kept", k, out[k], existingSettings()[k])
		}
	}
	if out["linkLifetimeDays"] != 14 {
		t.Fatalf("linkLifetimeDays = %v, want 14 kept", out["linkLifetimeDays"])
	}
}

func TestUpdateSettingsMergesTheFieldsItIsGiven(t *testing.T) {
	out, err := writeSettings(t, moderatorCtx("mod-1"), existingSettings(),
		map[string]any{"siteId": "site-1", "storeId": "store-live", "linkLifetimeDays": 21, "notifyEmail": "reviews@example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out["linkLifetimeDays"] != 21 || out["notifyEmail"] != "reviews@example.com" {
		t.Fatalf("out = %v", out)
	}
	if out["requestDelayDays"] != 14 || out["publicDisplay"] != false {
		t.Fatalf("a field the call did not name changed: %v", out)
	}
}

func TestUpdateSettingsRefusesValuesOutOfRange(t *testing.T) {
	bad := []map[string]any{
		{"storeId": "store-live", "linkLifetimeDays": 0},
		{"storeId": "store-live", "linkLifetimeDays": 400},
		{"storeId": "store-live", "requestDelayDays": -1},
		{"storeId": "store-live", "notifyEmail": "not an address"},
		{"storeId": "store-live", "unknownSetting": true},
		{"siteId": "site-1", "linkLifetimeDays": 10},
	}
	for _, args := range bad {
		if _, err := writeSettings(t, moderatorCtx("mod-1"), existingSettings(), args, false); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
}

// NAMING THE OWNER WHO APPROVES REMOVALS IS NOT A MODERATOR'S CALL. A
// moderator who could name themselves would remove reviews unapproved.
func TestOnlyTheCurrentOwnerOrAnOperatorNamesTheRemovalApprover(t *testing.T) {
	args := map[string]any{"siteId": "site-1", "storeId": "store-live", "removalApproverUserId": "mod-1"}
	if _, err := writeSettings(t, moderatorCtx("mod-1"), existingSettings(), args, false); err == nil {
		t.Fatal("a moderator named themselves the removal approver")
	}
	if _, err := writeSettings(t, moderatorCtx("owner-1"), existingSettings(), args, false); err != nil {
		t.Fatalf("the current owner was refused handing the role on: %v", err)
	}
	opCtx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "staff-1", Role: auth.RoleOwner})
	if _, err := writeSettings(t, opCtx, existingSettings(), args, false); err != nil {
		t.Fatalf("an operator setting the store up was refused: %v", err)
	}
}

// approverRefusal is the one sentence a refused removal approver gets.
const approverRefusal = "reviews: Choose someone in this store's organization who can moderate reviews."

// approverArgs names who as the store's removal approver.
func approverArgs(who string) map[string]any {
	return map[string]any{"siteId": "site-1", "storeId": "store-live", "removalApproverUserId": who}
}

// THE REMOVAL APPROVER IS ONE OF THE STORE'S OWN PEOPLE WHO CAN MODERATE IT
// (review focus 4). Not somebody outside the store's organization, however much
// they hold in their own; not one of the cluster's operators, who host the
// reviews and decide none; nobody who could not moderate there. Whoever names
// them -- an operator, or the current approver handing the role on -- and in
// addition to the rule on who may name one, which is unchanged. Each refusal
// is reached by asking about the person named and nobody else: the write
// never reads the organization's roster.
func TestSettingsRefuseAnApproverWhoCannotModerate(t *testing.T) {
	name := approverArgs
	for _, c := range []struct{ who, why string }{
		{"outsider-1", "a person of another organization, who moderates there"},
		{"staff-1", "a cluster operator in the store's own group"},
		{auth.RoleTestUserPrefix + "stand-in", "a role test's stand-in in the store's own group"},
		{"viewer-1", "a person of the organization without the reviews part"},
		{"doorless-1", "a person with the part who cannot open the organization's Deployables"},
		{"away-1", "a deactivated person"},
		{"gone-1", "a person who left the organization"},
		{"nobody-1", "nobody this cluster knows"},
	} {
		for _, namer := range []struct {
			ctx  context.Context
			name string
		}{{operatorCtxFor("op-1"), "an operator"}, {moderatorCtx("owner-1"), "the current approver"}} {
			roster := shopRoster()
			_, err := writeSettingsOn(t, namer.ctx, roster, existingSettings(), name(c.who), false)
			if err == nil || err.Error() != approverRefusal {
				t.Errorf("%s naming %s (%s): err = %v, want %q", namer.name, c.who, c.why, err, approverRefusal)
			}
			if members := roster.read("membersOfGroup"); len(members) != 0 {
				t.Errorf("%s naming %s: the write read the organization's roster (%v)", namer.name, c.who, members)
			}
			for _, about := range append(roster.read("userByIdSystem"), roster.asked...) {
				if about != c.who {
					t.Errorf("%s naming %s: the write read about %s", namer.name, c.who, about)
				}
			}
		}
	}
	// THE STORE'S MODERATOR IS ACCEPTED, from either, and the row says so.
	out, err := writeSettings(t, operatorCtxFor("op-1"), existingSettings(), name("mod-1"), false)
	if err != nil || out["removalApproverUserId"] != "mod-1" {
		t.Fatalf("an operator naming the store's moderator: %v (saved %v)", err, out["removalApproverUserId"])
	}
	if _, err := writeSettings(t, moderatorCtx("owner-1"), existingSettings(), name("mod-1"), false); err != nil {
		t.Fatalf("the current approver handing the role to the store's moderator: %v", err)
	}
	// Naming nobody names nobody to check, and the rule on who may name one
	// still stands over a moderator who is not the approver.
	if _, err := writeSettings(t, operatorCtxFor("op-1"), existingSettings(), name(""), false); err != nil {
		t.Fatalf("an operator clearing the approver: %v", err)
	}
	if _, err := writeSettings(t, moderatorCtx("mod-1"), existingSettings(), name("mod-1"), false); err == nil || err.Error() == approverRefusal {
		t.Fatalf("a moderator naming themselves: err = %v, want the rule on who names the approver", err)
	}

	// A SETTINGS ROW WRITTEN BEFORE THE ORGANIZATION WAS STAMPED carries none,
	// and the store's organization is then its site's -- for this check as for
	// reviewsPeople, which offers the site's people (storeOrganization). Every
	// person the picker offers is accepted, from either namer; somebody outside
	// is still refused; and the current approver's write leaves the row untied,
	// because only an operator records whose store it is.
	legacy := existingSettings()
	delete(legacy, "accountId")
	offered, err := readPeople(t, moderatorCtx("owner-1"), settingsReader(legacy), shopRoster(),
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil || strings.Join(peopleIDs(offered), ",") != "owner-1,mod-1" {
		t.Fatalf("people of a store whose settings carry no organization = %v (%v), want its site's two moderators", peopleIDs(offered), err)
	}
	for _, namer := range []struct {
		ctx  context.Context
		name string
	}{{operatorCtxFor("op-1"), "an operator"}, {moderatorCtx("owner-1"), "the current approver"}} {
		for _, who := range peopleIDs(offered) {
			if _, err := writeSettings(t, namer.ctx, legacy, name(who), false); err != nil {
				t.Errorf("%s naming %s, whom the picker offers for settings with no organization: %v", namer.name, who, err)
			}
		}
		if _, err := writeSettings(t, namer.ctx, legacy, name("outsider-1"), false); err == nil || err.Error() != approverRefusal {
			t.Errorf("%s naming outsider-1 for settings with no organization: err = %v, want %q", namer.name, err, approverRefusal)
		}
	}
	handed, err := writeSettings(t, moderatorCtx("owner-1"), legacy, name("mod-1"), false)
	if err != nil {
		t.Fatalf("the current approver handing the role on, settings with no organization: %v", err)
	}
	if got, set := handed["accountId"]; set {
		t.Fatalf("the current approver's write tied the store to organization %v; only an operator records it", got)
	}
}

// NAMING THE APPROVER ASKS ABOUT THAT PERSON ALONE. Whether the one person named
// can moderate is answered from the organization's groups, the person's own
// memberships, their own user row and the engine's answer for them -- never
// the organization's roster -- so naming an approver costs one person's reads
// however many people the organization has. Every read is the deployment's,
// and only the user row's carries internal origin.
func TestNamingTheApproverResolvesThatPersonAlone(t *testing.T) {
	deployment := auth.SystemActor(peopleActor).UserId
	for _, namer := range []struct {
		ctx  context.Context
		name string
	}{{operatorCtxFor("op-1"), "an operator"}, {moderatorCtx("owner-1"), "the current approver"}} {
		roster := shopRoster()
		if _, err := writeSettingsOn(t, namer.ctx, roster, existingSettings(), approverArgs("mod-1"), false); err != nil {
			t.Fatalf("%s naming mod-1: %v", namer.name, err)
		}
		if got := strings.Join(roster.asked, ","); got != "mod-1" {
			t.Errorf("%s naming mod-1: the engine was asked about %q, want mod-1 alone", namer.name, got)
		}
		for query, want := range map[string]string{
			"groupsForAccount": "acct-shop", "groupsForUser": "mod-1", "userByIdSystem": "mod-1", "membersOfGroup": "",
		} {
			if got := strings.Join(roster.read(query), ","); got != want {
				t.Errorf("%s naming mod-1: %s read about %q, want %q", namer.name, query, got, want)
			}
		}
		for _, read := range roster.reads {
			if read.actor != deployment || read.internal != (read.query == "userByIdSystem") {
				t.Errorf("%s naming mod-1: %+v, want it read as %s, with internal origin on the user row alone", namer.name, read, deployment)
			}
		}
	}
}

// THE APPROVER CHECK IS ANSWERED IN FULL OR NOT AT ALL, as the list is. A read
// behind it that failed -- the engine's while it resolved the person, or one of
// the deployment's own, failing outright or answering from part of what it
// reads -- refuses the write for the read, not for the person, and nothing is
// written (writeSettingsOn asserts it).
func TestTheApproverCheckRefusesWhenAReadBehindItFails(t *testing.T) {
	breaks := map[string]func(*orgRoster){
		"the engine's grant read": func(r *orgRoster) { r.failRead = true },
	}
	for _, query := range []string{"groupsForAccount", "groupsForUser", "userByIdSystem"} {
		breaks[query+" failing"] = func(r *orgRoster) { r.errorIn = query }
		breaks[query+" answering from part of what it reads"] = func(r *orgRoster) { r.noteFailureIn = query }
	}
	for name, broken := range breaks {
		roster := shopRoster()
		broken(roster)
		_, err := writeSettingsOn(t, operatorCtxFor("op-1"), roster, existingSettings(), approverArgs("mod-1"), false)
		if err == nil || err.Error() == approverRefusal {
			t.Errorf("%s: err = %v, want the write refused for the read that failed", name, err)
		}
	}
	// The refusal says what was read: the person named, alone. Naming an
	// approver reads none of the organization's people.
	roster := shopRoster()
	roster.errorIn = "userByIdSystem"
	_, err := writeSettingsOn(t, operatorCtxFor("op-1"), roster, existingSettings(), approverArgs("mod-1"), false)
	if want := "reviews: reading the approver named: the rows could not be read"; err == nil || err.Error() != want {
		t.Errorf("the named approver's user row failing: err = %v, want %q", err, want)
	}
}

// A ROLE TEST'S STAND-IN IS NEVER ONE OF THE STORE'S PEOPLE (ruling R3). While
// an organization role's test is live its 30-minute identity is a member of the
// organization's account group, and the engine answers it able to moderate --
// but it is nobody, so it is never listed and never accepted as the approver.
func TestARoleTestStandInIsNeverAModeratorOrTheApprover(t *testing.T) {
	persona := auth.RoleTestUserPrefix + "stand-in"
	people, err := readPeople(t, operatorCtxFor("op-1"), settingsReader(existingSettings()), shopRoster(),
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range peopleIDs(people) {
		if auth.IsRoleTestUser(id) {
			t.Fatalf("people = %v: a role test's stand-in is listed as a moderator", peopleIDs(people))
		}
	}
	if _, err := writeSettings(t, operatorCtxFor("op-1"), existingSettings(),
		map[string]any{"siteId": "site-1", "storeId": "store-live", "removalApproverUserId": persona}, false); err == nil || err.Error() != approverRefusal {
		t.Fatalf("naming a role test's stand-in the approver: err = %v, want %q", err, approverRefusal)
	}
}

// THE APPROVER IS KNOWN WHICHEVER WAY THEIR ID IS SPELLED. A person is chosen
// by the id reviewsPeople answers, bare; a signed-in caller's id arrives as the
// token carried it, `v1:identity:user:<id>` on a deployed cluster. One person
// must be one person to every rule that asks whether they approve removals.
func TestTheApproverIsKnownInEitherSpellingOfTheirId(t *testing.T) {
	canonical := moderatorCtx("v1:identity:user:owner-1")
	set := readSettings(t, canonical, &readsReader{settings: existingSettings()})
	if viewer, _ := set["viewer"].(map[string]any); viewer["approvesRemovals"] != true {
		t.Fatalf("viewer = %v, want the approver recognised", set["viewer"])
	}
	if may, _ := set["may"].(map[string]any); may["nameApprover"] != true {
		t.Fatalf("may = %v, want the approver able to hand the role on", set["may"])
	}
	published := &readsReader{reviews: []map[string]any{readReview("r1", "boot", "Warm", 1)},
		decisions: []map[string]any{decisionRow("r1", ActionPublish, 1, 0)}, settings: existingSettings()}
	got, err := getReview(t, canonical, published, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if acts := actsOf(got["acts"]); acts != "escalate,remove,erase" {
		t.Fatalf("the approver's acts = %q, want removal among them", acts)
	}
	if _, err := writeSettings(t, canonical, existingSettings(),
		map[string]any{"siteId": "site-1", "storeId": "store-live", "removalApproverUserId": "mod-1"}, false); err != nil {
		t.Fatalf("the approver, signed in under their canonical id, handing the role on: %v", err)
	}
}

// THE APPROVER IS STORED BY THEIR BARE ID, however the namer spelled it: the
// spelling reviewsPeople answers and clients write, so the row reads the same
// whoever named the person and however.
func TestTheApproverIsStoredByTheirBareId(t *testing.T) {
	for _, namer := range []struct {
		ctx  context.Context
		name string
	}{{operatorCtxFor("op-1"), "an operator"}, {moderatorCtx("owner-1"), "the current approver"}} {
		out, err := writeSettings(t, namer.ctx, existingSettings(), approverArgs("v1:identity:user:mod-1"), false)
		if err != nil {
			t.Fatalf("%s naming v1:identity:user:mod-1: %v", namer.name, err)
		}
		if out["removalApproverUserId"] != "mod-1" {
			t.Errorf("%s naming v1:identity:user:mod-1 saved %v, want mod-1", namer.name, out["removalApproverUserId"])
		}
	}
}

// AN ID OF ANOTHER CONCEPT NAMES NOBODY. A person's id is spelled bare or under
// the user concept's own prefix, and that prefix is the only one this package
// strips (bareUser). `v1:accounts:account:mod-1` is some other row's id that
// happens to end the way mod-1's does; reading it as mod-1 would let a stray
// id, or a caller who means it, name a person nobody named -- as the approver,
// or as one of the store's people. Nor is a user id with the prefix twice
// anybody's, not even its own: stripped once it is still canonical, which no
// person is spelled as here, though the engine answers mod-1's memberships for
// it -- so a membership spelled so lists nobody, as the write names nobody by
// it.
func TestAnIdOfAnotherConceptNamesNobody(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"v1:identity:user:mod-1", "mod-1", true},
		{"mod-1", "v1:identity:user:mod-1", true},
		{"mod-1", "mod-1", true},
		{"v1:accounts:account:mod-1", "mod-1", false},
		{"v1:identity:group:mod-1", "v1:identity:user:mod-1", false},
		{"v1:identity:user:v1:identity:user:mod-1", "mod-1", false},
		{"v1:identity:user:v1:identity:user:mod-1", "v1:identity:user:v1:identity:user:mod-1", false},
		{"v1:identity:user:", "", false},
	} {
		if got := sameUser(c.a, c.b); got != c.same {
			t.Errorf("sameUser(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
	for _, namer := range []struct {
		ctx  context.Context
		name string
	}{{operatorCtxFor("op-1"), "an operator"}, {moderatorCtx("owner-1"), "the current approver"}} {
		for _, who := range []string{"v1:accounts:account:mod-1", "v1:identity:user:v1:identity:user:mod-1"} {
			if _, err := writeSettings(t, namer.ctx, existingSettings(), approverArgs(who), false); err == nil || err.Error() != approverRefusal {
				t.Errorf("%s naming %s: err = %v, want %q", namer.name, who, err, approverRefusal)
			}
		}
	}
	// A membership spells its member the same two ways: solo-2 under the user
	// concept's prefix is solo-2, and the row naming an account's id is nobody's.
	// So is the row naming solo-3 with the prefix twice, whom the engine would
	// answer as solo-3: listed, they would be offered and then refused.
	roster := shopRoster()
	held := heldIn("acct-shop", readData, openDeployables, moderateReviews)
	roster.members["group-shop"] = append(roster.members["group-shop"],
		rosterMember("group-shop", "v1:accounts:account:solo-1", "added", "active"),
		rosterMember("group-shop", "v1:identity:user:solo-2", "added", "active"),
		rosterMember("group-shop", "v1:identity:user:v1:identity:user:solo-3", "added", "active"))
	roster.users["solo-1"], roster.users["solo-2"] = rosterUser("solo-1", "Sol One", "user", true), rosterUser("solo-2", "Sol Two", "user", true)
	roster.users["solo-3"] = rosterUser("solo-3", "Sol Three", "user", true)
	roster.holds["solo-1"], roster.holds["solo-2"], roster.holds["solo-3"] = held, held, held
	people, err := readPeople(t, moderatorCtx("mod-1"), settingsReader(existingSettings()), roster,
		map[string]any{"storeId": "store-live", "siteId": "site-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(peopleIDs(people), ","); got != "owner-1,mod-1,solo-2" {
		t.Fatalf("people = %s, want owner-1,mod-1,solo-2: a member spelled under another concept's prefix, or under the user's twice, is nobody", got)
	}
}

func TestEffectiveSettingsDefaults(t *testing.T) {
	s := EffectiveSettings(nil)
	if s.LinkLifetimeDays != 14 || s.RequestDelayDays != 14 || s.RequestsEnabled || s.PublicDisplay {
		t.Fatalf("defaults = %+v, want 14-day links, requests 14 days after the order, requests off, display off", s)
	}
	s = EffectiveSettings(map[string]any{"linkLifetimeDays": 7, "requestsEnabled": true})
	if s.LinkLifetimeDays != 7 || !s.RequestsEnabled {
		t.Fatalf("explicit values ignored: %+v", s)
	}
}

// TWO STORES ARE TWO ROWS, which is the whole point of the derived id: a
// merchant must be able to show reviews live while hiding them under preview.
func TestTheDisplayToggleIsPerStore(t *testing.T) {
	op := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "staff-1", Role: auth.RoleOwner})
	hidden, err := writeSettings(t, op, nil, map[string]any{"siteId": "site-1", "storeId": "store-live", "publicDisplay": false}, true)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := writeSettings(t, op, nil, map[string]any{"siteId": "site-1", "storeId": "store-live", "publicDisplay": true}, true)
	if err != nil {
		t.Fatal(err)
	}
	if hidden["publicDisplay"] == shown["publicDisplay"] {
		t.Fatal("toggle did not change")
	}
	// writeSettings asserts each answer is the named store's own row id.
	if _, err := writeSettings(t, op, nil, map[string]any{"siteId": "site-1", "storeId": "store-dev", "publicDisplay": true}, true); err != nil {
		t.Fatal(err)
	}
	if SettingsRowID("store-live") == SettingsRowID("store-dev") {
		t.Fatal("two stores share one settings row id")
	}
	if strings.Contains(SettingsRowID("v1:shopify:store:abc"), ":") {
		t.Fatalf("SettingsRowID = %q: the engine refuses a row id with a colon", SettingsRowID("v1:shopify:store:abc"))
	}
}

// A STORE NOBODY HAS SET UP IS SET UP BY THE CLUSTER'S OPERATORS, not by
// whoever reaches the call first.
func TestOnlyAnOperatorCreatesAStoresSettings(t *testing.T) {
	if _, err := writeSettings(t, moderatorCtx("mod-1"), nil,
		map[string]any{"siteId": "site-1", "storeId": "store-live", "publicDisplay": true}, true); err == nil {
		t.Fatal("a non-operator created a store's settings row")
	}
}

// A FIELD THE QUERY PROJECTS BUT THE ROW NEVER SET COMES BACK EMPTY, and empty
// is not zero: a store that never chose a delay waits 14 days, not none.
func TestAnUnsetSettingReadAsNullKeepsItsDefault(t *testing.T) {
	s := EffectiveSettings(map[string]any{"requestDelayDays": nil, "linkLifetimeDays": nil, "requestsEnabled": true})
	if s.RequestDelayDays != 14 || s.LinkLifetimeDays != 14 {
		t.Fatalf("settings = %+v, want the 14-day defaults for fields the row never set", s)
	}
	if s = EffectiveSettings(map[string]any{"requestDelayDays": 0}); s.RequestDelayDays != 0 {
		t.Fatalf("an explicit 0-day delay was not kept: %+v", s)
	}
}

// THE REQUEST SENDER IS AN OPERATOR'S CHOICE. It may be the installation's own
// mailbox, standing in for an organization with none, and only the people who
// run the installation may hand that mailbox to a store.
func TestOnlyAnOperatorNamesTheRequestSender(t *testing.T) {
	args := map[string]any{"siteId": "site-1", "storeId": "store-live", "requestSenderIdentityId": "si-house"}
	if _, err := writeSettings(t, moderatorCtx("owner-1"), existingSettings(), args, false); err == nil {
		t.Fatal("the store's own owner named the mailbox its requests leave from")
	}
	opCtx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "staff-1", Role: auth.RoleOwner})
	out, err := writeSettings(t, opCtx, existingSettings(), args, false)
	if err != nil {
		t.Fatalf("an operator naming the sender was refused: %v", err)
	}
	if out["requestSenderIdentityId"] != "si-house" {
		t.Fatalf("saved = %v, want the sender the operator named", out["requestSenderIdentityId"])
	}
	// And it survives every later write, whoever makes it.
	kept := existingSettings()
	kept["requestSenderIdentityId"] = "si-house"
	again, err := writeSettings(t, moderatorCtx("owner-1"), kept, map[string]any{"siteId": "site-1", "storeId": "store-live", "notifyEmail": "a@example.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if again["requestSenderIdentityId"] != "si-house" {
		t.Fatalf("a later write lost the sender: %v", again)
	}
	if got := EffectiveSettings(kept).RequestSenderIdentityID; got != "si-house" {
		t.Fatalf("effective sender = %q", got)
	}
}
