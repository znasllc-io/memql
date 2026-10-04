package memql

import (
	"strconv"
	"strings"
	"testing"
)

// TestSiteStoreSettingsGuard pins the rules for v1:platform:site.storeSettings
// (memql#5602): per-store runtime settings, an object of store id to the same
// flat object of plain strings `settings` is. Every inner object is held to
// `settings`' own rules -- they land in the same document -- and the store ids
// are BARE, so one store is never keyed two ways.
//
// DB-free through the nil receiver, like TestSiteSettingsGuard beside it.
func TestSiteStoreSettingsGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	ownerCtx, ownerActor := ownerActorContext()
	sysCtx, sysActor := systemSeedContext()
	g := validatorOnNilEngine()
	write := func(raw any) map[string]any { return map[string]any{"storeSettings": raw} }

	// ---- where the guard has nothing to say ----

	for name, payload := range map[string]map[string]any{
		"a write that names no storeSettings": {"hostname": "shop.example.com", "settings": map[string]any{"a": "b"}},
		"an explicit null":                    write(nil),
		"an empty object":                     write(map[string]any{}),
		"a store with no values":              write(map[string]any{"acme": map[string]any{}}),
	} {
		t.Run(name+" passes", func(t *testing.T) {
			if err := g.validateSiteStoreSettings(userCtx, payload, false, userActor); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}

	t.Run("plain values for well-named stores pass", func(t *testing.T) {
		payload := write(map[string]any{
			"acme":                                 map[string]any{"customerAccountClientId": "shp_client", "wholesaleAdapter": "shopifyB2B"},
			"acme-dev":                             map[string]any{"customerAccountClientId": "shp_dev"},
			"store_2.eu":                           map[string]any{"region": "eu"},
			"0c9a4f2e-3a43-4f5e-9d1e-2b6a6f1d7c11": map[string]any{"a": ""},
		})
		if err := g.validateSiteStoreSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("well-formed per-store settings must pass; got %v", err)
		}
	})

	// ---- the shape ----

	t.Run("storeSettings that are not an object are refused", func(t *testing.T) {
		for _, raw := range []any{"acme", []any{"acme"}, float64(1)} {
			if err := g.validateSiteStoreSettings(userCtx, write(raw), false, userActor); err == nil {
				t.Errorf("%T must be refused", raw)
			}
		}
	})

	t.Run("a store entry that is not an object is refused and named", func(t *testing.T) {
		err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": "client"}), false, userActor)
		if err == nil || !strings.Contains(err.Error(), "acme") {
			t.Fatalf("want a refusal naming the store, got %v", err)
		}
	})

	t.Run("a store id that is not a bare id is refused and named", func(t *testing.T) {
		for _, id := range []string{"", "v1:shopify:store:acme", "acme dev", "-acme", "acme/dev", strings.Repeat("s", 129)} {
			err := g.validateSiteStoreSettings(userCtx, write(map[string]any{id: map[string]any{"a": "b"}}), false, userActor)
			if err == nil {
				t.Fatalf("store id %q must be refused", id)
			}
			if id != "" && len(id) < 100 && !strings.Contains(err.Error(), id) {
				t.Errorf("the refusal must name the store id %q; got %q", id, err.Error())
			}
		}
	})

	// ---- each store's values, held to settings' own rules ----

	t.Run("a malformed key is refused naming the store and the key", func(t *testing.T) {
		err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": map[string]any{"client-id": "x"}}), false, userActor)
		if err == nil || !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "client-id") {
			t.Fatalf("want a refusal naming store and key, got %v", err)
		}
	})

	t.Run("a key ending in Ref is refused", func(t *testing.T) {
		err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": map[string]any{"clientSecretRef": "SHOPIFY_ACME_ADMIN_TOKEN"}}), false, userActor)
		if err == nil || !strings.Contains(err.Error(), "clientSecretRef") {
			t.Fatalf("a Ref key must be refused by name, got %v", err)
		}
	})

	t.Run("a non-string value is refused", func(t *testing.T) {
		for _, v := range []any{float64(3), true, map[string]any{"x": "y"}, []any{"a"}, nil} {
			if err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": map[string]any{"k": v}}), false, userActor); err == nil {
				t.Errorf("a %T value must be refused", v)
			}
		}
	})

	t.Run("an over-long value is refused at the settings cap", func(t *testing.T) {
		err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": map[string]any{"k": strings.Repeat("v", defaultSiteSettingsMaxValueLength+1)}}), false, userActor)
		if err == nil || !strings.Contains(err.Error(), "MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH") {
			t.Fatalf("want the value cap, got %v", err)
		}
	})

	t.Run("too many keys for one store are refused at the settings cap", func(t *testing.T) {
		values := map[string]any{}
		for i := 0; i <= defaultSiteSettingsMaxKeys; i++ {
			values["k"+strconv.Itoa(i)] = "v"
		}
		err := g.validateSiteStoreSettings(userCtx, write(map[string]any{"acme": values}), false, userActor)
		if err == nil || !strings.Contains(err.Error(), "MEMQL_SITE_SETTINGS_MAX_KEYS") {
			t.Fatalf("want the key cap, got %v", err)
		}
	})

	t.Run("too many stores are refused", func(t *testing.T) {
		stores := map[string]any{}
		for i := 0; i <= siteStoreSettingsMaxStores; i++ {
			stores["store"+strconv.Itoa(i)] = map[string]any{"k": "v"}
		}
		if err := g.validateSiteStoreSettings(userCtx, write(stores), false, userActor); err == nil {
			t.Fatal("more stores than the cap must be refused")
		}
	})

	// ---- systemOwned, as for settings ----

	t.Run("a systemOwned row refuses a user and a cluster owner", func(t *testing.T) {
		payload := write(map[string]any{"acme": map[string]any{"a": "b"}})
		for name, c := range map[string]struct {
			actor string
			err   error
		}{
			"user":  {userActor, g.validateSiteStoreSettings(userCtx, payload, true, userActor)},
			"owner": {ownerActor, g.validateSiteStoreSettings(ownerCtx, payload, true, ownerActor)},
		} {
			if c.err == nil || !strings.Contains(c.err.Error(), "systemOwned") {
				t.Errorf("%s: want the systemOwned refusal, got %v", name, c.err)
			}
		}
		if err := g.validateSiteStoreSettings(sysCtx, payload, true, sysActor); err != nil {
			t.Errorf("a system actor writes the deployment's own row; got %v", err)
		}
	})
}
