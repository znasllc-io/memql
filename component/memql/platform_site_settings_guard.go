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
// ships. A value put here is public by construction. The storefront binding
// already has the one convention for a value that must NOT be public: a field
// named `...Ref` that NAMES a v1:platform:globalSecret row, which the edge
// resolves at serve time for exactly one kind. A settings key ending in `Ref`
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
	settings, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf(
			"v1:platform:site: settings must be an object of string values -- a key a bundle reads as config.settings.<key> and the plain string it gets; got %T",
			raw,
		)
	}

	maxKeys, maxValueLength := siteSettingsCaps()
	if len(settings) > maxKeys {
		return fmt.Errorf(
			"v1:platform:site: %d settings is more than this cluster keeps per deployable (%d, MEMQL_SITE_SETTINGS_MAX_KEYS). The document the edge serves grows with every key, so the cap is on the row.",
			len(settings), maxKeys,
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
				"v1:platform:site: settings key %q is not a name a bundle can read -- a key is a letter followed by letters, digits or underscores, at most 64 characters (config.settings.<key>).",
				key,
			)
		}
		if strings.HasSuffix(key, siteSettingsRefSuffix) {
			return fmt.Errorf(
				"v1:platform:site: settings key %q ends in Ref, and a setting is never a reference -- the edge serves every value here to every visitor as typed, and resolves a named secret for exactly one field, the storefront binding's storefrontTokenRef on `binding`. A secret does not belong in settings under any name.",
				key,
			)
		}
		value, isString := settings[key].(string)
		if !isString {
			return fmt.Errorf(
				"v1:platform:site: settings key %q holds a %T, and a setting is a plain string -- the value the bundle reads as config.settings.%s is served as typed, never parsed.",
				key, settings[key], key,
			)
		}
		if n := len([]rune(value)); n > maxValueLength {
			return fmt.Errorf(
				"v1:platform:site: settings key %q holds %d characters, more than this cluster keeps per value (%d, MEMQL_SITE_SETTINGS_MAX_VALUE_LENGTH).",
				key, n, maxValueLength,
			)
		}
	}
	return nil
}
