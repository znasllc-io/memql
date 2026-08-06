package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// TestHarnessStepStateMachineReachable is the #1635 lock-in.
//
// The v1:harness:step state machine (enforced engine-side in
// component/memql/harness_step_validation.go) is:
//
//	pending -> ready                  (dependsOn satisfied)
//	ready   -> running                (controller claims)
//	running -> done | failed | blocked
//	blocked -> ready                  (blocker done)
//	failed  -> ready                  (retry, attempt++)
//	done is terminal
//
// A fresh step is forced to land in 'pending'. The bug (#1635) was that
// the exposed mutation surface in dsl/harness/mutations.memql emitted no
// step row with status="ready": addHarnessStep hardcodes
// status="pending" and the only transition mutations were
// start (->running), complete (->done) and fail (->failed). With no
// mutation able to drive pending -> ready, the whole chain past
// 'pending' was unreachable -- start/complete/fail all bounced off the
// engine guard ("invalid transition pending -> running" etc.).
//
// This test reconstructs the engine state machine, collects the set of
// step statuses the EXPOSED step mutations can actually produce, then
// walks the machine from the forced initial 'pending' using only edges
// whose target status some mutation emits. It asserts running / done /
// failed are all reachable. Before the fix (no ready-producing mutation)
// the walk is stuck at 'pending' and this fails; after the fix
// (readyHarnessStep) the full chain is reachable and it passes.
func TestHarnessStepStateMachineReachable(t *testing.T) {
	// Engine state machine edges (mirror of stepTransitions in
	// component/memql/harness_step_validation.go). Kept here as the
	// DSL-side contract: if the engine machine changes, this must too.
	edges := map[string][]string{
		"pending": {"ready"},
		"ready":   {"running"},
		"running": {"done", "failed", "blocked"},
		"blocked": {"ready"},
		"failed":  {"ready"},
		"done":    {},
	}

	produced := stepStatusesProducedByMutations(t)

	// A fresh step is forced into 'pending' by the engine guard.
	reachable := map[string]bool{"pending": true}
	for changed := true; changed; {
		changed = false
		for from := range reachable {
			for _, to := range edges[from] {
				// An edge is drivable only if some exposed mutation can
				// land a step row carrying that target status.
				if !produced[to] {
					continue
				}
				if !reachable[to] {
					reachable[to] = true
					changed = true
				}
			}
		}
	}

	for _, want := range []string{"ready", "running", "done", "failed"} {
		if !reachable[want] {
			t.Errorf("v1:harness:step status %q is unreachable from a fresh "+
				"'pending' step via the exposed mutation surface "+
				"(dsl/harness/mutations.memql) -- no mutation drives the "+
				"transition into it (#1635). Statuses some mutation can "+
				"produce: %v", want, sortedKeys(produced))
		}
	}
}

// stepStatusesProducedByMutations parses dsl/harness/mutations.memql and
// returns the set of status string literals that `mutation step ...`
// blocks emit via a `status: "X"` field. Plan / observation / memory
// mutations are ignored -- only the step state machine is in scope.
func stepStatusesProducedByMutations(t *testing.T) map[string]bool {
	t.Helper()

	raw := readHarnessMutations(t)

	mutationHeaderRe := regexp.MustCompile(`^\s*mutate\s+(\w+)\s+\w+\s*\{`)
	statusRe := regexp.MustCompile(`^\s*status:\s*"(\w+)"`)

	produced := map[string]bool{}
	inStepMutation := false
	for _, line := range strings.Split(raw, "\n") {
		// Strip line comments so commented-out prose can't register a status.
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		if m := mutationHeaderRe.FindStringSubmatch(line); m != nil {
			inStepMutation = m[1] == "step"
			continue
		}
		if !inStepMutation {
			continue
		}
		if m := statusRe.FindStringSubmatch(line); m != nil {
			produced[m[1]] = true
		}
	}
	if len(produced) == 0 {
		t.Fatal("parsed zero step statuses from dsl/harness/mutations.memql -- " +
			"the parser regex or file layout changed")
	}
	return produced
}

func readHarnessMutations(t *testing.T) string {
	t.Helper()
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		if !strings.HasSuffix(p, "harness/mutations.memql") {
			continue
		}
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		return string(raw)
	}
	t.Fatal("harness/mutations.memql not found in the DSL tree")
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// tiny insertion sort to avoid an extra import
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
