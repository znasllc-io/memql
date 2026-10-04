package memql

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/env"
)

// Runtime settings on v1:platform:site (epic memql#4906, decision P7 of the
// Deployables program): key-values a bundle reads at load, merged by the edge
// into the site's runtime-config document under `settings`, so one bundle can
// serve two deployables against different endpoints without a rebuild.
//
// The guard is Go rather than DSL for the reason every guard in this package
// is: a mutation body sees a value, not the KEYS of an object, and nothing in
// a filter grammar can say "every key is an identifier and every value is a
// short string". The mutation carries the shape half (`settings object!`);
// this is the half that decides.
//
// # Not a place for a secret, and the `Ref` refusal is how that is kept true
//
// The document these values land in is served to every visitor's browser,
// unauthenticated, cached nowhere and read by whatever JavaScript the bundle
// ships. A value put here is public by construction. A storefront's STORE ROW
// already has the one convention for a value that must NOT be public: a field
// named `...Ref` that NAMES a v1:platform:globalSecret row, which the edge
// resolves at serve time for exactly one kind. (The site's `binding` NAMES
// that row rather than carrying the reference itself, epic memql#5530, so the
// convention is one hop away from here now and no closer to belonging in
// `settings`.) A settings key ending in `Ref`
// would look like that convention and be honoured by nothing -- the edge
// serves the string as typed -- so the natural mistake ("apiTokenRef":
// "my-secret") would publish the secret's NAME, and the natural next mistake
// would be for someone to teach the edge to resolve it. Refusing the suffix
// closes both.
//
// # The caps come from the env registry
//
// MEMQL_SITE_SETTINGS_MAX_KEYS and MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH bound
// the row and therefore the document: the edge serves it on every page load,
// and a settings object a person could grow without limit is a document that
// grows with it. A cap a person cannot parse falls back to the default rather
// than to zero, because a zero cap refuses every write and says nothing about
// the typo in the overlay that caused it.
//
// # systemOwned rows refuse it
//
// The seeded portal and OS rows are the surfaces the cluster is managed
// through, re-seeded live at every boot. Settings on them would be the
// deployment's to set, and the deployment sets nothing here; a cluster owner's
// edit would be reverted by the next seed and look like it had worked until
// then. So the write is refused for every non-system actor, exactly as a
// status change is (validateSiteStatusTransition), reading the PRIOR row's
// flag for the same-delta reason that guard documents.

const (
	// defaultSiteSettingsMaxKeys is the MEMQL_SITE_SETTINGS_MAX_KEYS default.
	defaultSiteSettingsMaxKeys = 64
	// defaultSiteSettingsMaxValueLength is the
	// MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH default, in characters.
	defaultSiteSettingsMaxValueLength = 2048
	// siteSettingsRefSuffix is the suffix a key may not carry: it names the
	// storefront binding's secret-reference convention, which lives on
	// `binding` and is resolved for exactly one kind.
	siteSettingsRefSuffix = "Ref"
)

// siteSettingsKeyForm is the form of a settings key: an identifier a bundle
// reads as `config.settings.<key>` -- a letter, then letters, digits and
// underscores, at most 64 characters. Mirrored keystroke-rate in the OS; this
// is the copy that decides.
var siteSettingsKeyForm = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

// siteSettingsCaps reads the two caps from the environment, falling back to
// the defaults on an absent, unparseable or non-positive value.
func siteSettingsCaps() (maxKeys, maxValueLength int) {
	maxKeys, maxValueLength = defaultSiteSettingsMaxKeys, defaultSiteSettingsMaxValueLength
	reader := env.NewEnvReader("MEMQL_SITE_SETTINGS")
	if v, err := reader.OptionalInt("MAX_KEYS"); err == nil && v != nil && *v > 0 {
		maxKeys = *v
	}
	if v, err := reader.OptionalInt("MAX_VALUE_LENGTH"); err == nil && v != nil && *v > 0 {
		maxValueLength = *v
	}
	return maxKeys, maxValueLength
}

// validateSiteSettings refuses a `settings` delta that is not a flat object of
// well-formed keys over plain strings within the caps, and refuses any
// settings delta at all on a systemOwned row for a non-system actor.
//
// A write that names no `settings` key passes untouched: a publish, a rename
// and a status flip inherit the stored object through the read-merge and this
// guard has nothing to say about them. An explicit null or an empty object is
// the cleared state and passes for the same reason an empty tie does on
// updateSiteAccount -- clearing must be expressible.
func (e *MemQLEngine) validateSiteSettings(
	ctx context.Context,
	payload map[string]any,
	priorSystemOwned bool,
	actor string,
) error {
	if payload == nil {
		return nil
	}
	raw, present := payload["settings"]
	if !present {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if priorSystemOwned && !isSystemActor(identity, actor) {
		hostname := strings.TrimSpace(stringFromAny(payload["hostname"]))
		return fmt.Errorf(
			"v1:platform:site: %q is systemOwned and its runtime settings cannot be written -- it is one of the cluster's own surfaces, re-seeded at every boot, and a value set here would be reverted by the next seed. See dsl/platform/concepts.memql:site.systemOwned.",
			hostname,
		)
	}

	if raw == nil {
		return nil
	}
	return validateSiteSettingsObject(raw, "settings")
}

// validateSiteSettingsObject holds one flat settings object to the rules every
// value the runtime document carries obeys: an object, at most
// MEMQL_SITE_SETTINGS_MAX_KEYS keys of the identifier form and not ending in
// `Ref`, every value a plain string within MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH.
// `where` names the object in a refusal -- "settings", or one store's entry in
// storeSettings -- so the same rule reads the same way on both.
func validateSiteSettingsObject(raw any, where string) error {
	settings, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: %s must be an object of string values -- a key a bundle reads as config.settings.<key> and the plain string it gets; got %T",
			where, raw,
		)
	}

	maxKeys, maxValueLength := siteSettingsCaps()
	if len(settings) > maxKeys {
		return fmt.Errorf(
			"v1:platform:site: %d keys in %s is more than this cluster keeps per deployable (%d, MEMQL_SITE_SETTINGS_MAX_KEYS). The document the edge serves grows with every key, so the cap is on the row.",
			len(settings), where, maxKeys,
		)
	}

	// Sorted so a refusal names the same key on every attempt: a map walked
	// in iteration order would report a different first offender each time
	// and read as a moving target.
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if !siteSettingsKeyForm.MatchString(key) {
			return fmt.Errorf(
				"v1:platform:site: %s key %q is not a name a bundle can read -- a key is a letter followed by letters, digits or underscores, at most 64 characters (config.settings.<key>).",
				where, key,
			)
		}
		if strings.HasSuffix(key, siteSettingsRefSuffix) {
			return fmt.Errorf(
				"v1:platform:site: %s key %q ends in Ref, and a setting is never a reference -- the edge serves every value here to every visitor as typed, and resolves a named secret for exactly one field: the Storefront token's, on the v1:shopify:store row this site's `binding` names. A secret does not belong in settings under any name.",
				where, key,
			)
		}
		value, isString := settings[key].(string)
		if !isString {
			return fmt.Errorf(
				"v1:platform:site: %s key %q holds a %T, and a setting is a plain string -- the value the bundle reads as config.settings.%s is served as typed, never parsed.",
				where, key, settings[key], key,
			)
		}
		if n := len([]rune(value)); n > maxValueLength {
			return fmt.Errorf(
				"v1:platform:site: %s key %q holds %d characters, more than this cluster keeps per value (%d, MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH).",
				where, key, n, maxValueLength,
			)
		}
	}
	return nil
}

// siteStoreSettingsMaxStores bounds how many stores storeSettings may name.
// A storefront binds at most two stores at once -- Production's and
// Testing's -- and keeps an entry for a store it was bound to before, so that
// re-pointing a binding back restores that store's values; sixteen is far past
// any real population, and the document never carries more than one store's
// entry, so the bound is on the row rather than on what is served. A constant
// rather than an env cap because nothing about a cluster makes it want more.
const siteStoreSettingsMaxStores = 16

// siteStoreIdForm is the form of a storeSettings key: a store row's BARE id,
// as Connect writes it (acme-widgets) or as a hand-made store names it. No
// colon, so the canonical v1:shopify:store:<id> form is refused rather than
// kept beside the bare one -- one store is keyed one way, and the edge looks
// it up by the bare id its binding names.
var siteStoreIdForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// validateSiteStoreSettings refuses a `storeSettings` value that is not an
// object of bare store ids, each to a settings object `settings`' own rules
// admit (memql#5602), and refuses any storeSettings at all on a systemOwned
// row for a non-system actor -- validateSiteSettings' rules, for the values
// the edge merges over `settings` for one store.
//
// The VALUES are not checked against any store: an entry for a store this
// site is not bound to is never served, because the edge applies only the
// entry of the store the in-force binding names, and binding a store already
// requires reading it (platform_site_binding_guard.go).
func (e *MemQLEngine) validateSiteStoreSettings(
	ctx context.Context,
	payload map[string]any,
	priorSystemOwned bool,
	actor string,
) error {
	if payload == nil {
		return nil
	}
	raw, present := payload["storeSettings"]
	if !present {
		return nil
	}

	identity, _ := auth.UserIdentityFromContext(ctx)
	if priorSystemOwned && !isSystemActor(identity, actor) {
		hostname := strings.TrimSpace(stringFromAny(payload["hostname"]))
		return fmt.Errorf(
			"v1:platform:site: %q is systemOwned and its per-store settings cannot be written -- it is one of the cluster's own surfaces, re-seeded at every boot, and a value set here would be reverted by the next seed. See dsl/platform/concepts.memql:site.systemOwned.",
			hostname,
		)
	}

	if raw == nil {
		return nil
	}
	stores, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: storeSettings must be an object of store id to that store's settings, {\"<storeId>\": {key: \"value\"}}; got %T",
			raw,
		)
	}
	if len(stores) > siteStoreSettingsMaxStores {
		return fmt.Errorf(
			"v1:platform:site: storeSettings names %d stores, more than a deployable keeps (%d). A storefront is bound to at most two stores at once; remove the entries of stores it no longer uses.",
			len(stores), siteStoreSettingsMaxStores,
		)
	}

	ids := make([]string, 0, len(stores))
	for id := range stores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// ONE BUDGET FOR EVERY STORE TOGETHER: the characters one full `settings`
	// object can hold (MEMQL_SITE_SETTINGS_MAX_KEYS values of
	// MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH). Store by store, the caps alone
	// would let sixteen stores put two megabytes on every version of the row.
	maxKeys, maxValueLength := siteSettingsCaps()
	budget, total := maxKeys*maxValueLength, 0
	for _, id := range ids {
		if !siteStoreIdForm.MatchString(id) {
			return fmt.Errorf(
				"v1:platform:site: storeSettings key %q is not a store id -- name the store by its bare id, as the binding does (acme-widgets, never v1:shopify:store:acme-widgets): a letter or digit, then letters, digits, '.', '_' or '-', at most 128 characters.",
				id,
			)
		}
		if _, isObject := stores[id].(map[string]any); !isObject {
			return fmt.Errorf(
				"v1:platform:site: storeSettings[%q] must be an object of that store's settings; got %T",
				id, stores[id],
			)
		}
		if err := validateSiteSettingsObject(stores[id], fmt.Sprintf("storeSettings[%q]", id)); err != nil {
			return err
		}
		for _, v := range stores[id].(map[string]any) {
			total += len([]rune(stringFromAny(v)))
		}
	}
	if total > budget {
		return fmt.Errorf(
			"v1:platform:site: storeSettings hold %d characters of values across all stores, more than one deployable keeps (%d -- MEMQL_SITE_SETTINGS_MAX_KEYS values of MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH, the most `settings` itself can hold).",
			total, budget,
		)
	}
	return nil
}
