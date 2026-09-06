package proving

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/figure"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func loadCorpus(t *testing.T) scenario.Corpus {
	t.Helper()
	c, err := scenario.Load(os.DirFS(repoRoot(t)), "test/proving/scenarios")
	if err != nil {
		t.Fatalf("the committed corpus does not load:\n%v", err)
	}
	return c
}

func TestTheCommittedCorpusLoads(t *testing.T) {
	c := loadCorpus(t)
	// The epic scopes "twelve to twenty scenarios". The floor is what matters:
	// a corpus that shrank below it is one somebody deleted from, and an empty
	// corpus passes every gate while proving nothing.
	if len(c.Scenarios) < 12 {
		t.Fatalf("the corpus holds %d scenarios; the epic scopes twelve to twenty and a shrinking corpus is the quiet way this suite stops meaning anything", len(c.Scenarios))
	}
	if c.Fingerprint == "" {
		t.Error("the corpus has no fingerprint, so two bench runs could be joined into a trend without anyone noticing they measured different things")
	}
}

func TestEveryFamilyIsRepresented(t *testing.T) {
	// The epic names six families plus the governance properties. A family
	// with no scenario contributes nothing to the scorecard, and its absence
	// on the page reads as "measured and uninteresting" rather than "not
	// measured at all".
	c := loadCorpus(t)
	for _, f := range figure.AllFamilies() {
		if len(c.ByFamily(f)) == 0 {
			t.Errorf("no scenario in family %q; the scorecard would carry no figures for it and the page would not say why", f)
		}
	}
}

func TestEveryBlockingZeroClaimHasANegativeControl(t *testing.T) {
	// The most important structural rule in the corpus. A counter that is
	// never incremented on ANY path reads as zero forever, so a green suite
	// with a dead instrument is indistinguishable from a working one.
	if err := loadCorpus(t).CorpusControls(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryScenarioNeedingAModelHasACassetteOnBothArms(t *testing.T) {
	// The CI tier has NO PROVIDER, so both arms replay. A missing cassette is
	// a scenario that cannot run at all, and finding that out from a red lane
	// is worse than finding it out here.
	root := repoRoot(t)
	c := loadCorpus(t)
	set, err := cassette.Load(os.DirFS(root), "test/proving/cassettes")
	if err != nil {
		t.Fatalf("the committed cassettes do not load:\n%v", err)
	}
	for _, s := range c.Scenarios {
		if !NeedsCassette(s) {
			continue
		}
		for _, arm := range []figure.Arm{figure.ArmPlatform, figure.ArmBaseline} {
			if _, ok := set.For(s.Id, string(arm)); !ok {
				t.Errorf("%s has a reasoning step and no %s cassette; record one with `memql-bench --do=record --synthetic`", s.Id, arm)
			}
		}
	}
}

func TestNoCassetteExistsForAScenarioThatNeedsNone(t *testing.T) {
	// The mirror. A cassette left behind after its scenario stopped reasoning
	// is dead weight that reads, to the next person, as a scenario that calls
	// a model.
	root := repoRoot(t)
	c := loadCorpus(t)
	needs := map[string]bool{}
	for _, s := range c.Scenarios {
		if NeedsCassette(s) {
			needs[s.Id] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "test/proving/cassettes"))
	if err != nil {
		t.Fatalf("reading the cassette directory: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		i := strings.LastIndex(stem, ".")
		if i < 0 {
			t.Errorf("%s is not named <scenario>.<arm>.json", e.Name())
			continue
		}
		if !needs[stem[:i]] {
			t.Errorf("%s is a cassette for %q, which needs none -- either the scenario was removed or it stopped reasoning; delete the cassette", e.Name(), stem[:i])
		}
	}
}

func TestEveryNamedCheckInTheCorpusHasAnImplementation(t *testing.T) {
	// The loader refuses an UNREGISTERED name, which stops a scenario being
	// verified by nothing. This is the other half: a name that is registered
	// and has no arm in runNamedCheck would fall through to the default and
	// report a failure with a confusing message, on a scenario that is
	// probably fine.
	c := loadCorpus(t)
	used := map[string]bool{}
	for _, s := range c.Scenarios {
		for _, chk := range s.Verify {
			if chk.Named != "" {
				used[chk.Named] = true
			}
		}
	}
	// A world with one script already run, so a check that reaches the machine
	// facet has something to look at. Probing with a nil world would exercise
	// the guard rather than the check.
	probeWorld := world.New(world.Config{Scripts: map[string]string{"probe.sh": "ok\n"}})
	if _, err := probeWorld.RunScript("probe.sh", "probe", "probe:1"); err != nil {
		t.Fatalf("seeding the probe world: %v", err)
	}
	for name := range used {
		msg := runNamedCheck(name, scenario.Scenario{}, probeWorld, ArmResult{StepsExecuted: 1, RunId: "r", Resumed: true, SameRunId: true})
		if strings.Contains(msg, "has no implementation in the runner") {
			t.Errorf("the corpus uses the named check %q and the runner does not implement it", name)
		}
	}
	if len(used) == 0 {
		t.Error("no scenario uses a named check; the escape hatch is untested and would rot")
	}
}

func TestEveryRegisteredMetricIsClaimedBySomeScenario(t *testing.T) {
	// A registered metric nothing produces is a row on the honesty table with
	// no number under it, forever. That is worse than not registering it: the
	// page promises a measurement the suite never makes.
	c := loadCorpus(t)
	claimed := map[figure.Metric]bool{}
	for _, s := range c.Scenarios {
		for _, m := range s.Claims {
			claimed[m] = true
		}
	}
	var missing []string
	for _, m := range figure.RegisteredMetrics() {
		if !claimed[m] {
			missing = append(missing, string(m))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d registered metric(s) that no scenario claims:\n  %s\n\n"+
			"Either add a scenario that produces it, or remove the registration: an unclaimed metric is a "+
			"promise on the honesty table that the suite never keeps.", len(missing), strings.Join(missing, "\n  "))
	}
}
