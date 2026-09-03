package memql

import (
	"strconv"
	"strings"
	"testing"
)

// TestSiteSettingsGuard pins the runtime-settings rules for v1:platform:site
// (epic memql#4906, decision P7): the form of a key, the plainness of a value,
// the two caps the env registry carries, the `Ref` refusal, and the
// systemOwned exemption that every status-class write on the concept carries.
//
// DB-free through the nil receiver, like the delete and status guards beside
// it: the guard reads the payload, the prior row's flag, the actor, and the
// process environment -- nothing on the engine.
func TestSiteSettingsGuard(t *testing.T) {
	userCtx, userActor := userActorContext()
	ownerCtx, ownerActor := ownerActorContext()
	sysCtx, sysActor := systemSeedContext()
	g := validatorOnNilEngine()

	// ---- the guard does not fire where there are no settings ----

	t.Run("a write that names no settings passes", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "bundleRef": "blob://sites/a/1/"}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a publish carries no settings of its own and must pass; got %v", err)
		}
	})

	t.Run("an explicit null clears and passes", func(t *testing.T) {
		payload := map[string]any{"hostname": "shop.example.com", "settings": nil}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("null settings is the cleared state; got %v", err)
		}
	})

	t.Run("an empty object clears and passes", func(t *testing.T) {
		payload := map[string]any{"settings": map[string]any{}}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("an empty object is the cleared state; got %v", err)
		}
	})

	// ---- the ordinary write ----

	t.Run("plain string values under well-formed keys pass", func(t *testing.T) {
		payload := map[string]any{"settings": map[string]any{
			"apiBase":     "https://api.example.com",
			"feature_x":   "on",
			"Region2":     "eu",
			"a":           "",
			"referenceId": "not-a-ref-suffix",
		}}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("well-formed settings must pass; got %v", err)
		}
	})

	// ---- the shape of a key ----

	t.Run("a malformed key is refused and named", func(t *testing.T) {
		for _, key := range []string{"", "1abc", "api-base", "api.base", "api base", "ünicode", strings.Repeat("k", 65)} {
			payload := map[string]any{"settings": map[string]any{key: "v"}}
			err := g.validateSiteSettings(userCtx, payload, false, userActor)
			if err == nil {
				t.Fatalf("key %q must be refused", key)
			}
			if key != "" && !strings.Contains(err.Error(), key) {
				t.Errorf("the refusal must name the key; got %q", err.Error())
			}
		}
	})

	t.Run("a 64-character key is the longest accepted", func(t *testing.T) {
		payload := map[string]any{"settings": map[string]any{"k" + strings.Repeat("0", 63): "v"}}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("a 64-character key is within the form; got %v", err)
		}
	})

	t.Run("a key ending in Ref is refused, and the refusal says where a ref lives", func(t *testing.T) {
		payload := map[string]any{"settings": map[string]any{"apiTokenRef": "some-secret-name"}}
		err := g.validateSiteSettings(userCtx, payload, false, userActor)
		if err == nil {
			t.Fatal("a key ending in Ref must be refused")
		}
		if !strings.Contains(err.Error(), "apiTokenRef") || !strings.Contains(err.Error(), "binding") {
			t.Errorf("the refusal must name the key and point at the binding; got %q", err.Error())
		}
	})

	// ---- the plainness of a value ----

	t.Run("a non-string value is refused", func(t *testing.T) {
		for name, v := range map[string]any{
			"number": 42.0,
			"bool":   true,
			"object": map[string]any{"nested": "x"},
			"list":   []any{"a"},
			"null":   nil,
		} {
			payload := map[string]any{"settings": map[string]any{"key": v}}
			if err := g.validateSiteSettings(userCtx, payload, false, userActor); err == nil {
				t.Errorf("a %s value must be refused", name)
			}
		}
	})

	t.Run("settings that are not an object are refused", func(t *testing.T) {
		for name, v := range map[string]any{"string": "apiBase=x", "list": []any{"a"}, "number": 1.0} {
			payload := map[string]any{"settings": v}
			if err := g.validateSiteSettings(userCtx, payload, false, userActor); err == nil {
				t.Errorf("settings as a %s must be refused", name)
			}
		}
	})

	// ---- the two caps ----

	t.Run("an over-long value is refused at the default cap", func(t *testing.T) {
		ok := map[string]any{"settings": map[string]any{"key": strings.Repeat("v", defaultSiteSettingsMaxValueLength)}}
		if err := g.validateSiteSettings(userCtx, ok, false, userActor); err != nil {
			t.Fatalf("a value at the cap must pass; got %v", err)
		}
		over := map[string]any{"settings": map[string]any{"key": strings.Repeat("v", defaultSiteSettingsMaxValueLength+1)}}
		err := g.validateSiteSettings(userCtx, over, false, userActor)
		if err == nil {
			t.Fatal("a value past the cap must be refused")
		}
		if !strings.Contains(err.Error(), "MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH") {
			t.Errorf("the refusal must name the knob; got %q", err.Error())
		}
	})

	t.Run("too many keys are refused at the default cap", func(t *testing.T) {
		at := map[string]any{}
		for i := 0; i < defaultSiteSettingsMaxKeys; i++ {
			at["k"+strconv.Itoa(i)] = "v"
		}
		if len(at) != defaultSiteSettingsMaxKeys {
			t.Fatalf("fixture built %d keys, want %d", len(at), defaultSiteSettingsMaxKeys)
		}
		if err := g.validateSiteSettings(userCtx, map[string]any{"settings": at}, false, userActor); err != nil {
			t.Fatalf("a map at the cap must pass; got %v", err)
		}
		at["oneMore"] = "v"
		err := g.validateSiteSettings(userCtx, map[string]any{"settings": at}, false, userActor)
		if err == nil {
			t.Fatal("a map past the cap must be refused")
		}
		if !strings.Contains(err.Error(), "MEMQL_SITE_SETTINGS_MAX_KEYS") {
			t.Errorf("the refusal must name the knob; got %q", err.Error())
		}
	})

	t.Run("the caps come from the env registry", func(t *testing.T) {
		t.Setenv("MEMQL_SITE_SETTINGS_MAX_KEYS", "2")
		t.Setenv("MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH", "5")
		three := map[string]any{"settings": map[string]any{"a": "1", "b": "2", "c": "3"}}
		if err := g.validateSiteSettings(userCtx, three, false, userActor); err == nil {
			t.Error("three keys under a cap of two must be refused")
		}
		long := map[string]any{"settings": map[string]any{"a": "123456"}}
		if err := g.validateSiteSettings(userCtx, long, false, userActor); err == nil {
			t.Error("six characters under a cap of five must be refused")
		}
		fine := map[string]any{"settings": map[string]any{"a": "12345", "b": ""}}
		if err := g.validateSiteSettings(userCtx, fine, false, userActor); err != nil {
			t.Errorf("two keys of five characters must pass; got %v", err)
		}
	})

	t.Run("a malformed cap falls back to the default rather than to zero", func(t *testing.T) {
		// A zero cap would refuse every write, silently, on a typo in an
		// overlay. The default is the answer a knob nobody could parse gives.
		t.Setenv("MEMQL_SITE_SETTINGS_MAX_KEYS", "lots")
		t.Setenv("MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH", "-1")
		payload := map[string]any{"settings": map[string]any{"a": "1", "b": "2", "c": "3"}}
		if err := g.validateSiteSettings(userCtx, payload, false, userActor); err != nil {
			t.Fatalf("unparseable caps must fall back to the defaults; got %v", err)
		}
	})

	// ---- systemOwned rows refuse it like every other status-class write ----

	t.Run("a user cannot write settings onto a systemOwned row", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "settings": map[string]any{"a": "1"}}
		err := g.validateSiteSettings(userCtx, payload, true, userActor)
		if err == nil {
			t.Fatal("expected the write to be refused")
		}
		if !strings.Contains(err.Error(), "systemOwned") {
			t.Errorf("the refusal must say why; got %q", err.Error())
		}
	})

	t.Run("a cluster owner cannot either -- the row is the deployment's", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "settings": map[string]any{"a": "1"}}
		if err := g.validateSiteSettings(ownerCtx, payload, true, ownerActor); err == nil {
			t.Fatal("expected the write to be refused for a cluster owner too")
		}
	})

	t.Run("a raw write cannot smuggle systemOwned:false in the same delta", func(t *testing.T) {
		payload := map[string]any{"systemOwned": false, "settings": map[string]any{"a": "1"}}
		if err := g.validateSiteSettings(userCtx, payload, true, userActor); err == nil {
			t.Fatal("the guard must read the PRIOR flag, not the caller's claim")
		}
	})

	t.Run("a system actor may write settings onto a systemOwned row", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "settings": map[string]any{"a": "1"}}
		if err := g.validateSiteSettings(sysCtx, payload, true, sysActor); err != nil {
			t.Fatalf("the seed must keep working; got %v", err)
		}
	})

	t.Run("a systemOwned row with no settings in the delta passes", func(t *testing.T) {
		payload := map[string]any{"hostname": "portal.example.com", "status": "live"}
		if err := g.validateSiteSettings(userCtx, payload, true, userActor); err != nil {
			t.Fatalf("the exemption gates settings writes only; got %v", err)
		}
	})
}

// TestSiteSettingsKeyForm pins the key regex directly, since the guard's
// message is what a person reads and the regex is what decides.
func TestSiteSettingsKeyForm(t *testing.T) {
	for key, want := range map[string]bool{
		"apiBase":                     true,
		"A":                           true,
		"a_b_c9":                      true,
		"k" + strings.Repeat("9", 63): true,
		"":                            false,
		"9lives":                      false,
		"_lead":                       false,
		"has-dash":                    false,
		"has.dot":                     false,
		"k" + strings.Repeat("9", 64): false,
	} {
		if got := siteSettingsKeyForm.MatchString(key); got != want {
			t.Errorf("key %q: form match = %v, want %v", key, got, want)
		}
	}
}
