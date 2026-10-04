package work

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// policiesOn is every operational policy that runs on one window.
func policiesOn(env string) []operationalPolicy {
	var out []operationalPolicy
	for _, p := range operationalPolicies {
		if p.env == env {
			out = append(out, p)
		}
	}
	return out
}

// TestOperationalPoliciesSharingAWindowShareItsDefault. A window is keyed by
// its env name, never by its concept: two policies read v1:work:run, and the
// two pipelines policies read one variable. Policies naming one variable must
// agree on its default, or the window would be whichever was computed last.
func TestOperationalPoliciesSharingAWindowShareItsDefault(t *testing.T) {
	defaults := map[string]int{}
	shared := 0
	for _, p := range operationalPolicies {
		if d, ok := defaults[p.env]; ok {
			shared++
			if d != p.days {
				t.Errorf("%s defaults to %d on one policy and %d on another", p.env, d, p.days)
			}
		}
		defaults[p.env] = p.days
	}
	// The reachable positive: the pipelines window IS shared, so a loop that
	// found no pair would be measuring nothing.
	if shared == 0 {
		t.Fatal("no two policies share a window; the pipelines run and its work run should")
	}
}

// TestPipelinesWindowRetiresTheRunAndItsWorkRun pins ledger ruling R9's
// shape: one variable, two policies -- the v1:pipelines:run row and the
// v1:work:run it compiled into, the second with its children -- so a run and
// its work leave on the same night rather than on two clocks.
func TestPipelinesWindowRetiresTheRunAndItsWorkRun(t *testing.T) {
	got := policiesOn(EnvPipelinesRunRetentionDays)
	var concepts []string
	for _, p := range got {
		concepts = append(concepts, p.concept)
		if p.days != DefaultPipelinesRunRetentionDays {
			t.Errorf("%s defaults to %d days, want %d", p.concept, p.days, DefaultPipelinesRunRetentionDays)
		}
	}
	slices.Sort(concepts)
	if want := []string{pipelinesRunConcept, runConcept}; !slices.Equal(concepts, want) {
		t.Fatalf("%s governs %v, want %v", EnvPipelinesRunRetentionDays, concepts, want)
	}
	for _, p := range got {
		if p.concept == runConcept && !strings.Contains(p.predicate, "'"+pipelines.WorkTriggerPrefix+"%'") {
			t.Errorf("the pipeline work-run policy does not select the driver's trigger %q: %s", pipelines.WorkTriggerPrefix, p.predicate)
		}
	}
	// The system-run policy is the other reader of v1:work:run, and the two
	// must never select the same run: one is triggered by a schedule, the
	// other by the pipelines driver.
	system := policiesOn(EnvSystemRunRetentionDays)
	if len(system) != 1 || system[0].concept != runConcept || !strings.Contains(system[0].predicate, "'schedule'") {
		t.Fatalf("the system-run policy changed shape: %+v", system)
	}
}

// TestRetentionPredicatesNameStatusesTheirConceptsDeclare. A predicate that
// names a status its concept does not declare matches nothing, and a sweep
// that retires nothing looks exactly like one with nothing to retire. So the
// terminal words the pipelines policies select on are checked against the
// concepts' own enums, loaded from the embedded DSL.
func TestRetentionPredicatesNameStatusesTheirConceptsDeclare(t *testing.T) {
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	checked := 0
	for _, p := range policiesOn(EnvPipelinesRunRetentionDays) {
		words := map[string][]string{
			pipelinesRunConcept: {"completed"},
			runConcept:          {runStatusSucceeded, runStatusFailed, runStatusCancelled},
		}[p.concept]
		enum := statusEnum(t, p.concept)
		for _, word := range words {
			checked++
			if !slices.Contains(enum, word) {
				t.Errorf("%s declares no status %q (it declares %v)", p.concept, word, enum)
			}
			if !strings.Contains(p.predicate, "'"+word+"'") {
				t.Errorf("the %s policy does not select %q: %s", p.concept, word, p.predicate)
			}
		}
	}
	// Coverage: one word for the run row and three for its work run. Fewer
	// means a policy went missing and the loop above checked less than it says.
	if checked != 4 {
		t.Fatalf("checked %d terminal words, want 4 across the two pipelines policies", checked)
	}
}

func statusEnum(t *testing.T, concept string) []string {
	t.Helper()
	c, err := memorynodes.Get(concept)
	if err != nil {
		t.Fatalf("%s is not a loaded concept: %v", concept, err)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(c.Schemas["definition"], &schema); err != nil {
		t.Fatalf("%s schema: %v", concept, err)
	}
	enum := schema.Properties["status"].Enum
	if len(enum) == 0 {
		t.Fatalf("%s declares no status enum; the check would pass over nothing", concept)
	}
	return enum
}
