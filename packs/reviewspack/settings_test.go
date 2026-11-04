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
// that. These tests pin the merge, the defaults and who may name the owner
// who approves removals.

func settingsReader(row map[string]any) *decideReader {
	return &decideReader{settings: row}
}

func writeSettings(t *testing.T, ctx context.Context, row map[string]any, args map[string]any, display bool) (map[string]any, error) {
	t.Helper()
	w := &fakeWriter{}
	p := &Provider{reader: settingsReader(row), writer: w}
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
