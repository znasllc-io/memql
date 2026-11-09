package router

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	routerlib "github.com/znasllc-io/memql/component/router"
)

// fakeRender stands in for component/routingrules.GenerateRule, which this
// module cannot import (it requires the root module at a published version,
// and the renderer is newer than any). It renders DETERMINISTICALLY -- keys
// sorted -- because the real one does, and because the hash is only safe if a
// re-render of an unchanged rule is byte-identical.
func fakeRender(f routerlib.Form) (string, error) {
	var b strings.Builder
	b.WriteString("@when(")
	keys := make([]string, 0, len(f.When))
	for k := range f.When {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k + "=\"" + f.When[k] + "\"")
	}
	b.WriteString(")\n")
	for _, ex := range f.Excludes {
		b.WriteString("@exclude(\"" + ex + "\")\n")
	}
	b.WriteString("@policy(\"" + f.Policy + "\")\n")
	b.WriteString("rule " + f.Name + " { }\n")
	return b.String(), nil
}

func window(calls, failures int) routerlib.Window {
	return routerlib.Window{
		ModelId:            "qwen3.5:9b",
		Level:              "strong",
		Week:               "2026-W36",
		Calls:              calls,
		StructuredFailures: failures,
	}
}

func TestNoProposalUnderTheCallFloor(t *testing.T) {
	// Nineteen calls all failing is not evidence; it is a bad afternoon. The
	// ROW still carries the count -- that is the concept's job -- so an operator
	// asking why nothing was proposed reads the answer rather than inferring it.
	_, ok, err := proposeExclusion(window(19, 19), fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("%d calls is under the %d-call floor and must propose nothing", 19, 20)
	}
}

func TestNoProposalAtExactlyTheThreshold(t *testing.T) {
	// 0.3 is not "exceeds 0.3". A boundary decided by nothing is a boundary two
	// readers can disagree about, and this one decides whether a person is asked
	// to demote a model.
	_, ok, err := proposeExclusion(window(100, 30), fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a failure rate exactly at the threshold must not propose; the rule is EXCEEDS")
	}
	_, ok, err = proposeExclusion(window(100, 31), fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a failure rate above the threshold must propose")
	}
}

func TestAWindowWithNoLevelProposesNothing(t *testing.T) {
	// THE CASE THIS BRANCH ACTUALLY HITS, and the reason it is a refusal rather
	// than a fallback. A call that did not say which level it was serving cannot
	// support a demotion AT a level; demoting at every level instead would be a
	// far heavier act than the evidence supports, and it would be taken on the
	// strength of a missing field.
	w := window(100, 90)
	w.Level = ""
	_, ok, err := proposeExclusion(w, fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a window with no level must propose nothing")
	}
}

func TestAWindowWithAnUnknownLevelProposesNothing(t *testing.T) {
	// A level outside the closed four would render a rule the grammar refuses
	// to load -- so the proposal would be approved and then fail to arm, which
	// is the worst of both.
	w := window(100, 90)
	w.Level = "colossal"
	_, ok, _ := proposeExclusion(w, fakeRender)
	if ok {
		t.Fatal("a level outside the closed set must propose nothing")
	}
}

func TestTheProposalCarriesTheNumbers(t *testing.T) {
	// "This model is failing" is not a decidable claim. "17 of 41 structured
	// calls failed last week" is, and the person being asked to demote a model
	// is entitled to the second one.
	p, ok, err := proposeExclusion(window(41, 17), fakeRender)
	if err != nil || !ok {
		t.Fatalf("expected a proposal: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"17", "41", "qwen3.5:9b", "strong", "2026-W36"} {
		if !strings.Contains(p.Reason, want) {
			t.Fatalf("the reason must carry %q, got %q", want, p.Reason)
		}
	}
}

func TestTheHashIsOverTheRenderedSourceAndNotTheForm(t *testing.T) {
	// An approval is a decision about one specific rule TEXT. Hashing the inputs
	// would let a renderer change move the text under an approval that still
	// verified, which is the one failure the hash exists to prevent and the one
	// nobody would notice.
	p, _, err := proposeExclusion(window(100, 90), fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	if p.Hash != routerlib.ProposalHash(p.RuleSource) {
		t.Fatal("the hash must be over the rendered source")
	}
	if p.Hash == routerlib.ProposalHash(p.RuleSource+" ") {
		t.Fatal("the hash must change when the source does, to a single space")
	}
}

func TestTheProposalIsDeterministic(t *testing.T) {
	// Same evidence, byte-identical rule, identical hash -- on every run. A
	// nondeterministic renderer voids the approval's whole guarantee, and this
	// is the assertion that would catch one arriving through map iteration.
	first, _, err := proposeExclusion(window(100, 90), fakeRender)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, _, err := proposeExclusion(window(100, 90), fakeRender)
		if err != nil {
			t.Fatal(err)
		}
		if again.RuleSource != first.RuleSource || again.Hash != first.Hash {
			t.Fatalf("run %d differed:\n%q\n%q", i, again.RuleSource, first.RuleSource)
		}
	}
}

func TestTheWhenBlockCarriesOnlyTheKeysThatAreSet(t *testing.T) {
	// A key PRESENT with an empty value is a condition matching only an empty
	// value; a key ABSENT is no condition at all. Emitting every key with "" for
	// the blanks produces a rule that silently never fires -- which here would
	// be a demotion a person approved and that did nothing.
	var form routerlib.Form
	_, _, _, err := proposeEvidence(context.Background(), window(100, 90), "demotion", 0, false, func(f routerlib.Form) (string, error) { form = f; return fakeRender(f) }, noRuleBeatsIt)
	if err != nil {
		t.Fatal(err)
	}
	if len(form.When) != 1 {
		t.Fatalf("the when block must carry only `level`, got %#v", form.When)
	}
	if form.When["level"] != "strong" {
		t.Fatalf("got %#v", form.When)
	}
	for _, key := range []string{"modality", "prompt", "role", "actorRole", "tag", "touches"} {
		if _, present := form.When[key]; present {
			t.Fatalf("an unset condition key must be ABSENT, not empty: %q", key)
		}
	}
}

func TestARendererIsRequired(t *testing.T) {
	// There is exactly one renderer of the rule grammar in the tree, and this
	// package must not become a second. A missing one is an error rather than a
	// local fallback.
	_, _, err := proposeExclusion(window(100, 90), nil)
	if err == nil {
		t.Fatal("a proposal with no renderer must be an error, never a locally-rendered rule")
	}
}

func TestARenderFailureIsAnErrorAndNotASilentSkip(t *testing.T) {
	// A rule that could not be rendered is a proposal that could not be made,
	// and it must be loud: the alternative is a fold that quietly stops
	// proposing and looks exactly like a fleet where every model behaves.
	boom := func(routerlib.Form) (string, error) { return "", errors.New("grammar refused") }
	_, ok, err := proposeExclusion(window(100, 90), boom)
	if ok {
		t.Fatal("a failed render must not produce a proposal")
	}
	if err == nil {
		t.Fatal("a failed render must be reported")
	}
}

func TestPromotionReadsAMeasurementAndNotAWeekOfCalls(t *testing.T) {
	// The asymmetry is forced rather than chosen: an excluded model serves no
	// calls, so there is no service evidence to fold, and the probe is the only
	// thing that can speak for it.
	w := window(0, 0)
	if _, ok, _ := proposePromotion(w, 0.5, true, fakeRender); ok {
		t.Fatal("a measurement under the bar must not propose a promotion")
	}
	if _, ok, _ := proposePromotion(w, 0.95, false, fakeRender); ok {
		t.Fatal("no measurement at all must not propose a promotion")
	}
	p, ok, err := proposePromotion(w, 0.95, true, fakeRender)
	if err != nil || !ok {
		t.Fatalf("a measurement at or above the bar must propose: ok=%v err=%v", ok, err)
	}
	if p.Direction != routerlib.DirectionPromotion {
		t.Fatalf("direction %q", p.Direction)
	}
	if len(p.RuleSource) == 0 || strings.Contains(p.RuleSource, "@exclude") {
		t.Fatalf("a promotion is the same rule with nothing excluded, got %q", p.RuleSource)
	}
}

func TestARuleNameIsDerivedSoTwoFoldsAgree(t *testing.T) {
	// Derived rather than random, so a second fold of the same evidence produces
	// the same name and the authoring pipeline treats it as the same rule rather
	// than as a second one beside it.
	a := routerlib.RuleName(routerlib.DirectionDemotion, "qwen3.5:9b", "strong")
	b := routerlib.RuleName(routerlib.DirectionDemotion, "qwen3.5:9b", "strong")
	if a != b {
		t.Fatalf("%q vs %q", a, b)
	}
	if strings.ContainsAny(a, ":/.") {
		t.Fatalf("a rule name must be an identifier, got %q", a)
	}
	if a == routerlib.RuleName(routerlib.DirectionDemotion, "qwen3.5:9b", "fast") {
		t.Fatal("two levels must produce two rule names")
	}
	// A saved rule's name is lowerCamelCase letters and digits: routingRuleSave
	// refuses anything else, and so do MemQL OS's rule forms. An approved
	// demotion is saved through the same checks, so a name of any other shape
	// is a proposal nobody could ever apply.
	if !regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`).MatchString(a) {
		t.Fatalf("a rule name must be lowerCamelCase letters and digits, got %q", a)
	}
	// Two model ids with the same letters and digits are still two models, and
	// one name for both would let approving one replace the other's demotion.
	if a == routerlib.RuleName(routerlib.DirectionDemotion, "qwen35:9b", "strong") {
		t.Fatal("two model ids must produce two rule names")
	}
}

func TestSortWindowsIsStable(t *testing.T) {
	ws := []routerlib.Window{
		{ModelId: "b", Level: "reasoning"},
		{ModelId: "a", Level: "strong"},
		{ModelId: "a", Level: "fast"},
	}
	routerlib.SortWindows(ws)
	if ws[0].ModelId != "a" || ws[0].Level != "fast" {
		t.Fatalf("got %+v", ws)
	}
	if ws[1].ModelId != "a" || ws[1].Level != "strong" {
		t.Fatalf("got %+v", ws)
	}
}

func TestFailureRateOfAnEmptyWindowIsZeroAndTheFloorIsWhatMakesThatSafe(t *testing.T) {
	// A rate over no calls is not a rate. Returning zero is only safe because
	// MinimumCalls gates every caller -- so the assertion is the pairing, not
	// the number.
	if r := (routerlib.Window{}).FailureRate(); r != 0 {
		t.Fatalf("rate %v", r)
	}
	if _, ok, _ := proposeExclusion(routerlib.Window{ModelId: "m", Level: "fast", Week: "2026-W36"}, fakeRender); ok {
		t.Fatal("an empty window must be stopped by the floor, not by the rate")
	}
}

// noRuleBeatsIt is the checker for a fold whose demotions can all decide a
// call -- the case every test above is about.
func noRuleBeatsIt(context.Context, string) (string, error) { return "", nil }

func proposeExclusion(w routerlib.Window, render routerlib.Renderer) (routerlib.Proposal, bool, error) {
	p, _, ok, err := proposeEvidence(context.Background(), w, "demotion", 0, false, render, noRuleBeatsIt)
	return p, ok, err
}
func proposePromotion(w routerlib.Window, validity float64, measured bool, render routerlib.Renderer) (routerlib.Proposal, bool, error) {
	p, _, ok, err := proposeEvidence(context.Background(), w, "promotion", validity, measured, render, noRuleBeatsIt)
	return p, ok, err
}

func TestABeatenDemotionIsWithheld(t *testing.T) {
	// fastLane is locked and matches every fast call, so a demotion saved at
	// fast would decide nothing. Asking a person to approve it would ask them
	// to approve a change that changes no routing (design record D3).
	var asked string
	p, withheld, ok, err := proposeEvidence(context.Background(), window(100, 90), "demotion", 0, false, fakeRender,
		func(_ context.Context, source string) (string, error) { asked = source; return "fastLane", nil })
	if err != nil {
		t.Fatal(err)
	}
	if ok || withheld != "fastLane" {
		t.Fatalf("a beaten demotion must be withheld naming the rule that beats it: ok=%v withheld=%q", ok, withheld)
	}
	if p.RuleSource != "" || p.Hash != "" {
		t.Fatalf("a withheld demotion must open nothing, got %+v", p)
	}
	if !strings.Contains(asked, "rule "+routerlib.RuleName(routerlib.DirectionDemotion, "qwen3.5:9b", "strong")) {
		t.Fatalf("the checker must be asked about the rendered rule, got %q", asked)
	}
}

func TestADemotionNeedsAChecker(t *testing.T) {
	// No checker is not "nothing beats it": the fold must not propose a rule
	// nobody checked, any more than one nobody rendered.
	_, _, ok, err := proposeEvidence(context.Background(), window(100, 90), "demotion", 0, false, fakeRender, nil)
	if ok || err == nil {
		t.Fatalf("a demotion with no checker must be an error: ok=%v err=%v", ok, err)
	}
}

func TestACheckerFailureIsAnErrorAndNotASilentSkip(t *testing.T) {
	_, _, ok, err := proposeEvidence(context.Background(), window(100, 90), "demotion", 0, false, fakeRender,
		func(context.Context, string) (string, error) { return "", errors.New("configuration unreadable") })
	if ok || err == nil {
		t.Fatalf("a failed check must be reported: ok=%v err=%v", ok, err)
	}
}

func TestAPromotionIsNotChecked(t *testing.T) {
	// A promotion retires the demotion it lifts; its own text is never saved,
	// so whether that text could decide a call is not a question.
	_, _, ok, err := proposeEvidence(context.Background(), window(0, 0), "promotion", 0.95, true, fakeRender,
		func(context.Context, string) (string, error) {
			t.Fatal("a promotion must not be checked")
			return "", nil
		})
	if err != nil || !ok {
		t.Fatalf("a measured promotion must still propose: ok=%v err=%v", ok, err)
	}
}

// engineWithoutTheCheck is an engine that predates RoutingRuleBeatenBy.
type engineWithoutTheCheck struct{ memql.IntegrationEngineAccess }

func TestTheFoldRefusesAnEngineThatCannotCheck(t *testing.T) {
	i := &Integration{engine: engineWithoutTheCheck{}}
	if _, err := i.ruleBeatenBy(context.Background(), "rule x { }"); err == nil {
		t.Fatal("an engine without the check must refuse, so the fold proposes nothing it could not check")
	}
}
