package proving

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/proving/cassette"
	"github.com/znasllc-io/memql/component/proving/scenario"
	"github.com/znasllc-io/memql/component/proving/world"
)

// overheadRuns is how many times each arm of the overhead measurement runs.
// Odd, so the median is an observation rather than an average of two.
const overheadRuns = 5

// MeasureJournalOverhead answers "what does the journal itself add per step".
//
// # The measurement this REPLACES, and why it was wrong
//
// The obvious instrument is the platform arm's wall-clock over the baseline
// arm's, and it is nonsense. The baseline is an in-process loop that touches
// no database; the platform opens an executor and writes journal rows to
// Postgres. The first published figure was a ratio of 34,000, which is not the
// journal's overhead -- it is the cost of having a database at all, wearing
// the journal's label.
//
// That is the failure the whole suite exists to prevent, produced by the
// suite, so it is worth naming precisely: the number was arithmetically
// correct and measured something other than what its name said.
//
// # The instrument that is right
//
// The SAME automation, the SAME steps, the SAME executor and the SAME process,
// run twice: once with an engine and once without. `newWorkJournal` returns nil
// for a nil executor, so an executor built with no engine runs every step and
// writes no journal row -- and every step here is a function step dispatched to
// the proving registry, which never needs the engine. The only difference
// between the two runs is the journal, which is what makes the difference the
// journal's.
//
// The two arms ALTERNATE rather than running in blocks, so a machine that
// warms up or is disturbed mid-measurement disturbs both equally.
//
// ok is false when the measurement cannot be made honestly -- an unjournaled
// run that failed, or one too fast to time. The caller publishes `unmeasured`
// with the returned reason rather than a number.
func (r *Runner) MeasureJournalOverhead(ctx context.Context, s scenario.Scenario) (msPerStep float64, ok bool, reason string) {
	if len(s.Inject) > 0 {
		return 0, false, "the scenario injects a failure, so the two runs do different work and the difference would not be the journal's"
	}
	if len(s.Steps) == 0 {
		return 0, false, "the scenario has no steps"
	}

	run := func(withJournal bool) (time.Duration, error) {
		w := world.New(world.Config{Scripts: scriptsOf(s), Addresses: addressesOf(s), Routes: routesOf(s)})
		var player *cassette.Player
		if needsModel(s) {
			c, found := r.Cassettes.For(s.Id, "platform")
			if !found {
				return 0, fmt.Errorf("no platform cassette for %s", s.Id)
			}
			player = cassette.NewPlayer(c)
		}
		reg := newStepRegistry(s, w, player)
		opts := automations.ExecutorOptions{StepRegistry: reg, Logger: r.Logger}
		if withJournal {
			opts.Engine = r.Engine
		}
		exec := automations.NewExecutor(opts)
		defer exec.Close()

		auto := buildAutomation(s, automationName(s))
		started := time.Now()
		run, err := exec.Execute(ctx, auto, "proving-overhead")
		elapsed := time.Since(started)
		if err != nil && run == nil {
			return 0, err
		}
		if run.Status == "failed" {
			return 0, fmt.Errorf("the run failed, so its timing measures a partial execution")
		}
		return elapsed, nil
	}

	var journaled, bare []float64
	for i := 0; i < overheadRuns; i++ {
		// Alternate, so drift over the measurement disturbs both equally.
		d, err := run(true)
		if err != nil {
			return 0, false, "the journaled run failed: " + err.Error()
		}
		journaled = append(journaled, float64(d.Nanoseconds()))

		d, err = run(false)
		if err != nil {
			return 0, false, "the unjournaled run failed: " + err.Error()
		}
		bare = append(bare, float64(d.Nanoseconds()))
	}

	medJournaled := medianOf(journaled)
	medBare := medianOf(bare)
	if medBare <= 0 || medJournaled <= 0 {
		return 0, false, "the runs were too fast to time"
	}
	// MILLISECONDS PER STEP, not a ratio against the unjournaled run. A ratio
	// was the first shape and it published 12,907: a speed scenario's steps
	// are deliberately trivial, so the journal is essentially all of the time
	// and the denominator is degenerate. An absolute per-step cost is a
	// number a reader can act on whatever the steps do -- and it is what the
	// epic asks for, published whichever way it falls.
	perStepNs := (medJournaled - medBare) / float64(len(s.Steps))
	return perStepNs / 1e6, true, ""
}

func medianOf(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
