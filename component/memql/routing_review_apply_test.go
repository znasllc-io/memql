package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
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

func developerCtx() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "dev-1", Role: auth.RoleDeveloper})
}

func TestApplySavesTheExactApprovedRuleUnderANewRevision(t *testing.T) {
	eng, store, base := evidenceFixture(t)
	got, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"),
	})
	require.NoError(t, err)
	require.Equal(t, RoutingReviewApplied{Rule: "evidenceDemotionStrongM", Revision: 1, Changed: true}, got)
	require.EqualValues(t, 1, store.doc.Revision)
	require.Equal(t, "dev-1", store.doc.UpdatedBy)
	saved := store.doc.Rules["evidenceDemotionStrongM"]
	require.Equal(t, []string{"fleet:m"}, saved.Excludes)
	require.Equal(t, 50, saved.Precedence)
	require.Equal(t, "strong", saved.When.Level)
	require.False(t, saved.Locked)

	// Another replica reads the same document on its next call.
	_, rules, err := policyFixture(store).SnapshotRouting(developerCtx(), base)
	require.NoError(t, err)
	_, ok := rules.Lookup("evidenceDemotionStrongM")
	require.True(t, ok)
}

func TestApplyIsANoOpWhenAlreadySaved(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	change := RoutingReviewChange{SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m")}
	_, err := eng.ApplyRoutingReview(developerCtx(), change)
	require.NoError(t, err)
	got, err := eng.ApplyRoutingReview(developerCtx(), change)
	require.NoError(t, err)
	require.Equal(t, RoutingReviewApplied{Rule: "evidenceDemotionStrongM", Revision: 1, Changed: false}, got)
	require.EqualValues(t, 1, store.doc.Revision, "a retried decide must not write a second revision")
}

func TestApplyRefusesAWriter(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	writer := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "w-1", Role: auth.RoleWriter})
	_, err := eng.ApplyRoutingReview(writer, RoutingReviewChange{
		SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"),
	})
	require.ErrorContains(t, err, "owner or developer")
	require.EqualValues(t, 0, store.doc.Revision)
	require.Empty(t, store.doc.Rules)
}

func TestApplyRefusesANameMismatch(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongOther", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"),
	})
	require.ErrorContains(t, err, "evidenceDemotionStrongOther")
	require.EqualValues(t, 0, store.doc.Revision)
}

func TestApplyRefusesABeatenRule(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionFastM", SaveSource: evidenceRule("evidenceDemotionFastM", "fast", "m"),
	})
	require.ErrorContains(t, err, `"fastLane"`)
	require.EqualValues(t, 0, store.doc.Revision)
	require.Empty(t, store.doc.Rules)
}

func TestApplyRefusesASecondDemotionAtTheLevel(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongA", SaveSource: evidenceRule("evidenceDemotionStrongA", "strong", "a"),
	})
	require.NoError(t, err)
	_, err = eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongB", SaveSource: evidenceRule("evidenceDemotionStrongB", "strong", "b"),
	})
	require.ErrorContains(t, err, `"evidenceDemotionStrongA"`)
	require.ErrorContains(t, err, "Fleet > Routing > Rules")
	require.EqualValues(t, 1, store.doc.Revision)
	require.Len(t, store.doc.Rules, 1)
}

func TestApplyRetiresTheDemotion(t *testing.T) {
	eng, store, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"),
	})
	require.NoError(t, err)

	got, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{RetireName: "evidenceDemotionStrongM"})
	require.NoError(t, err)
	require.Equal(t, RoutingReviewApplied{Rule: "evidenceDemotionStrongM", Revision: 2, Changed: true}, got)
	require.EqualValues(t, 2, store.doc.Revision)
	require.NotContains(t, store.doc.Rules, "evidenceDemotionStrongM")

	got, err = eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{RetireName: "evidenceDemotionStrongM"})
	require.NoError(t, err)
	require.Equal(t, RoutingReviewApplied{Rule: "evidenceDemotionStrongM", Revision: 2, Changed: false}, got)

	_, err = eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{RetireName: "fastLane"})
	require.ErrorContains(t, err, "fastLane")
	require.EqualValues(t, 2, store.doc.Revision)
}

func TestApplyRefusesAChangeThatIsBothOrNeither(t *testing.T) {
	eng, _, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{})
	require.Error(t, err)
	_, err = eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"), RetireName: "evidenceDemotionStrongM",
	})
	require.Error(t, err)
}

// movingStore is another writer landing between the apply's read and its
// swap: the first CompareAndSwap finds the revision moved under it.
type movingStore struct {
	*testPolicyStore
	moved bool
}

func (s *movingStore) CompareAndSwap(ctx context.Context, expected int64, doc PolicyDocument) error {
	if !s.moved {
		s.moved = true
		s.testPolicyStore.mu.Lock()
		s.testPolicyStore.doc.Revision++
		s.testPolicyStore.doc.Overrides["localOnly"] = PolicyConfig{Name: "localOnly", Primary: "app:codex"}
		s.testPolicyStore.mu.Unlock()
	}
	return s.testPolicyStore.CompareAndSwap(ctx, expected, doc)
}

func TestApplyRetriesOnceRoutingMoved(t *testing.T) {
	_, _, base := evidenceFixture(t)
	inner := &testPolicyStore{doc: PolicyDocument{Overrides: map[string]PolicyConfig{}}}
	store := &movingStore{testPolicyStore: inner}
	eng := &MemQLEngine{policies: policyFixture(store), rules: base}
	got, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotionStrongM", SaveSource: evidenceRule("evidenceDemotionStrongM", "strong", "m"),
	})
	require.NoError(t, err)
	require.Equal(t, RoutingReviewApplied{Rule: "evidenceDemotionStrongM", Revision: 2, Changed: true}, got)
	require.EqualValues(t, 2, inner.doc.Revision)
	require.Equal(t, "app:codex", inner.doc.Overrides["localOnly"].Primary, "the other writer's change must survive the retry")
	require.Contains(t, inner.doc.Rules, "evidenceDemotionStrongM")
}

func TestApplyRefusesAReviewRaisedUnderTheOldRuleName(t *testing.T) {
	// The fold named rules with underscores until 2026-11-08, a shape no saved
	// rule may take. A review raised then can never be applied, and the person
	// approving it must be told so -- and that declining is the way out --
	// rather than meet an identifier rule they never wrote.
	eng, store, _ := evidenceFixture(t)
	_, err := eng.ApplyRoutingReview(developerCtx(), RoutingReviewChange{
		SaveName: "evidenceDemotion_m_strong", SaveSource: evidenceRule("evidenceDemotion_m_strong", "strong", "m"),
	})
	require.ErrorContains(t, err, "Decline")
	require.ErrorContains(t, err, "evidenceDemotion_m_strong")
	require.EqualValues(t, 0, store.doc.Revision)
	require.Empty(t, store.doc.Rules)
}
