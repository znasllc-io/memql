package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// routing_review_apply_test.go -- the nightly routing review's half of the
// routing configuration (design record
// docs/superpowers/specs/2026-11-08-routing-review-apply-design.md).
//
// The fixture is the shape the router actually walks: locked shipped rules
// first, saved rules after them by precedence, `default` last. The three
// locked rules are the ones that decide what a demotion can reach: `fastLane`
// matches every fast call, `backgroundLane` states a tag a demotion never
// states, and `default` is the floor that sorts after everything.

func evidenceFixture(t *testing.T) (*MemQLEngine, *testPolicyStore, *RuleRegistry) {
	t.Helper()
	store := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	base := NewRuleRegistry()
	require.NoError(t, base.Register(&RuleConfig{Name: "default", Policy: "localFirst", Precedence: 0, Locked: true}))
	require.NoError(t, base.Register(&RuleConfig{Name: "backgroundLane", Policy: "localFirst", Precedence: 40, Locked: true,
		When: RuleWhen{Tag: "background", Present: map[string]bool{"tag": true}}}))
	require.NoError(t, base.Register(&RuleConfig{Name: "fastLane", Policy: "localFirst", Precedence: 45, Locked: true,
		When: RuleWhen{Level: "fast", Present: map[string]bool{"level": true}}}))
	require.NoError(t, base.Finalize())
	return &MemQLEngine{policies: policyFixture(store), rules: base}, store, base
}

// evidenceRule is the text component/routingrules.GenerateRule renders for a
// demotion, line for line: doc comment, condition, level, policy, precedence,
// on-unavailable, exclusion, declaration.
func evidenceRule(name, level, model string) string {
	return "/// Generated from 2026-W45 evidence: 17 of 41 structured calls to " + model + " at level " + level + " failed.\n" +
		"@when(level=\"" + level + "\")\n" +
		"@level(\"" + level + "\")\n" +
		"@policy(\"localFirst\")\n" +
		"@precedence(50)\n" +
		"@onUnavailable(\"degrade\")\n" +
		"@exclude(\"fleet:" + model + "\")\n" +
		"rule " + name + " { }\n"
}

func TestRuleBeatenByNamesTheLockedLevelRule(t *testing.T) {
	eng, _, _ := evidenceFixture(t)
	got, err := eng.RoutingRuleBeatenBy(policyOwner(), evidenceRule("evidenceDemotionFastM", "fast", "m"))
	require.NoError(t, err)
	require.Equal(t, "fastLane", got, "fastLane is locked and matches every fast call, so a fast demotion never decides one")
}

func TestNothingBeatsAStrongDemotion(t *testing.T) {
	eng, _, _ := evidenceFixture(t)
	ctx := policyOwner()
	source := evidenceRule("evidenceDemotionStrongM", "strong", "m")
	got, err := eng.RoutingRuleBeatenBy(ctx, source)
	require.NoError(t, err)
	require.Equal(t, "", got, "backgroundLane states a tag the demotion does not, so it does not take every strong call")

	// The rule's own saved copy is not a rival, even at a higher precedence.
	_, err = eng.routingRuleSaveBuiltin(ctx, map[string]any{
		"name": "evidenceDemotionStrongM", "policy": "localFirst", "precedence": 60, "expectedRevision": 0,
		"conditions": map[string]any{"level": "strong"}, "excludes": []any{"fleet:m"},
	}, 0)
	require.NoError(t, err)
	got, err = eng.RoutingRuleBeatenBy(ctx, source)
	require.NoError(t, err)
	require.Equal(t, "", got)
}

func TestAHigherCustomCatchAllBeatsIt(t *testing.T) {
	eng, _, _ := evidenceFixture(t)
	ctx := policyOwner()
	save := func(name string, precedence int, revision int64) {
		t.Helper()
		_, err := eng.routingRuleSaveBuiltin(ctx, map[string]any{
			"name": name, "policy": "localOnly", "precedence": precedence, "expectedRevision": revision,
		}, 0)
		require.NoError(t, err)
	}
	source := evidenceRule("evidenceDemotionStrongM", "strong", "m")

	// A catch-all BELOW the demotion is walked after it, so it beats nothing.
	save("vendorFloor", 10, 0)
	got, err := eng.RoutingRuleBeatenBy(ctx, source)
	require.NoError(t, err)
	require.Equal(t, "", got)

	// A catch-all ABOVE it takes every call first.
	save("alwaysVendor", 500, 1)
	got, err = eng.RoutingRuleBeatenBy(ctx, source)
	require.NoError(t, err)
	require.Equal(t, "alwaysVendor", got)
}

func TestRoutingRuleBeatenByRefusesTextThatIsNotARule(t *testing.T) {
	eng, _, _ := evidenceFixture(t)
	_, err := eng.RoutingRuleBeatenBy(policyOwner(), "policy notARule { primary \"fleet:strongest\" }")
	require.Error(t, err)
}

func TestRuleCoversIsConservative(t *testing.T) {
	when := func(pairs ...string) RuleWhen {
		w := RuleWhen{Present: map[string]bool{}}
		for i := 0; i+1 < len(pairs); i += 2 {
			w.Present[pairs[i]] = true
			switch pairs[i] {
			case "level":
				w.Level = pairs[i+1]
			case "prompt":
				w.Prompt = pairs[i+1]
			case "tag":
				w.Tag = pairs[i+1]
			case "touches":
				w.Touches = pairs[i+1]
			}
		}
		return w
	}
	cases := []struct {
		name          string
		earlier, rule RuleWhen
		want          bool
	}{
		{"no conditions take every call", when(), when("level", "strong"), true},
		{"the same level", when("level", "strong"), when("level", "strong"), true},
		{"a narrower rule is covered", when("level", "strong"), when("level", "strong", "tag", "x"), true},
		{"a condition the rule does not state", when("level", "strong", "tag", "x"), when("level", "strong"), false},
		{"another level", when("level", "fast"), when("level", "strong"), false},
		{"a touches prefix covers its extensions", when("touches", "v1:identity:"), when("touches", "v1:identity:user"), true},
		{"but not the reverse", when("touches", "v1:identity:user"), when("touches", "v1:identity:"), false},
		{"an empty prompt is a condition, not an absence", when("prompt", ""), when("level", "strong"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ruleCovers(&RuleConfig{Name: "earlier", When: tc.earlier}, &RuleConfig{Name: "rule", When: tc.rule})
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseApprovedRuleRefusesLockedAndDisabled(t *testing.T) {
	source := evidenceRule("evidenceDemotionStrongM", "strong", "m")
	cfg, err := parseApprovedRule(source)
	require.NoError(t, err)
	require.Equal(t, "evidenceDemotionStrongM", cfg.Name)
	require.Equal(t, 50, cfg.Precedence)
	require.Equal(t, []string{"fleet:m"}, cfg.Excludes)
	require.True(t, cfg.When.Has("level"))
	require.Equal(t, "strong", cfg.When.Level)
	require.Contains(t, cfg.Description, "17 of 41")

	_, err = parseApprovedRule(strings.Replace(source, "rule ", "@locked\nrule ", 1))
	require.ErrorContains(t, err, "@locked", "a locked rule belongs to the embedded tree alone")
	_, err = parseApprovedRule(strings.Replace(source, "rule ", "@disabled\nrule ", 1))
	require.ErrorContains(t, err, "@disabled", "a disabled rule decides nothing, so approving it would change nothing")
}
