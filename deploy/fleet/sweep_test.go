package fleet

import (
	"regexp"
	"strings"
	"testing"
)

// The scheduled sweeps (epic memql#3852, task memql#3856).
//
// # The failure this file exists for
//
// A sweep's candidate query is deliberately UNWINDOWED. `runningInstances`
// returns every running instance in the fleet, because a filter cannot call
// `addDuration` and the bracket therefore has to be applied per row, in the
// automation's `forEach ... where` clause.
//
// Which means the `where` is the only thing between a sweep and the whole
// fleet. Drop it from `teardownAfterGrace` -- in a refactor, in a merge
// resolution, while "simplifying" -- and the next 04:00 UTC run tears down
// every suspended tenant we have, having taken a final backup of each and
// reported complete success. There is no error, no partial state, and no
// second chance: the Applications are gone and the finalizer has cascaded.
//
// A `where` clause is a small thing to lose and an unrecoverable thing to have
// lost, so it gets a gate.

var (
	sweepAutomation = regexp.MustCompile(`(?m)^automation\s+(\w+)\s*\{`)
	scheduleTrigger = regexp.MustCompile(`@trigger\(schedule=`)
	forEachClause   = regexp.MustCompile(`forEach\s+\w+\s+in\s+[\w.]+\s*(where\b)?`)
)

// sweepBlocks returns each automation in trial.memql with its source, including
// the @trigger line above it.
func sweepBlocks(t *testing.T) map[string]string {
	t.Helper()
	src := fleetFile(t, "trial.memql")
	out := map[string]string{}

	lines := strings.Split(src, "\n")
	var preamble []string
	for i := 0; i < len(lines); i++ {
		m := sweepAutomation.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.TrimSpace(lines[i]) == "" {
				preamble = nil
			} else {
				preamble = append(preamble, lines[i])
			}
			continue
		}
		body := append(append([]string{}, preamble...), lines[i])
		depth := strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		for i+1 < len(lines) && depth > 0 {
			i++
			body = append(body, lines[i])
			depth += strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		}
		out[m[1]] = strings.Join(body, "\n")
		preamble = nil
	}
	return out
}

// TestEverySweepNarrowsItsCandidates.
//
// Every `forEach` over a sweep's candidate set must carry a `where`. The
// candidate queries return everything in a status by design, so a bare `forEach`
// acts on the entire fleet -- and the sweep that would do the most damage is
// also the one whose action cannot be undone.
func TestEverySweepNarrowsItsCandidates(t *testing.T) {
	blocks := sweepBlocks(t)
	if len(blocks) == 0 {
		t.Fatal("parsed no automations from trial.memql -- either the file moved or this parse stopped matching, and either way this gate is watching nothing")
	}

	var checked int
	for name, body := range blocks {
		if !scheduleTrigger.MatchString(body) {
			continue
		}
		checked++

		loops := forEachClause.FindAllStringSubmatch(body, -1)
		if len(loops) == 0 {
			t.Errorf("scheduled automation %s has no forEach; a sweep that acts on nothing is a sweep that silently does not run", name)
			continue
		}
		for _, loop := range loops {
			if loop[1] == "" {
				t.Errorf("scheduled automation %s has a forEach with NO `where` clause:\n    %s\n"+
					"Its candidate query is unwindowed by design -- it returns every row in a status -- so this loop acts on the whole fleet. For teardownAfterGrace that is every suspended tenant destroyed, with a successful-looking run and nothing to recover from.",
					name, strings.TrimSpace(loop[0]))
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no scheduled automations, so this gate checked nothing")
	}
}

// TestTheDestructiveSweepIsTheOnlyTeardownPath.
//
// `requestInstanceTeardown` is the one mutation in this domain whose
// consequence is irreversible. It must be reachable from exactly one place: the
// grace-expiry sweep.
//
// The property being protected is a SEQUENCE, not a permission. Nothing a
// customer or an operator does in one action destroys data; the path is pause,
// then fourteen days, then this. A second caller -- a convenience mutation, an
// Orbit action, a "delete my account" flow that seemed obviously fine -- removes
// the fourteen days without removing anything that looks like a safeguard.
func TestTheDestructiveSweepIsTheOnlyTeardownPath(t *testing.T) {
	const teardown = "requestInstanceTeardown"

	callers := map[string]int{}
	for _, file := range []string{"automations.memql", "trial.memql", "billing.memql"} {
		src := fleetFile(t, file)
		// Count call sites, not the import line and not the doc comments.
		for line := range strings.SplitSeq(src, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "///") || strings.HasPrefix(trimmed, "use ") {
				continue
			}
			if strings.HasPrefix(trimmed, teardown+" {") {
				callers[file]++
			}
		}
	}

	total := 0
	for _, n := range callers {
		total += n
	}
	switch {
	case total == 0:
		t.Fatalf("%s is called from nowhere -- the grace-expiry sweep does not destroy anything, so a torn-down tenant is one we keep paying for", teardown)
	case total > 1:
		t.Errorf("%s is called from %d places (%v). It must have exactly one caller: the grace-expiry sweep. What protects a customer's data here is a SEQUENCE -- pause, fourteen days, teardown -- and a second caller removes the fourteen days without removing anything that looks like a safeguard.", teardown, total, callers)
	case callers["trial.memql"] != 1:
		t.Errorf("%s's single caller is not in trial.memql (found %v); the only path to it is the grace-expiry sweep", teardown, callers)
	}
}
