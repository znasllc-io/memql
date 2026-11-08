package reviewspack_test

// live_people_e2e_test.go -- WHO CAN MODERATE A STORE, AGAINST A REAL DATABASE.
//
// reads_test.go and settings_test.go prove reviewsPeople and the removal
// approver's check as functions over rows and over a stand-in for the engine's
// resolver. This proves them through the real engine: the builtin reaches its
// handler through the DSL, the organization gate admits a caller who holds the
// reviews part in the store's organization and refuses one who holds it only
// in another, the organization's roster is read as the deployment reads it,
// and the engine's own resolver answers who can moderate there -- with the ids
// the wire carries, a signed-in caller's canonical and a chosen person's bare.
//
// Postgres-gated: skips when no database is reachable; MEMQL_REQUIRE_DB=1
// turns that skip into a failure.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// peopleWorld is one store of one organization, in the production shape: the
// organization's account group opens Deployables and grants its reviews part,
// and its people's role holds the data verbs -- writer, the member rung's
// compiled name, since this engine installs no role catalog.
type peopleWorld struct {
	eng                  *memql.MemQLEngine
	n, store, site       string
	moderator, colleague string // the two who can moderate
	staff                string // a cluster operator placed in the organization's group
	persona              string // a role test's stand-in in the organization's group
	away                 string // deactivated
	bystander            string // in the organization, without the reviews part
	outsider             string // holds everything in another organization
}

func newPeopleWorld(t *testing.T) *peopleWorld {
	t.Helper()
	eng := livePackEngine(t)
	n := strconv.FormatInt(time.Now().UnixNano(), 36)
	w := &peopleWorld{eng: eng, n: n, store: "store-reviews-people-" + n, site: "site-reviews-people-" + n}
	account, other := "acct-reviews-people-"+n, "acct-reviews-people-other-"+n
	seedSiteIn(t, eng, w.site, "user-reviews-people-shop-"+n, w.store, account)
	group, otherGroup := seedOrganization(t, eng, account), seedOrganization(t, eng, other)
	for _, g := range []string{group, otherGroup} {
		grantGroup(t, eng, g, "read", "app:deployables")
		grantGroup(t, eng, g, "execute", "app:deployables/reviews")
	}
	support := "group-reviews-people-support-" + n
	seedGroup(t, eng, support, "custom", account)

	person := func(name, display, role string, active bool, groupID, in, origin string) string {
		id := "user-reviews-people-" + name + "-" + n
		seedPersonIn(t, eng, id, display, role, active, groupID, in, origin)
		return id
	}
	member := string(auth.RoleWriter)
	w.moderator = person("moderator", "Robin Moderator", member, true, group, account, "added")
	w.colleague = person("colleague", "Avery Colleague", member, true, group, account, "added")
	w.staff = person("staff", "Sam Staff", string(auth.RoleDeveloper), true, group, account, "added")
	w.away = person("away", "Alex Away", member, false, group, account, "added")
	w.bystander = person("bystander", "Bea Bystander", member, true, support, account, "added")
	w.outsider = person("outsider", "Oli Outsider", member, true, otherGroup, other, "added")
	w.persona = auth.RoleTestUserPrefix + "reviews-people-" + n
	seedPersonIn(t, eng, w.persona, "Test Member", member, true, group, account, "role-test")
	return w
}

// as is a signed-in person of the organization as a deployed cluster spells
// them: the canonical id their token carries, and the role their row holds.
func (w *peopleWorld) as(userID string) context.Context {
	return asPerson("v1:identity:user:"+userID, auth.RoleWriter)
}

func (w *peopleWorld) operator() context.Context {
	return asPerson("v1:identity:user:user-reviews-people-op-"+w.n, auth.RoleOwner)
}

func (w *peopleWorld) people(t *testing.T, ctx context.Context) []map[string]any {
	t.Helper()
	answer := payloadOf(t, w.eng, ctx, `builtin reviewsPeople(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
	list, _ := answer["people"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, p.(map[string]any))
	}
	return out
}

func (w *peopleWorld) nameApprover(ctx context.Context, who string) error {
	_, err := w.eng.Execute(ctx, `builtin reviewsUpdateSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+
		`, removalApproverUserId: `+quote(who)+`)`)
	return err
}

func idsOf(people []map[string]any) string {
	out := make([]string, 0, len(people))
	for _, p := range people {
		out = append(out, asText(p["userId"]))
	}
	return strings.Join(out, ",")
}

const approverRefusal = "Choose someone in this store's organization who can moderate reviews."

func TestLiveE2E_ReviewsPeopleListsTheStoresModerators(t *testing.T) {
	w := newPeopleWorld(t)

	// A moderator of the store reads its people: the two who can moderate, by
	// name -- not the operator in its group, the stand-in, the deactivated,
	// the person without the part, or anybody of another organization.
	people := w.people(t, w.as(w.moderator))
	if got := idsOf(people); got != w.colleague+","+w.moderator {
		t.Fatalf("people = %s, want %s,%s", got, w.colleague, w.moderator)
	}
	first := people[0]
	if first["displayName"] != "Avery Colleague" || first["email"] != w.colleague+"@example.test" || first["mayRemove"] != false || len(first) != 4 {
		t.Fatalf("a person = %v, want their id, name and address, and that they approve nothing yet", first)
	}

	// The cluster's operators read the same people: the answer is the store's.
	if got := idsOf(w.people(t, w.operator())); got != w.colleague+","+w.moderator {
		t.Fatalf("an operator read %s", got)
	}

	// THE ORGANIZATION GATE: the part held in ANOTHER organization opens
	// nothing here, and a person of this one without the part reads nothing.
	for _, who := range []string{w.outsider, w.bystander} {
		_, err := w.eng.Execute(w.as(who), `builtin reviewsPeople(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
		if err == nil || !strings.Contains(err.Error(), memql.CodeCapabilityNotHeld) {
			t.Errorf("%s read the store's people: err = %v, want the organization gate's refusal", who, err)
		}
	}
}

// Review focus 4: the removal approver cannot be set to someone outside the
// store's organization or to an operator, through the real engine.
func TestLiveE2E_SettingsRefuseAnApproverWhoCannotModerate(t *testing.T) {
	w := newPeopleWorld(t)
	for _, who := range []string{w.outsider, w.staff, w.persona, w.away, w.bystander, "user-reviews-people-nobody-" + w.n} {
		if err := w.nameApprover(w.operator(), who); err == nil || !strings.Contains(err.Error(), approverRefusal) {
			t.Errorf("an operator naming %s the approver: err = %v, want %q", who, err, approverRefusal)
		}
	}
	// Refused before anything was written: the store is still not set up.
	if settings := payloadOf(t, w.eng, w.as(w.moderator), `builtin reviewsSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`); settings["exists"] != false {
		t.Fatalf("settings = %v after refusals only, want none written", settings)
	}

	// The store's moderator, by the bare id the list answers.
	if err := w.nameApprover(w.operator(), w.colleague); err != nil {
		t.Fatalf("an operator naming the store's moderator: %v", err)
	}
	// Recognised under the canonical id they sign in with, and said so on the list.
	settings := payloadOf(t, w.eng, w.as(w.colleague), `builtin reviewsSettings(storeId: `+quote(w.store)+`, siteId: `+quote(w.site)+`)`)
	viewer, _ := settings["viewer"].(map[string]any)
	may, _ := settings["may"].(map[string]any)
	if viewer["approvesRemovals"] != true || may["nameApprover"] != true {
		t.Fatalf("the approver's settings = %v, want them recognised and able to hand the role on", settings)
	}
	for _, p := range w.people(t, w.as(w.moderator)) {
		if want := asText(p["userId"]) == w.colleague; p["mayRemove"] != want {
			t.Fatalf("%s mayRemove = %v, want %v", p["userId"], p["mayRemove"], want)
		}
	}

	// The approver hands the role on: to the store's people, and nobody else.
	if err := w.nameApprover(w.as(w.colleague), w.outsider); err == nil || !strings.Contains(err.Error(), approverRefusal) {
		t.Fatalf("the approver handing the role outside the organization: err = %v, want %q", err, approverRefusal)
	}
	if err := w.nameApprover(w.as(w.colleague), w.moderator); err != nil {
		t.Fatalf("the approver handing the role to the store's moderator: %v", err)
	}
}
