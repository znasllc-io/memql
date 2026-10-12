package work

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

// policyIndex is where the policy for (concept, env) sits in the list, or -1.
func policyIndex(concept, env string) int {
	return slices.IndexFunc(operationalPolicies, func(p operationalPolicy) bool { return p.concept == concept && p.env == env })
}

// TestOperationalPoliciesSharingAWindowShareItsDefault. A window is keyed by
// its env name, never by its concept: two policies read v1:work:run, and the
// three pipelines policies read one variable. Policies naming one variable
// must agree on its default, or the window would be whichever was computed
// last.
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
		t.Fatal("no two policies share a window; the pipelines policies should")
	}
}

// TestPipelinesWindowRetiresTheRunItsWorkAndItsGoal pins rulings R9 and R28:
// one variable, three policies -- the v1:work:run a run compiled into (with
// its children), the v1:pipelines:run row, and the v1:work:goal once no run
// of it remains -- in that order. The order is what lets ONE sweep finish a
// run nothing keeps: each policy finds what the one before it left, and the
// goal's "no run remains" can only be true after the work-run policy ran. A
// run something keeps is not finished in one sweep, and its goal waits with it.
func TestPipelinesWindowRetiresTheRunItsWorkAndItsGoal(t *testing.T) {
	got := policiesOn(EnvPipelinesRunRetentionDays)
	var concepts []string
	for _, p := range got {
		concepts = append(concepts, p.concept)
		if p.days != DefaultPipelinesRunRetentionDays {
			t.Errorf("%s defaults to %d days, want %d", p.concept, p.days, DefaultPipelinesRunRetentionDays)
		}
	}
	slices.Sort(concepts)
	if want := []string{pipelinesRunConcept, goalConcept, runConcept}; !slices.Equal(concepts, want) {
		t.Fatalf("%s governs %v, want %v", EnvPipelinesRunRetentionDays, concepts, want)
	}
	work, row, goal := policyIndex(runConcept, EnvPipelinesRunRetentionDays), policyIndex(pipelinesRunConcept, EnvPipelinesRunRetentionDays), policyIndex(goalConcept, EnvPipelinesRunRetentionDays)
	if !(work < row && row < goal) {
		t.Fatalf("the pipelines policies run work run %d, run row %d, goal %d; want them in that order", work, row, goal)
	}

	// The work-run predicate is built from the driver's own prefix, inside a
	// LIKE literal: a metacharacter would widen the match and a quote would end
	// the literal. Neither may ever be in it.
	if p := pipelines.WorkTriggerPrefix; p == "" || strings.ContainsAny(p, `%_'\`) {
		t.Fatalf("pipelines.WorkTriggerPrefix %q is empty or holds a LIKE metacharacter or a quote; the policy would select the wrong runs", p)
	}
	if !strings.Contains(operationalPolicies[work].predicate, "'"+pipelines.WorkTriggerPrefix+"%'") {
		t.Errorf("the pipeline work-run policy does not select the driver's trigger %q: %s", pipelines.WorkTriggerPrefix, operationalPolicies[work].predicate)
	}

	// The system-run policy is the other reader of v1:work:run, and the two
	// must never select the same run: one is triggered by a schedule, the
	// other by the pipelines driver.
	system := policiesOn(EnvSystemRunRetentionDays)
	if len(system) != 1 || system[0].concept != runConcept || !strings.Contains(system[0].predicate, "'schedule'") {
		t.Fatalf("the system-run policy changed shape: %+v", system)
	}
}

// TestRetentionPredicatesSelectOnlyWhatTheirConceptsDeclare. A predicate that
// names a value its concept does not declare matches nothing, and a sweep
// that retires nothing looks exactly like one with nothing to retire. So the
// values the pipelines policies select on are checked against the concepts'
// own enums, loaded from the embedded DSL -- in BOTH directions: every value
// a policy selects is declared and selected, and every declared value it does
// NOT select is listed here with the reason. A status added to a concept
// later fails here until somebody decides whether its rows ever go.
func TestRetentionPredicatesSelectOnlyWhatTheirConceptsDeclare(t *testing.T) {
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	notAPipelines := "a goal some other surface opened; goals are not on the list"
	for _, tc := range []struct {
		concept, field string
		selected       []string
		notSelected    map[string]string // value -> why the policy leaves it
	}{
		{pipelinesRunConcept, "status", []string{"completed"}, map[string]string{
			"queued":      "not started",
			"in_progress": "driven by an agent now",
		}},
		{runConcept, "status", []string{runStatusSucceeded, runStatusFailed, runStatusCancelled}, map[string]string{
			runStatusCompiling: "not finished",
			runStatusRunning:   "not finished",
			runStatusWaiting:   "not finished",
			runStatusAbandoned: "no writer closes a pipeline's run abandoned (runnerOwnsRecovery); kept rather than guessed at",
		}},
		{goalConcept, "requestedVia", []string{"pipeline"}, map[string]string{
			"": notAPipelines, "api": notAPipelines, "ask": notAPipelines, "nexus": notAPipelines,
			"responsibility": notAPipelines, "library": notAPipelines, "materializer": notAPipelines, "agent": notAPipelines,
		}},
		{goalConcept, "origin", []string{"system"}, map[string]string{
			"user":           "a person's goal, never the driver's",
			"responsibility": "a standing responsibility's goal, never the driver's",
		}},
	} {
		at := policyIndex(tc.concept, EnvPipelinesRunRetentionDays)
		if at < 0 {
			t.Errorf("no pipelines policy on %s", tc.concept)
			continue
		}
		predicate := operationalPolicies[at].predicate
		enum := enumOf(t, tc.concept, tc.field)
		for _, v := range tc.selected {
			if !slices.Contains(enum, v) {
				t.Errorf("%s.%s declares no %q (it declares %v)", tc.concept, tc.field, v, enum)
			}
			if !strings.Contains(predicate, "'"+v+"'") {
				t.Errorf("the %s policy does not select %s %q: %s", tc.concept, tc.field, v, predicate)
			}
		}
		for v, why := range tc.notSelected {
			if !slices.Contains(enum, v) {
				t.Errorf("%s.%s no longer declares %q, listed as not selected (%s); drop it from this list", tc.concept, tc.field, v, why)
			}
			if strings.Contains(predicate, "'"+v+"'") {
				t.Errorf("the %s policy selects %s %q, listed as not selected because: %s", tc.concept, tc.field, v, why)
			}
		}
		for _, v := range enum {
			if !slices.Contains(tc.selected, v) && tc.notSelected[v] == "" {
				t.Errorf("%s.%s declares %q, which the retention policy neither selects nor lists as deliberately not selected; decide whether its rows ever go", tc.concept, tc.field, v)
			}
		}
	}
}

// enumOf is a concept field's declared enum, read from its loaded schema.
func enumOf(t *testing.T, concept, field string) []string {
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
	enum := schema.Properties[field].Enum
	if len(enum) == 0 {
		t.Fatalf("%s declares no %s enum; the check would pass over nothing", concept, field)
	}
	return enum
}

// TestRetentionWalkConstants pins what the day-window walk is bounded by
// (2026-10-11, storage growth stability): a day, a hundred candidates, four
// minutes.
func TestRetentionWalkConstants(t *testing.T) {
	if retentionCandidateWindow != 24*time.Hour || retentionCandidateLimit != 100 || retentionRunBudget != 4*time.Minute {
		t.Fatalf("the walk reads %s windows, %d candidates at a time, for %s; the decided values are 24h, 100 and 4m",
			retentionCandidateWindow, retentionCandidateLimit, retentionRunBudget)
	}
	// A read is a batch. A page larger than a batch would be split into several
	// the walk never sees one by one; a smaller one would never look full.
	if retentionCandidateLimit != retirementBatchSize {
		t.Fatalf("a candidate read returns %d and a batch is %d; they are one number", retentionCandidateLimit, retirementBatchSize)
	}
}

// TestEveryPolicysCandidateReadIsBounded: whatever the policy, a candidate read
// is bounded below and above by createdAt, ends in a constant LIMIT, carries the
// policy's predicate unchanged, and -- when it is the next page of a window --
// starts after the last row of the page before. And its placeholders are filled,
// one argument each, in the order they appear: a miscount here is a policy that
// reads the wrong window, or none.
func TestEveryPolicysCandidateReadIsBounded(t *testing.T) {
	cutoff := time.Date(2026, 9, 11, 3, 40, 0, 0, time.UTC)
	lo := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	hi := lo.Add(retentionCandidateWindow)
	cursor := &pageEnd{scanned: retentionCandidateLimit, at: lo.Add(3 * time.Hour), id: "v1:work:run:last-of-the-page-before"}
	for _, p := range operationalPolicies {
		for _, after := range []*pageEnd{nil, cursor} {
			name := fmt.Sprintf("%s on %s, paged=%v", p.concept, p.env, after != nil)
			query, params := retentionCandidateRead(p, cutoff, lo, hi, after)
			for _, want := range []string{`n."createdAt" >= ?`, `n."createdAt" < ?`, ` LIMIT ` + fmt.Sprint(retentionCandidateLimit), `(` + p.predicate + `)`} {
				if !strings.Contains(query, want) {
					t.Errorf("%s: the read lacks %q:\n%s", name, want, query)
				}
			}
			if !strings.HasSuffix(query, ` LIMIT `+fmt.Sprint(retentionCandidateLimit)) {
				t.Errorf("%s: the read does not end in its constant LIMIT:\n%s", name, query)
			}
			if got := strings.Count(query, `(n."createdAt",n.id) > (?,?)`); (after != nil) != (got == 1) {
				t.Errorf("%s: %d page cursors in the read", name, got)
			}
			if got := strings.Count(query, "?"); got != len(params) {
				t.Errorf("%s: %d placeholders and %d arguments:\n%s", name, got, len(params), query)
				continue
			}
			from := lo
			if after != nil {
				from = after.at
			}
			if params[0] != p.concept || !params[1].(time.Time).Equal(from) || !params[2].(time.Time).Equal(hi) {
				t.Errorf("%s: arguments %v, want the concept and the window [%s, %s)", name, params[:3], from, hi)
			}
			if after != nil && (!params[3].(time.Time).Equal(after.at) || params[4] != after.id) {
				t.Errorf("%s: the page cursor is %v, want (%s, %s)", name, params[3:5], after.at, after.id)
			}
			// The run policies' child test is the only one with a cutoff of its own.
			wantCutoff := p.concept == runConcept
			if has := len(params) > 0 && params[len(params)-1] == any(cutoff); has != wantCutoff {
				t.Errorf("%s: cutoff argument present=%v, want %v", name, has, wantCutoff)
			}
		}
	}
}
