package scorecard

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/proving/figure"
)

func prov() figure.Provenance {
	return figure.Provenance{Commit: "9e91625", Date: "2026-09-06", Tier: figure.TierCI, Runner: "ubuntu-latest"}
}

func measured(t *testing.T, m figure.Metric, v float64) figure.Figure {
	t.Helper()
	f, err := figure.Measured(m, []float64{v}, prov())
	if err != nil {
		t.Fatalf("Measured(%s): %v", m, err)
	}
	return f
}

func unmeasured(t *testing.T, m figure.Metric) figure.Figure {
	t.Helper()
	f, err := figure.Unmeasured(m, figure.ReasonNotMeasurableOnReplay, "", prov())
	if err != nil {
		t.Fatalf("Unmeasured(%s): %v", m, err)
	}
	return f
}

func card(t *testing.T, entries ...Entry) Scorecard {
	t.Helper()
	return Scorecard{
		Version:           CurrentVersion,
		Date:              "2026-09-06",
		Commit:            "9e91625",
		CorpusFingerprint: "abc123def4567890",
		Tiers: map[figure.Tier]TierState{
			figure.TierCI:   {LastRun: "2026-09-06", Armed: true, Note: "runs on every pull request"},
			figure.TierLive: {Armed: false, Note: "dispatched by hand; it has not been run"},
		},
		Entries: entries,
	}
}

func entry(t *testing.T, scenario string, fam figure.Family, arm figure.Arm, f figure.Figure) Entry {
	t.Helper()
	return Entry{Scenario: scenario, Family: fam, Arm: arm, Figure: f}
}

func TestValidateRefusesAScorecardThatCannotBePublished(t *testing.T) {
	good := card(t, entry(t, "durability.kill", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	if err := good.Validate(); err != nil {
		t.Fatalf("the fixture every case below breaks one field of does not validate: %v", err)
	}

	for _, tc := range []struct {
		name   string
		break_ func(*Scorecard)
		want   string
	}{
		{"no fingerprint", func(s *Scorecard) { s.CorpusFingerprint = "" }, "joined into a trend"},
		{"bad date", func(s *Scorecard) { s.Date = "6 Sept" }, "not YYYY-MM-DD"},
		{"no commit", func(s *Scorecard) { s.Commit = "" }, "no commit"},
		{"absent tier", func(s *Scorecard) { delete(s.Tiers, figure.TierLive) }, "must SAY so"},
		{"silent never-run tier", func(s *Scorecard) { s.Tiers[figure.TierLive] = TierState{} }, "no reason"},
		{"no entries", func(s *Scorecard) { s.Entries = nil }, "no entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := card(t, good.Entries...)
			tc.break_(&s)
			err := s.Validate()
			if err == nil {
				t.Fatal("Validate accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestATierThatHasNeverRunMustSayWhy(t *testing.T) {
	// The live lane ships disarmed (design P3), so "every live figure is
	// tierNotRun" is the normal state -- and it is only honest if the page
	// also says the lane is not armed and why.
	s := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	page := RenderPage(s)
	if !strings.Contains(page, "never") {
		t.Error("the page does not say the live tier has never run")
	}
	if !strings.Contains(page, "dispatched by hand") {
		t.Error("the page does not carry the tier's note, so a reader cannot tell why the figures are empty")
	}
}

func TestAFailedGovernancePropertyMustCarryItsDetail(t *testing.T) {
	s := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	s.Governance = []Property{{Name: "everyEffectHasAReceipt", Scenario: "d.k", Passed: false}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "red light with no next step") {
		t.Fatalf("Validate = %v, want a refusal", err)
	}
}

func TestTheRoundTripPreservesEverything(t *testing.T) {
	s := card(t,
		entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)),
		entry(t, "r.k", figure.FamilyReliability, figure.ArmBaseline, unmeasured(t, figure.MetricPassVariance)),
	)
	b, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := Unmarshal(b)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Entries) != 2 {
		t.Fatalf("round trip lost entries: %d", len(back.Entries))
	}
	// The one that matters: an unmeasured figure must survive as unmeasured,
	// not as a zero.
	for _, e := range back.Entries {
		if e.Figure.Metric == figure.MetricPassVariance {
			if e.Figure.IsMeasured() {
				t.Fatal("an unmeasured figure came back measured")
			}
			if e.Figure.Absent != figure.ReasonNotMeasurableOnReplay {
				t.Errorf("the reason was lost: %q", e.Figure.Absent)
			}
		}
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("the artifact has no trailing newline; a committed file without one diffs badly forever")
	}
}

func TestUnmarshalRefusesAnUnknownField(t *testing.T) {
	if _, err := Unmarshal([]byte(`{"version":1,"date":"2026-09-06","reslut":{}}`)); err == nil {
		t.Fatal("Unmarshal accepted an unknown field")
	}
}

func TestNewestOrdersByFilenameAndInsistsTheContentAgrees(t *testing.T) {
	mk := func(date string) []byte {
		s := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
		s.Date = date
		b, err := s.Marshal()
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return b
	}
	fsys := fstest.MapFS{
		"sc/2026-09-01.json": {Data: mk("2026-09-01")},
		"sc/2026-09-06.json": {Data: mk("2026-09-06")},
		"sc/2026-08-30.json": {Data: mk("2026-08-30")},
	}
	s, name, ok, err := Newest(fsys, "sc")
	if err != nil || !ok {
		t.Fatalf("Newest: %v ok=%v", err, ok)
	}
	if name != "2026-09-06.json" || s.Date != "2026-09-06" {
		t.Fatalf("Newest = %s / %s", name, s.Date)
	}

	// A name and a content that disagree make the ordering a guess.
	mismatched := fstest.MapFS{"sc/2026-09-06.json": {Data: mk("2026-01-01")}}
	if _, _, _, err := Newest(mismatched, "sc"); err == nil || !strings.Contains(err.Error(), "must agree") {
		t.Fatalf("Newest = %v, want a refusal", err)
	}
}

func TestNewestReportsAnEmptyDirectoryAsAStateAndNotAnError(t *testing.T) {
	// Before the first run there is no scorecard, and that must render as
	// "no scorecard yet" rather than as an error or, worse, as zeroes.
	_, _, ok, err := Newest(fstest.MapFS{"sc/.keep": {Data: []byte("")}}, "sc")
	if err != nil {
		t.Fatalf("Newest: %v", err)
	}
	if ok {
		t.Fatal("Newest reported a scorecard in an empty directory")
	}
}

// --- The page --------------------------------------------------------------

func TestTheGeneratedPageCarriesItsFrontMatterAndItsMarker(t *testing.T) {
	page := RenderPage(card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0))))
	if !strings.HasPrefix(page, "---\n") {
		t.Fatal("the page does not open with front matter; docs/public/** is gated on it")
	}
	for _, key := range []string{"title:", "audience: public", "status: stable", "area: overview", "sinceVersion:", "owner:"} {
		if !strings.Contains(page, key) {
			t.Errorf("front matter is missing %q", key)
		}
	}
	if !strings.Contains(page, GeneratedMarker) {
		t.Error("the page does not say it is generated; somebody will edit it")
	}
}

func TestEveryNumberOnThePageCarriesItsSpreadAndItsN(t *testing.T) {
	f, err := figure.Measured(figure.MetricRecoveryWallClock, []float64{100, 120, 140, 160, 900}, prov())
	if err != nil {
		t.Fatalf("Measured: %v", err)
	}
	page := RenderPage(card(t, entry(t, "rec.a", figure.FamilyRecovery, figure.ArmPlatform, f)))
	if !strings.Contains(page, "N=5") {
		t.Error("the page prints a figure without its N")
	}
	if strings.Contains(page, "| 140ms |") {
		t.Error("the page printed a bare median; every number goes through figure.Render")
	}
}

func TestAnUnmeasuredFigureRendersAsAnEmDashWithItsReason(t *testing.T) {
	page := RenderPage(card(t, entry(t, "rel.a", figure.FamilyReliability, figure.ArmPlatform, unmeasured(t, figure.MetricPassVariance))))
	if !strings.Contains(page, "not measurable on a replay") {
		t.Fatalf("the page does not carry the reason:\n%s", page)
	}
	if strings.Contains(page, "| 0 |") {
		t.Error("an unmeasured figure printed as a zero")
	}
}

func TestTheHonestyTableIsGeneratedFromTheRegistry(t *testing.T) {
	// Written down, it would drift: a metric added to a family would quietly
	// acquire a claim the table does not make.
	page := RenderPage(card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0))))
	for _, m := range figure.RegisteredMetrics() {
		if !strings.Contains(page, string(m)) {
			t.Errorf("the honesty table omits %s", m)
		}
	}
}

func TestAMetricOnlyOneArmProducedIsListedRatherThanOmitted(t *testing.T) {
	// A missing comparison row reads as "no difference", which is the one
	// thing it does not mean.
	page := RenderPage(card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0))))
	if !strings.Contains(page, "the baseline arm produced no figure") {
		t.Errorf("a one-armed metric was silently omitted from the comparison:\n%s", page)
	}
}

func TestThePageIsAPureFunctionOfTheScorecard(t *testing.T) {
	s := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	if RenderPage(s) != RenderPage(s) {
		t.Fatal("RenderPage is not deterministic, so --check could never be a diff")
	}
}

// --- The regression gate ---------------------------------------------------

func TestTheGateBlocksOnStructuralRegressionsAndReportsCostAndSpeed(t *testing.T) {
	before := card(t,
		entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)),
		entry(t, "sp.a", figure.FamilySpeed, figure.ArmPlatform, measured(t, figure.MetricWallClockPerGoal, 100)),
	)
	now := card(t,
		entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 1)),
		entry(t, "sp.a", figure.FamilySpeed, figure.ArmPlatform, measured(t, figure.MetricWallClockPerGoal, 400)),
	)
	g := Gate(before, now, true)
	if g.Passed() {
		t.Fatal("a duplicated side effect did not block")
	}
	if len(g.Blocking) != 1 || g.Blocking[0].Metric != figure.MetricDuplicatedEffects {
		t.Fatalf("blocking = %+v", g.Blocking)
	}
	if len(g.Reported) != 1 || g.Reported[0].Metric != figure.MetricWallClockPerGoal {
		t.Fatalf("a 4x speed regression was not merely reported: %+v", g.Reported)
	}
	out := g.Render()
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "never block") {
		t.Errorf("Render = %q", out)
	}
}

func TestTheGateBlocksWhenAMetricSTOPSBeingMeasured(t *testing.T) {
	// The check a naive gate skips. A suite that quietly stops measuring
	// something reports no regression forever, which is the most comfortable
	// possible way to be broken.
	before := card(t,
		entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)),
		entry(t, "d.r", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricResumeReExecuted, 0)),
	)
	now := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	g := Gate(before, now, true)
	if g.Passed() {
		t.Fatal("dropping a metric passed the gate")
	}
	if len(g.LostMetrics) != 1 {
		t.Fatalf("lost = %v", g.LostMetrics)
	}
	if !strings.Contains(g.Render(), "no regression forever") {
		t.Error("the failure does not explain why a lost metric is a blocking condition")
	}
}

func TestAFailedGovernancePropertyBlocksRegardlessOfHistory(t *testing.T) {
	now := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	now.Governance = []Property{{Name: "everyEffectHasAReceipt", Scenario: "d.k", Passed: false, Detail: "step post left no receipt"}}
	g := Gate(Scorecard{}, now, false)
	if g.Passed() {
		t.Fatal("a failed governance property passed the gate on a first run")
	}
	if !strings.Contains(g.Render(), "left no receipt") {
		t.Error("the detail did not reach the output")
	}
}

func TestTheFirstRunSaysThereWasNothingToCompareRatherThanPassingSilently(t *testing.T) {
	now := card(t, entry(t, "d.k", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)))
	g := Gate(Scorecard{}, now, false)
	if !g.Passed() {
		t.Fatal("the first run failed the gate")
	}
	if !strings.Contains(g.Render(), "nothing to compare") {
		t.Errorf("Render = %q; 'found nothing to compare' and 'found no regressions' are different states", g.Render())
	}
}

func TestImprovementsArePublishedBesideRegressions(t *testing.T) {
	// The epic's honesty rule names this: regressions are published WITH
	// improvements, which stops the output reading as a list of bad news.
	before := card(t, entry(t, "a.c", figure.FamilyAmortizedCost, figure.ArmPlatform, measured(t, figure.MetricProviderCalls, 5)))
	now := card(t, entry(t, "a.c", figure.FamilyAmortizedCost, figure.ArmPlatform, measured(t, figure.MetricProviderCalls, 0)))
	g := Gate(before, now, true)
	if len(g.Improvements) != 1 {
		t.Fatalf("improvements = %+v", g.Improvements)
	}
	if !strings.Contains(g.Render(), "IMPROVED") {
		t.Error("the improvement was not printed")
	}
}

func TestAnUndecidableComparisonIsReportedRatherThanDropped(t *testing.T) {
	// A comparison silently skipped is indistinguishable from one that passed.
	before := card(t, entry(t, "rel.a", figure.FamilyReliability, figure.ArmPlatform, unmeasured(t, figure.MetricPassVariance)))
	now := card(t, entry(t, "rel.a", figure.FamilyReliability, figure.ArmPlatform, measured(t, figure.MetricPassVariance, 0.1)))
	g := Gate(before, now, true)
	if len(g.Undecidable) != 1 {
		t.Fatalf("undecidable = %+v", g.Undecidable)
	}
	if !strings.Contains(g.Render(), "UNDECIDABLE") {
		t.Error("the undecidable comparison was dropped")
	}
}

// --- The claims gate -------------------------------------------------------

func TestAMarkedClaimIsCheckedAgainstTheScorecard(t *testing.T) {
	doc := "Replaying a run re-executes no completed step.\n" +
		"<!-- proving: metric=durability.resumedStepsReExecuted arm=platform value=0 -->\n"
	claims, errs := ParseClaims("README.md", doc)
	if len(errs) != 0 {
		t.Fatalf("ParseClaims: %v", errs)
	}
	if len(claims) != 1 {
		t.Fatalf("parsed %d claims", len(claims))
	}
	if claims[0].Prose != "Replaying a run re-executes no completed step." {
		t.Errorf("prose = %q; the failure message quotes the sentence the number is in", claims[0].Prose)
	}

	good := card(t, entry(t, "d.r", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricResumeReExecuted, 0)))
	if f := CheckClaims(claims, good, true); len(f) != 0 {
		t.Fatalf("a true claim failed: %v", f)
	}

	bad := card(t, entry(t, "d.r", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricResumeReExecuted, 2)))
	f := CheckClaims(claims, bad, true)
	if len(f) != 1 {
		t.Fatalf("a claim that outlived its number passed: %v", f)
	}
	if !strings.Contains(f[0].Error(), "measured 2") {
		t.Errorf("the failure does not name the measured value: %s", f[0].Error())
	}
	if !strings.Contains(f[0].Error(), "Replaying a run") {
		t.Errorf("the failure does not quote the prose: %s", f[0].Error())
	}
}

func TestAClaimRestingOnAnUnmeasuredFigureFails(t *testing.T) {
	claims, _ := ParseClaims("README.md", "Runs are fast.\n<!-- proving: metric=speed.wallClockPerGoalMs arm=platform value=100 -->\n")
	s := card(t, entry(t, "sp.a", figure.FamilySpeed, figure.ArmPlatform, unmeasured(t, figure.MetricWallClockPerGoal)))
	f := CheckClaims(claims, s, true)
	if len(f) != 1 || !strings.Contains(f[0].Error(), "rests on an absence") {
		t.Fatalf("failures = %v", f)
	}
}

func TestAClaimWithNoScorecardAtAllFails(t *testing.T) {
	// "There is no scorecard yet" is not an excuse for publishing a number.
	claims, _ := ParseClaims("README.md", "x\n<!-- proving: metric=durability.duplicatedSideEffects arm=platform value=0 -->\n")
	f := CheckClaims(claims, Scorecard{}, false)
	if len(f) != 1 || !strings.Contains(f[0].Error(), "rests on nothing") {
		t.Fatalf("failures = %v", f)
	}
}

func TestEveryScenarioMeasuringAMetricMustSupportTheClaim(t *testing.T) {
	// Publishing a number that holds for one scenario and not another is how
	// a true sentence becomes a misleading one.
	claims, _ := ParseClaims("README.md", "No duplicates.\n<!-- proving: metric=durability.duplicatedSideEffects arm=platform value=0 -->\n")
	s := card(t,
		entry(t, "d.a", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0)),
		entry(t, "d.b", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 1)),
	)
	f := CheckClaims(claims, s, true)
	if len(f) != 1 || !strings.Contains(f[0].Error(), "d.b") {
		t.Fatalf("failures = %v; the scenario that disagrees must be named", f)
	}
}

func TestAMalformedMarkerIsAnErrorRatherThanASkip(t *testing.T) {
	// A marker nobody parses is a claim nobody checks, and it looks exactly
	// like a claim that passed.
	for _, tc := range []struct{ doc, want string }{
		{"<!-- proving: arm=platform value=0 -->", "names no metric"},
		{"<!-- proving: metric=durability.duplicatedSideEffects arm=platform -->", "names no value"},
		{"<!-- proving: metric=x arm=sideways value=0 -->", "not platform or baseline"},
		{"<!-- proving: metric=x value=0 op=roughly -->", "not eq, lte or gte"},
		{"<!-- proving: metric=x value=nope -->", "not a number"},
		{"<!-- proving: metirc=x value=0 -->", "unknown marker key"},
	} {
		_, errs := ParseClaims("f.md", tc.doc)
		if len(errs) == 0 {
			t.Errorf("%q produced no error", tc.doc)
			continue
		}
		joined := ""
		for _, e := range errs {
			joined += e.Error() + "\n"
		}
		if !strings.Contains(joined, tc.want) {
			t.Errorf("%q: errors = %s, want %q", tc.doc, joined, tc.want)
		}
	}
}

func TestAnLteClaimAllowsABetterNumber(t *testing.T) {
	claims, _ := ParseClaims("f.md", "Under 5% overhead.\n<!-- proving: metric=speed.journalPerStepOverheadRatio arm=platform op=lte value=0.05 -->\n")
	s := card(t, entry(t, "sp.j", figure.FamilySpeed, figure.ArmPlatform, measured(t, figure.MetricJournalOverhead, 0.031)))
	if f := CheckClaims(claims, s, true); len(f) != 0 {
		t.Fatalf("3.1%% failed a claim of at most 5%%: %v", f)
	}
	worse := card(t, entry(t, "sp.j", figure.FamilySpeed, figure.ArmPlatform, measured(t, figure.MetricJournalOverhead, 0.09)))
	if f := CheckClaims(claims, worse, true); len(f) != 1 {
		t.Fatalf("9%% passed a claim of at most 5%%: %v", f)
	}
}

func TestAClaimOfZeroMustBeExactlyZero(t *testing.T) {
	// The durability family's headline. A relative tolerance around zero is
	// zero, and this asserts that the arithmetic actually does that.
	claims, _ := ParseClaims("f.md", "None.\n<!-- proving: metric=durability.duplicatedSideEffects arm=platform value=0 -->\n")
	s := card(t, entry(t, "d.a", figure.FamilyDurability, figure.ArmPlatform, measured(t, figure.MetricDuplicatedEffects, 0.0001)))
	if f := CheckClaims(claims, s, true); len(f) != 1 {
		t.Fatalf("a near-zero satisfied a claim of exactly zero: %v", f)
	}
}

func TestStillPendingCatchesAClaimThatHasBecomeTrue(t *testing.T) {
	// The mirror half: a claim that is proven and still listed as unproven is
	// a different kind of stale, and the value of that table is that it is
	// true.
	pending, _ := ParseClaims("why.md", "| zero calls | ... |\n<!-- proving: metric=amortizedCost.providerCalls arm=platform value=0 -->\n")
	s := card(t, entry(t, "a.c", figure.FamilyAmortizedCost, figure.ArmPlatform, measured(t, figure.MetricProviderCalls, 0)))
	f := StillPending(pending, s, true)
	if len(f) != 1 || !strings.Contains(f[0].Error(), "Promote the claim") {
		t.Fatalf("failures = %v", f)
	}
	// While it is genuinely unmeasured, the table is correct and silent.
	unm := card(t, entry(t, "a.c", figure.FamilyAmortizedCost, figure.ArmPlatform, unmeasured(t, figure.MetricProviderCalls)))
	if f := StillPending(pending, unm, true); len(f) != 0 {
		t.Fatalf("a correctly-pending claim was flagged: %v", f)
	}
}
