// Package probe measures what a local model can ACTUALLY do, against a
// suite the cluster names by version.
//
// It exists because the label is a claim and the router needs a fact. A
// machine advertises `structured=1` because its runtime reports a
// `tools` capability -- a proxy the models package is explicit about
// choosing -- and whether the model then HOLDS to one of this platform's
// own schemas is a different question, answerable only by asking it. The
// engine ranks on the answer (record D4) and gates on nothing, which is
// the right way round for a first release: a probe that refused a
// working model on a bad run is worse than no probe.
//
// Three rules run through the package.
//
//   - A FIGURE AND AN ABSENCE ARE NEVER THE SAME SHAPE. A case that
//     scored zero and a case that could not run are opposite facts --
//     the first says the model failed, the second says nothing about the
//     model at all -- and anything that rendered both as "0" would rank a
//     working model below a broken one. Figure carries one or the other,
//     never both, and Measured() is the only way to ask.
//
//   - THE SUITE VERSION IS A PIN, NOT A FLOOR. Figures are filed on the
//     engine by (machineId, modelId, suiteVersion). Running a different
//     set of cases under a number the cluster asked for would file
//     measurements of one thing as measurements of another, so an
//     unrecognised version is REFUSED in both directions -- newer and
//     older alike.
//
//   - ONE WEDGED CASE MUST NOT COST THE OTHERS. Each case runs under its
//     own deadline. A whole-run ceiling would lose every figure to the
//     last case that hung, which is precisely the case most worth
//     reporting.
package probe

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
)

// SuiteVersion is the only suite this cockpit can run.
//
// Bump it in the SAME commit that changes any case. A case edited under
// an unchanged version silently redefines what every figure already
// filed under that number meant.
const SuiteVersion = 1

// DefaultCaseTimeout bounds one case.
//
// Two minutes rather than the model call's ten: a probe case is a fixed
// prompt of known size against a model on this machine, and one that has
// not answered in two minutes is not slow, it is stuck. The 32K
// throughput case is the one this is sized for -- a cold 27B model
// working through a 32,000-token prompt on a busy laptop.
const DefaultCaseTimeout = 2 * time.Minute

// Figure is one measurement, or the reason there is none.
type Figure struct {
	// Name is the stable key the engine files this figure under. Never
	// translated for display -- the renderer spells it out.
	Name string
	// Value is meaningless unless Measured() is true.
	Value float64
	// Unit is what Value is in ("", "tokens/sec", "sec"). An empty unit
	// is a ratio between 0 and 1.
	Unit string
	// Detail is what was seen, for a person reading the figure ("5 of 5
	// schemas held"). It never carries the value again in words: a
	// number rendered twice can disagree with itself.
	Detail string
	// AbsentReason is a complete sentence when there is no value.
	// Non-empty means Value must not be read.
	AbsentReason string
}

func (f Figure) Measured() bool { return f.AbsentReason == "" }

// Report is one run of the suite.
type Report struct {
	Model        string
	SuiteVersion int
	StartedAt    time.Time
	Figures      []Figure
}

// Figure returns the named figure. A name the suite does not produce
// comes back absent with a reason rather than as a zero value, so a
// caller asking for a figure that was never in this suite version reads
// a sentence instead of a silent 0.
func (r Report) Figure(name string) Figure {
	for _, f := range r.Figures {
		if f.Name == name {
			return f
		}
	}
	return Figure{
		Name:         name,
		AbsentReason: fmt.Sprintf("probe suite %d has no case named %q.", r.SuiteVersion, name),
	}
}

// Request is one run.
type Request struct {
	// SuiteVersion is what the CLUSTER asked for. Checked against
	// SuiteVersion before anything runs.
	SuiteVersion int
	// Model is the runtime-facing id, passed through untranslated.
	Model string
	// Client reaches the runtime. Supplied by the caller rather than
	// built here so the suite is drivable against a fake.
	Client modelcall.Client
	// CaseTimeout bounds each case; zero means DefaultCaseTimeout.
	CaseTimeout time.Duration
	// Progress, when set, is called before each case starts and again
	// when it finishes. BEFORE, not only after: somebody watching a
	// probe that hangs has to be able to read off the screen which case
	// hung, and a line printed only on completion is a line the wedged
	// case never prints.
	Progress func(Event)
	// Now is the clock, a seam for the throughput cases.
	Now func() time.Time
}

// Event is one step of a run, for a caller rendering progress.
type Event struct {
	Case    string
	Index   int
	Total   int
	Started bool
	Figure  Figure
}

// ErrUnknownSuite is the version refusal. A sentinel so a caller can
// tell it from a runtime failure -- the fixes are completely different,
// and only one of them is "update the cockpit".
var ErrUnknownSuite = errors.New("probe: unknown suite version")

// Run executes the suite and reports a figure per case.
//
// It returns an error ONLY for a refusal that stopped the whole run --
// today, the version gate. A case that could not run is reported as an
// absent figure inside the Report, because the run still measured
// everything else and losing those figures to one failure is the shape
// this package exists to avoid.
func Run(ctx context.Context, req Request) (Report, error) {
	if req.SuiteVersion != SuiteVersion {
		return Report{}, fmt.Errorf("%w: %s", ErrUnknownSuite, unknownSuiteSentence(req.SuiteVersion))
	}
	if req.Client == nil {
		return Report{}, errors.New("probe: no runtime client; nothing to measure against")
	}

	now := req.Now
	if now == nil {
		now = time.Now
	}
	timeout := req.CaseTimeout
	if timeout <= 0 {
		timeout = DefaultCaseTimeout
	}

	cases := suite()
	report := Report{Model: req.Model, SuiteVersion: SuiteVersion, StartedAt: now()}

	for i, c := range cases {
		emit(req.Progress, Event{Case: c.Name, Index: i + 1, Total: len(cases), Started: true})

		caseCtx, cancel := context.WithTimeout(ctx, timeout)
		figs := c.Run(caseCtx, req.Model, req.Client, now)
		cancel()

		// A case that ran out of time reports the TIMEOUT rather than
		// whatever the truncated attempt produced, on every figure it
		// owns. The distinction matters because a half-finished
		// throughput case yields a plausible-looking number that is
		// simply wrong, and a wrong number ranks a machine where an
		// absent one does not.
		if errors.Is(caseCtx.Err(), context.DeadlineExceeded) {
			figs = c.absent(fmt.Sprintf(
				"the %s case did not answer within %s and was ended.", c.Label, timeout))
		}

		// A cancelled PARENT is different: the person interrupted, and
		// the remaining cases are not measured at all rather than
		// reported as failures they never got the chance to be.
		//
		// A FIGURE THIS CASE ALREADY MEASURED IS KEPT, which is the
		// package's first rule applied to its own bookkeeping: the
		// cancellation can land after c.Run returned complete figures
		// -- the window is one clock read wide -- and overwriting a
		// real measurement with "was not run" is exactly the
		// figure-and-absence confusion this package exists to prevent,
		// committed by the code that documents it.
		if ctx.Err() != nil {
			figs = keepMeasured(figs, c.absent(
				fmt.Sprintf("the %s case was not run: the probe was cancelled.", c.Label)))
			report.Figures = append(report.Figures, figs...)
			emitAll(req.Progress, c, i, len(cases), figs)
			for _, rest := range cases[i+1:] {
				report.Figures = append(report.Figures, rest.absent(
					fmt.Sprintf("the %s case was not run: the probe was cancelled.", rest.Label))...)
			}
			return report, nil
		}

		report.Figures = append(report.Figures, figs...)
		emitAll(req.Progress, c, i, len(cases), figs)
	}

	sort.SliceStable(report.Figures, func(i, j int) bool {
		return caseOrder(report.Figures[i].Name) < caseOrder(report.Figures[j].Name)
	})
	return report, nil
}

// unknownSuiteSentence is the whole refusal, written for the terminal it
// lands in. It names both numbers, because "unsupported version" without
// them leaves a person with nothing to compare.
func unknownSuiteSentence(asked int) string {
	return fmt.Sprintf(
		"this machine knows probe suite version %d; the cluster asked for version %d. "+
			"Update the cockpit on this machine and run the probe again.",
		SuiteVersion, asked)
}

func emit(f func(Event), e Event) {
	if f != nil {
		f(e)
	}
}

// keepMeasured overlays `absent` onto `measured`, keeping any figure
// that actually carries a value. Both slices name the same figures in
// the same order, because both come from the same case.
func keepMeasured(measured, absent []Figure) []Figure {
	out := make([]Figure, 0, len(absent))
	for _, a := range absent {
		kept := a
		for _, m := range measured {
			if m.Name == a.Name && m.Measured() {
				kept = m
				break
			}
		}
		out = append(out, kept)
	}
	return out
}

func emitAll(f func(Event), c probeCase, i, total int, figs []Figure) {
	for _, fig := range figs {
		emit(f, Event{Case: c.Name, Index: i + 1, Total: total, Figure: fig})
	}
}
