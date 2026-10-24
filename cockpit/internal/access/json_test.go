package access

import (
	"encoding/json"
	"strings"
	"testing"
)

func renderJSON(t *testing.T, s Summary, cluster string) map[string]any {
	t.Helper()
	var b strings.Builder
	if err := RenderJSON(&b, s, cluster); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b.String())
	}
	return got
}

// block pulls one top-level object, FAILING rather than panicking when it is
// missing. An unchecked assertion here aborts the whole test binary on an
// interface conversion, abandoning every other test in the package and naming
// the conversion instead of the broken contract -- in the tests whose entire
// job is to catch that contract changing.
func block(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	raw, present := doc[key]
	if !present {
		t.Fatalf("%q is missing from the JSON; a caller cannot tell absence from empty: %#v", key, doc)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object: %#v", key, raw, raw)
	}
	return obj
}

// A SCRIPT has to tell "no role" from "the cluster never said", for the same
// reason a reader does -- and JSON makes it easier to get wrong, because a
// missing key and a false one are both falsey.
func TestRenderJSONMarksAnUnreportedRoleAsUnreportedRatherThanOmittingIt(t *testing.T) {
	role := block(t, renderJSON(t, Summary{UserID: "u_ada"}, "acme"), "role")

	if reported, _ := role["reported"].(bool); reported {
		t.Errorf("role.reported = true, want false")
	}
	if onWire, _ := role["on_wire"].(bool); onWire {
		t.Errorf("role.on_wire = true for the pinned wire, which has no `role` field")
	}
	if _, present := role["slug"]; present {
		t.Errorf("role.slug is present for an unreported role: %#v", role)
	}
}

// on_wire and reported answer DIFFERENT questions, and a caller needs both:
// one says the contract predates the field, the other says this cluster sent
// nothing. Telling somebody to upgrade a cluster that is already current is
// its own kind of wrong answer.
func TestRenderJSONSeparatesAnUnknownFieldFromAnUnsentOne(t *testing.T) {
	silent := block(t, renderJSON(t, Summary{
		UserID: "u_ada",
		Role:   Role{OnTheWire: true},
	}, "acme"), "role")

	if onWire, _ := silent["on_wire"].(bool); !onWire {
		t.Errorf("role.on_wire = false for a build whose wire carries `role`")
	}
	if reported, _ := silent["reported"].(bool); reported {
		t.Errorf("role.reported = true for a cluster that sent no role")
	}
}

func TestRenderJSONCarriesTheSlugNameAndRank(t *testing.T) {
	doc := renderJSON(t, Summary{
		UserID: "u_ada",
		Role: Role{
			OnTheWire: true, Reported: true,
			Slug: "release-manager", Name: "Release Manager", Rank: 150, HasRank: true,
		},
	}, "acme")
	role := block(t, doc, "role")

	if reported, _ := role["reported"].(bool); !reported {
		t.Errorf("role.reported = false, want true")
	}
	if role["slug"] != "release-manager" {
		t.Errorf("role.slug = %v, want release-manager", role["slug"])
	}
	if role["name"] != "Release Manager" {
		t.Errorf("role.name = %v, want Release Manager", role["name"])
	}
	if rank, _ := role["rank"].(float64); rank != 150 {
		t.Errorf("role.rank = %v, want 150", role["rank"])
	}
	if doc["cluster"] != "acme" {
		t.Errorf("cluster = %v, want acme", doc["cluster"])
	}
}

// Rank 0 must survive as a REPORTED zero, not vanish through omitempty the way
// a bare int would.
func TestRenderJSONKeepsAnExplicitRankOfZero(t *testing.T) {
	role := block(t, renderJSON(t, Summary{
		Role: Role{OnTheWire: true, Reported: true, Slug: "retired-role", HasRank: true},
	}, "acme"), "role")

	rank, present := role["rank"]
	if !present {
		t.Fatalf("role.rank is absent for an explicit rank of 0: %#v", role)
	}
	if v, _ := rank.(float64); v != 0 {
		t.Errorf("role.rank = %v, want 0", rank)
	}
}

// Finding 19: the groups block had no coverage at all, which is how the
// omitempty defect below shipped.
func TestRenderJSONCarriesGroups(t *testing.T) {
	groups := block(t, renderJSON(t, Summary{
		Groups: Groups{OnTheWire: true, Reported: true, Items: []Group{
			{ID: "g_1", Name: "Platform", Kind: "team", AccountID: "acct_9", AccountName: "Acme Corp"},
		}},
	}, "acme"), "groups")

	items, ok := groups["items"].([]any)
	if !ok {
		t.Fatalf("groups.items is %T, want a list: %#v", groups["items"], groups)
	}
	if len(items) != 1 {
		t.Fatalf("len(groups.items) = %d, want 1", len(items))
	}
	one, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("groups.items[0] is %T, want an object", items[0])
	}
	for key, want := range map[string]string{
		"id": "g_1", "name": "Platform", "kind": "team",
		"account_id": "acct_9", "account_name": "Acme Corp",
	} {
		if one[key] != want {
			t.Errorf("groups.items[0].%s = %v, want %q", key, one[key], want)
		}
	}
}

func TestRenderJSONCarriesTheAccountScope(t *testing.T) {
	scope := block(t, renderJSON(t, Summary{
		Scope: Scope{OnTheWire: true, Reported: true, AccountIDs: []string{"acct_9"}},
	}, "acme"), "account_scope")

	ids, ok := scope["account_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != "acct_9" {
		t.Errorf("account_scope.account_ids = %#v, want [acct_9]", scope["account_ids"])
	}
	if every, present := scope["every_account"]; !present || every != false {
		t.Errorf("account_scope.every_account = %#v, want an explicit false", scope["every_account"])
	}
}

func TestRenderJSONCarriesEveryAccount(t *testing.T) {
	scope := block(t, renderJSON(t, Summary{
		Scope: Scope{OnTheWire: true, Reported: true, EveryAccount: true},
	}, "acme"), "account_scope")

	if every, _ := scope["every_account"].(bool); !every {
		t.Errorf("account_scope.every_account = %#v, want true", scope["every_account"])
	}
}

// Finding 8: a false bool is wire-identical to silence, so emitting one for a
// cluster that never answered is a confident negative invented by the cockpit.
// An audit script gating on `every_account == false` would believe it.
func TestRenderJSONDoesNotClaimEveryAccountIsFalseWhenNothingWasReported(t *testing.T) {
	scope := block(t, renderJSON(t, Summary{UserID: "u"}, "acme"), "account_scope")

	if _, present := scope["every_account"]; present {
		t.Errorf("account_scope.every_account is present for a cluster that reported no scope: %#v", scope)
	}
}

// Finding 9: `items` and `account_ids` are ALWAYS iterable. A dropped key is a
// third shape meaning the opposite of the second, and `.groups.items[]` dies
// on it -- for the ordinary user who simply belongs to no groups.
func TestRenderJSONAlwaysEmitsIterableLists(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Summary
	}{
		{"nothing on the wire", Summary{UserID: "u"}},
		{"on the wire, nothing reported", Summary{
			UserID: "u",
			Groups: Groups{OnTheWire: true},
			Scope:  Scope{OnTheWire: true},
		}},
		{"reported and empty", Summary{
			UserID: "u",
			Groups: Groups{OnTheWire: true, Reported: true},
			Scope:  Scope{OnTheWire: true, Reported: true},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := renderJSON(t, tc.s, "acme")
			if items, ok := block(t, doc, "groups")["items"].([]any); !ok || items == nil {
				t.Errorf("groups.items is not an iterable list: %#v", block(t, doc, "groups")["items"])
			}
			if ids, ok := block(t, doc, "account_scope")["account_ids"].([]any); !ok || ids == nil {
				t.Errorf("account_scope.account_ids is not an iterable list: %#v", block(t, doc, "account_scope")["account_ids"])
			}
		})
	}
}
