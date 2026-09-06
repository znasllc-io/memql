package scenario

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/proving/figure"
)

func intp(v int) *int { return &v }

// valid returns a scenario that passes Validate, for a test to break one field
// of at a time. A helper per file rather than a package fixture: a shared
// mutable fixture is how one test's edit becomes another test's mystery.
func valid() Scenario {
	return Scenario{
		Id:        "durability.resume-elsewhere",
		Family:    figure.FamilyDurability,
		Title:     "A run killed mid-step resumes elsewhere with no duplicated effect",
		Goal:      "Reconcile the ledger for {{account}}",
		Variables: []map[string]string{{"account": "acme"}},
		Steps: []Step{
			{Key: "read", Type: "query", Target: "ledgerLines"},
			{Key: "post", Type: "mutation", Target: "postReconciliation", DependsOn: []string{"read"}, Effect: FacetMailbox},
		},
		World:  World{Mailbox: &MailboxWorld{Addresses: []string{"ops@example.test"}}},
		Inject: []Injection{{At: "post", Kind: KindKill}},
		Verify: []Check{
			{Rows: "v1:work:step", Where: map[string]string{"key": "post", "status": "done"}, Count: intp(1)},
			{Effects: "mailbox.sent", Count: intp(1)},
		},
		Claims: []figure.Metric{figure.MetricDuplicatedEffects, figure.MetricResumeReExecuted},
	}
}

func TestTheValidFixtureIsValid(t *testing.T) {
	// A negative control for every test below: if the fixture stopped being
	// valid, each "breaking field X is refused" test would pass for the wrong
	// reason and none of them would be measuring anything.
	if err := valid().Validate(); err != nil {
		t.Fatalf("the fixture every other test breaks one field of does not itself validate: %v", err)
	}
}

func TestEveryClosedSetIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*Scenario)
		want   string
	}{
		{"family", func(s *Scenario) { s.Family = "speedy" }, "not one of"},
		{"injection kind", func(s *Scenario) { s.Inject[0].Kind = "crash" }, "not one of"},
		{"effect facet", func(s *Scenario) { s.Steps[1].Effect = "database" }, "not one of"},
		{"effects facet in a verifier", func(s *Scenario) { s.Verify[1].Effects = "database.written" }, "not one of"},
		{"named check", func(s *Scenario) { s.Verify[0] = Check{Named: "somethingInvented", Count: intp(1)} }, "not registered"},
		{"claimed metric", func(s *Scenario) { s.Claims = []figure.Metric{"made.up"} }, "not a registered metric"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.break_(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted a value outside the closed set")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestAnUnknownNamedCheckIsRefusedAtLoadRatherThanAtRun(t *testing.T) {
	// A scenario that cannot be verified must not be able to report a pass,
	// and the only way to guarantee that is to refuse it before anything runs.
	s := valid()
	s.Verify = append(s.Verify, Check{Named: "checkTheVibes", Count: intp(1)})
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an unregistered named check")
	}
	if !strings.Contains(err.Error(), "the registry is closed") {
		t.Errorf("error = %v, want it to say the registry is closed and list the legal names", err)
	}
	for _, name := range NamedChecks() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not list the registered check %q, so the author has to go and find them", name)
		}
	}
}

func TestEveryRegisteredNamedCheckSaysWhyItIsNotData(t *testing.T) {
	// The escape hatch exists for what data cannot express. An entry with no
	// such reason is one that should have been a row or effect assertion, and
	// the registry is where that gets noticed.
	if len(NamedChecks()) == 0 {
		t.Fatal("no named checks registered; the test would pass vacuously")
	}
	for _, name := range NamedChecks() {
		reason, ok := NamedCheckReason(name)
		if !ok {
			t.Fatalf("%q is listed but not registered", name)
		}
		if len(strings.Fields(reason)) < 6 {
			t.Errorf("%q's reason is %q; it must say what data could not express", name, reason)
		}
	}
}

func TestAScenarioWithNoVerifierIsRefused(t *testing.T) {
	s := valid()
	s.Verify = nil
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a scenario with no verifier")
	}
	if !strings.Contains(err.Error(), "cannot report a pass") {
		t.Errorf("error = %v, want it to say why", err)
	}
}

func TestACheckThatAssertsNoCountIsRefused(t *testing.T) {
	// `count: 0` and "no count asserted" are different claims, and the
	// durability family's headline is the first of the two. A default would
	// make it the second.
	_, err := Check{Rows: "v1:work:step"}.Form()
	if err == nil {
		t.Fatal("Form() accepted a check with neither count nor atLeast")
	}
	if !strings.Contains(err.Error(), "count: 0") {
		t.Errorf("error = %v, want it to name the spelling for `nothing happened`", err)
	}
	if _, err := (Check{Rows: "v1:work:step", Count: intp(0)}).Form(); err != nil {
		t.Fatalf("an explicit zero was refused: %v", err)
	}
}

func TestACheckNamingTwoFormsIsRefused(t *testing.T) {
	_, err := Check{Rows: "v1:work:step", Effects: "mailbox.sent", Count: intp(1)}.Form()
	if err == nil {
		t.Fatal("Form() accepted a check naming two forms")
	}
	if !strings.Contains(err.Error(), "alternatives") {
		t.Errorf("error = %v, want it to say they are alternatives", err)
	}
}

func TestAnEffectOnAFacetTheWorldDoesNotDeclareIsRefused(t *testing.T) {
	// Otherwise the step's effect silently goes nowhere and the scenario
	// reports a clean run.
	s := valid()
	s.World = World{}
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an effect against an undeclared world")
	}
	if !strings.Contains(err.Error(), "declares no mailbox world") {
		t.Errorf("error = %v, want it to name the missing facet", err)
	}
}

func TestAnInjectionAtAnUnknownStepIsRefused(t *testing.T) {
	s := valid()
	s.Inject[0].At = "psot"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not a step") {
		t.Fatalf("Validate() = %v, want a refusal naming the typo", err)
	}
}

func TestATransientInjectionThatNeverStopsIsRefusedUnlessFailureIsExpected(t *testing.T) {
	// A transient failure that fires on every attempt is a failure, not a
	// recovery. The corpus may express that, but not by accident.
	s := valid()
	s.Inject = []Injection{{At: "post", Kind: KindTransient, Message: "connection refused"}}
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate() accepted an always-firing transient injection under a success verifier")
	}
	if !strings.Contains(err.Error(), "once: true") {
		t.Errorf("error = %v, want it to name the fix", err)
	}

	s.Inject[0].Once = true
	if err := s.Validate(); err != nil {
		t.Fatalf("a once-only transient injection was refused: %v", err)
	}

	// And the deliberate form: a verifier that expects the failure.
	s.Inject[0].Once = false
	s.Verify = []Check{{Rows: "v1:work:run", Where: map[string]string{"status": "failed"}, Count: intp(1)}}
	if err := s.Validate(); err != nil {
		t.Fatalf("an always-firing injection under a failure verifier was refused: %v", err)
	}
}

func TestAnInjectionOtherThanKillMustCarryItsMessage(t *testing.T) {
	// The classifier's rules table matches on the error text, so a scenario
	// proving "the rules classified this without a model call" has to say
	// exactly what the failure looked like.
	s := valid()
	s.Inject = []Injection{{At: "post", Kind: KindEnvironment}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "matches on the error text") {
		t.Fatalf("Validate() = %v, want a refusal explaining why the message matters", err)
	}
	// A kill is not a symptom: nothing classifies it, so it needs no message.
	s.Inject = []Injection{{At: "post", Kind: KindKill}}
	if err := s.Validate(); err != nil {
		t.Fatalf("a kill injection was made to carry a message: %v", err)
	}
}

func TestAClaimedMetricMustBelongToTheScenariosFamily(t *testing.T) {
	s := valid()
	s.Claims = []figure.Metric{figure.MetricWallClockPerGoal}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "not this scenario's") {
		t.Fatalf("Validate() = %v, want a refusal", err)
	}
}

func TestAnIdMustBeginWithItsFamily(t *testing.T) {
	s := valid()
	s.Id = "resume-elsewhere"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "grouped by the prefix") {
		t.Fatalf("Validate() = %v, want a refusal", err)
	}
}

// --- Loading ---------------------------------------------------------------

const minimalScenario = `{
  "id": "durability.kill-and-resume",
  "family": "durability",
  "title": "A killed run resumes from the journal",
  "goal": "Reconcile {{account}}",
  "variables": [{"account": "acme"}],
  "steps": [{"key": "post", "type": "mutation", "target": "post", "effect": "mailbox"}],
  "world": {"mailbox": {"addresses": ["ops@example.test"]}},
  "inject": [{"at": "post", "kind": "kill"}],
  "verify": [{"effects": "mailbox.sent", "count": 1}],
  "claims": ["durability.duplicatedSideEffects"]
}`

func TestLoadReadsAndValidates(t *testing.T) {
	fsys := fstest.MapFS{
		"scenarios/durability.kill-and-resume.json": {Data: []byte(minimalScenario)},
	}
	c, err := Load(fsys, "scenarios")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Scenarios) != 1 {
		t.Fatalf("loaded %d scenarios, want 1", len(c.Scenarios))
	}
	if c.Fingerprint == "" {
		t.Error("the corpus has no fingerprint; two runs could not be told apart")
	}
}

func TestLoadRefusesAnUnknownKey(t *testing.T) {
	// A silently ignored `injcet` is a scenario that measures a clean run
	// while its author believes it injects a failure.
	bad := strings.Replace(minimalScenario, `"inject"`, `"injcet"`, 1)
	fsys := fstest.MapFS{"scenarios/durability.kill-and-resume.json": {Data: []byte(bad)}}
	_, err := Load(fsys, "scenarios")
	if err == nil {
		t.Fatal("Load accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "injcet") {
		t.Errorf("error = %v, want it to name the typo", err)
	}
}

func TestLoadRefusesAnIdThatDisagreesWithItsFilename(t *testing.T) {
	fsys := fstest.MapFS{"scenarios/durability.other-name.json": {Data: []byte(minimalScenario)}}
	if _, err := Load(fsys, "scenarios"); err == nil || !strings.Contains(err.Error(), "must agree") {
		t.Fatalf("Load = %v, want a refusal", err)
	}
}

func TestLoadRefusesAnEmptyCorpus(t *testing.T) {
	// An empty corpus passes every gate and proves nothing, which is the most
	// dangerous state this suite can be in.
	fsys := fstest.MapFS{"scenarios/README.md": {Data: []byte("nothing here")}}
	_, err := Load(fsys, "scenarios")
	if err == nil {
		t.Fatal("Load accepted an empty corpus")
	}
	if !strings.Contains(err.Error(), "proves nothing") {
		t.Errorf("error = %v, want it to say why an empty corpus is a failure", err)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	// Fixing one error at a time against a CI round trip is the reason
	// nobody keeps a corpus current.
	a := strings.Replace(minimalScenario, `"family": "durability"`, `"family": "nope"`, 1)
	b := strings.Replace(minimalScenario, `"id": "durability.kill-and-resume"`, `"id": "durability.second"`, 1)
	b = strings.Replace(b, `"verify": [{"effects": "mailbox.sent", "count": 1}]`, `"verify": []`, 1)
	fsys := fstest.MapFS{
		"scenarios/durability.kill-and-resume.json": {Data: []byte(a)},
		"scenarios/durability.second.json":          {Data: []byte(b)},
	}
	_, err := Load(fsys, "scenarios")
	if err == nil {
		t.Fatal("Load accepted two broken scenarios")
	}
	if !strings.Contains(err.Error(), "2 problem(s)") {
		t.Errorf("error = %v, want both problems reported at once", err)
	}
}

func TestTheFingerprintTracksContentAndNotFormatting(t *testing.T) {
	compact := fstest.MapFS{"s/durability.kill-and-resume.json": {Data: []byte(minimalScenario)}}
	spaced := fstest.MapFS{"s/durability.kill-and-resume.json": {Data: []byte(strings.ReplaceAll(minimalScenario, "\n", "\n  "))}}
	a, err := Load(compact, "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b, err := Load(spaced, "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Error("reformatting the JSON changed the fingerprint, so a whitespace commit would read as a different corpus")
	}

	changed := strings.Replace(minimalScenario, `"count": 1`, `"count": 2`, 1)
	c, err := Load(fstest.MapFS{"s/durability.kill-and-resume.json": {Data: []byte(changed)}}, "s")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.Fingerprint == c.Fingerprint {
		t.Error("changing an asserted count did not change the fingerprint, so two different corpora would read as a trend")
	}
}

func TestCorpusControlsDemandsANegativeControlForEveryBlockingZeroClaim(t *testing.T) {
	// A counter that is never incremented on ANY path reads as zero forever.
	c := Corpus{Scenarios: []Scenario{valid()}}
	err := c.CorpusControls()
	if err == nil {
		t.Fatal("CorpusControls accepted a zero-claim with no negative control")
	}
	if !strings.Contains(err.Error(), "dead counter") {
		t.Errorf("error = %v, want it to name the failure mode", err)
	}

	control := valid()
	control.Id = "durability.control-duplicates-are-detected"
	control.NegativeControlFor = figure.MetricDuplicatedEffects
	c2 := valid()
	c2.NegativeControlFor = figure.MetricResumeReExecuted
	c2.Id = "durability.control-resume-reexecutes"
	if err := (Corpus{Scenarios: []Scenario{valid(), control, c2}}).CorpusControls(); err != nil {
		t.Fatalf("CorpusControls refused a corpus with controls for both: %v", err)
	}
}
