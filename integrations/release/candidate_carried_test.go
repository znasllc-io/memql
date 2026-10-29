package release

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var carriedTestNow = time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)

func carriedTestRun(id string, attempt int, rerunOf string, queued time.Time) carriedRun {
	return carriedRun{ID: id, OwnerUserID: "owner", PipelineID: "pipeline", Repository: "acme/engine", SHA: strings.Repeat("b", 40),
		Mode: "full", Event: "release", RunKey: "release-key", RerunOf: rerunOf, Version: "v1.2.3", ReleaseTag: "v1.2.3",
		WorkRunID: "work-" + id, Attempt: attempt, QueuedAt: queued}
}

func carriedTestReader(runs map[string]carriedRun) func(string) (carriedRun, error) {
	return func(id string) (carriedRun, error) {
		run, ok := runs[id]
		if !ok {
			return carriedRun{}, errors.New("no such run")
		}
		return run, nil
	}
}

// TestCarriedChainWalksToTheFirstAttempt: the attempts a carried release pass
// may stand on are the earlier attempts of the same release, newest first,
// reached through rerunOf alone.
func TestCarriedChainWalksToTheFirstAttempt(t *testing.T) {
	runs := map[string]carriedRun{
		"a1": carriedTestRun("a1", 1, "", carriedTestNow.Add(-2*time.Hour)),
		"a2": carriedTestRun("a2", 2, "a1", carriedTestNow.Add(-time.Hour)),
	}
	carrying := carriedTestRun("a3", 3, "v1:pipelines:run:a2", carriedTestNow)
	chain, err := carriedChain(carrying, carriedTestReader(runs))
	if err != nil || len(chain) != 2 || chain[0].ID != "a2" || chain[1].ID != "a1" {
		t.Fatalf("chain = %+v, %v", chain, err)
	}
}

// TestCarriedChainRefusals: a chain that leaves its release, skips a link,
// does not end at attempt 1, outgrows the bound or began more than 24 hours
// before the carrying attempt proves nothing.
func TestCarriedChainRefusals(t *testing.T) {
	for name, build := range map[string]func(map[string]carriedRun) carriedRun{
		"a first attempt carries nothing": func(runs map[string]carriedRun) carriedRun { return runs["a1"] },
		"missing link": func(runs map[string]carriedRun) carriedRun {
			return carriedTestRun("a2", 2, "gone", carriedTestNow)
		},
		"other run key": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.RunKey = "push-key"
			runs["a1"] = r
			return runs["a2"]
		},
		"other commit": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.SHA = strings.Repeat("c", 40)
			runs["a1"] = r
			return runs["a2"]
		},
		"other event": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.Event = "push"
			runs["a1"] = r
			return runs["a2"]
		},
		"other tag": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.Version, r.ReleaseTag = "v1.2.2", "v1.2.2"
			runs["a1"] = r
			return runs["a2"]
		},
		"other pipeline": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.PipelineID = "other"
			runs["a1"] = r
			return runs["a2"]
		},
		"not a release": func(runs map[string]carriedRun) carriedRun { r := runs["a2"]; r.Event = "push"; return r },
		"attempt does not decrease": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.Attempt = 2
			runs["a1"] = r
			return runs["a2"]
		},
		"stale chain": func(runs map[string]carriedRun) carriedRun {
			r := runs["a1"]
			r.QueuedAt = carriedTestNow.Add(-25 * time.Hour)
			runs["a1"] = r
			return runs["a2"]
		},
		"more than 50 attempts": func(runs map[string]carriedRun) carriedRun {
			for n := 1; n <= 51; n++ {
				rerunOf := ""
				if n > 1 {
					rerunOf = fmt.Sprintf("long%d", n-1)
				}
				runs[fmt.Sprintf("long%d", n)] = carriedTestRun(fmt.Sprintf("long%d", n), n, rerunOf, carriedTestNow)
			}
			return runs["long51"]
		},
	} {
		t.Run(name, func(t *testing.T) {
			runs := map[string]carriedRun{
				"a1": carriedTestRun("a1", 1, "", carriedTestNow.Add(-time.Hour)),
				"a2": carriedTestRun("a2", 2, "a1", carriedTestNow),
			}
			carrying := build(runs)
			if chain, err := carriedChain(carrying, carriedTestReader(runs)); err == nil {
				t.Fatalf("a chain that proves nothing was accepted: %+v", chain)
			}
		})
	}
}
