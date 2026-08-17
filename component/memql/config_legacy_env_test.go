package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/envregistry"
)

// config_legacy_env_test.go -- znasllc-io/memql#3831.
//
// The pre-convention env vars were RENAMED to MEMQL_ names so they could be
// registered at all: the prefix lint refuses a non-MEMQL_ registry entry that is
// not a legacy alias, and the drift gate refuses an unregistered read, so there
// was no state of the registry in which both held.
//
// A rename is only NOT a breaking change because of the boot-time alias shim,
// and "the alias exists" is a weaker claim than "an operator's existing
// configuration still takes effect". genesis's own tests cover the shim
// generically against synthetic names; this covers the END of the path -- a
// value an operator set long ago arriving in the field it has always
// controlled.
//
// WHY IT IS WORTH A TEST. The path has two halves maintained in different
// packages: envregistry.LegacyAliases must name the pair, and this package's const
// must read the NEW name. Get either wrong and the knob stops working with
// NOTHING failing -- the loader falls back to its default, which is a perfectly
// plausible value, so a cluster silently reverts to a 500-row ceiling and looks
// fine. That is the same silence the whole issue is about, one layer out.
func TestLegacyEngineEnvNamesStillConfigure(t *testing.T) {
	cases := []struct {
		legacy string
		value  string
		field  string
		got    func(engineConfig) int64
		want   int64
	}{
		{
			legacy: "MEMORY_ENGINE_MAX_RESULTS",
			value:  "4321",
			field:  "MaxResults",
			got:    func(c engineConfig) int64 { return int64(c.MaxResults) },
			want:   4321,
		},
		{
			// MaxWindow is only accepted when it EXCEEDS MaxResults, so this
			// value has to clear the 500 default for the assertion to mean
			// anything -- a smaller one would be correctly ignored and the test
			// would be measuring the clamp, not the alias.
			legacy: "MEMORY_ENGINE_MAX_WINDOW",
			value:  "9876",
			field:  "MaxWindow",
			got:    func(c engineConfig) int64 { return int64(c.MaxWindow) },
			want:   9876,
		},
		{
			// Below the 500 MaxResults default, since DefaultListCap is clamped
			// down to it.
			legacy: "MEMORY_ENGINE_DEFAULT_LIST_CAP",
			value:  "37",
			field:  "DefaultListCap",
			got:    func(c engineConfig) int64 { return int64(c.DefaultListCap) },
			want:   37,
		},
		{
			legacy: "MEMORY_ENGINE_CACHE_MAX_ITEMS",
			value:  "999",
			field:  "CacheSize",
			got:    func(c engineConfig) int64 { return c.CacheSize },
			want:   999,
		},
		{
			legacy: "CACHE_MAX_TTL",
			value:  "123",
			field:  "CacheMaxTTLSeconds",
			got:    func(c engineConfig) int64 { return c.CacheMaxTTLSeconds },
			want:   123,
		},
	}

	for _, tc := range cases {
		t.Run(tc.legacy, func(t *testing.T) {
			// ONLY the legacy name is set -- exactly an operator who has not
			// touched their configuration since before the rename.
			t.Setenv(tc.legacy, tc.value)

			// The shim, called where main.go calls it: after the env layers are
			// painted, before any component reads its config.
			envregistry.ApplyLegacyEnvAliases(nil)

			if got := tc.got(loadEngineConfigFromEnv()); got != tc.want {
				t.Errorf("%s=%s did not reach %s: got %d, want %d.\n"+
					"The rename is non-breaking ONLY because the alias bridges it. If this "+
					"fails, an operator's existing configuration silently stopped taking "+
					"effect and the loader quietly used its default instead -- which is a "+
					"plausible value, so nothing would look broken (memql#3831).",
					tc.legacy, tc.value, tc.field, got, tc.want)
			}
		})
	}
}

// The NEW name must win when both are set, because that is what makes the
// alias a migration path rather than a trap: an operator who has adopted the
// new spelling should not be overridden by a stale legacy value still sitting
// in a sealed envelope or a shell profile they forgot about.
func TestNewEngineEnvNameBeatsItsLegacyAlias(t *testing.T) {
	t.Setenv("MEMORY_ENGINE_MAX_RESULTS", "111")
	t.Setenv("MEMQL_MEMORY_ENGINE_MAX_RESULTS", "222")

	envregistry.ApplyLegacyEnvAliases(nil)

	if got := loadEngineConfigFromEnv().MaxResults; got != 222 {
		t.Errorf("MaxResults = %d, want 222. A legacy value that overrides the new "+
			"name turns the migration path into a trap: adopting the new spelling "+
			"would appear to do nothing.", got)
	}
}
