package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/probe"
)

func offering(infos ...models.Info) models.Inventory {
	return models.Inventory{Floor: models.FloorVerdict{Met: true}, Models: infos}
}

func allowedModel(id string, attrs models.Attributes) models.Info {
	return models.Info{
		ID: id, Kind: models.KindOllama, Runtime: models.KindOllama,
		BaseURL: "http://127.0.0.1:11434", Allowed: true, Attributes: attrs,
	}
}

func runProbeWith(t *testing.T, inv models.Inventory, report probe.Report, runErr error, mut func(*probeRun)) (int, string) {
	t.Helper()
	out := &strings.Builder{}
	r := &probeRun{
		out:        out,
		suite:      probe.SuiteVersion,
		policyPath: "/home/op/.memql/policy.yaml",
		inventory:  func(context.Context) models.Inventory { return inv },
		client:     func(models.Info) modelcall.Client { return nil },
		run: func(context.Context, probe.Request) (probe.Report, error) {
			return report, runErr
		},
	}
	if mut != nil {
		mut(r)
	}
	return runProbe(context.Background(), r), out.String()
}

func measuredReport() probe.Report {
	return probe.Report{
		Model: "qwen3.5:9b", SuiteVersion: probe.SuiteVersion,
		Figures: []probe.Figure{
			{Name: probe.FigureStructuredValidity, Value: 1.0, Detail: "5 of 5 schemas held"},
			{Name: probe.FigureToolCorrectness, Value: 0.67, Detail: "2 of 3 definitions (missed: search)"},
			{Name: probe.FigureThroughput8K, Value: 49.8, Unit: "tokens/sec", Detail: "92 tokens in 1.848s"},
			{Name: probe.FigureTTFT8K, Value: 1.041, Unit: "sec", Detail: "first token after 1.041s"},
			{Name: probe.FigureThroughput32K, AbsentReason: "the 32K throughput case did not answer within 2m0s and was ended."},
			{Name: probe.FigureTTFT32K, AbsentReason: "the 32K throughput case did not answer within 2m0s and was ended."},
		},
	}
}

// A MEASURED ROW AND AN UNMEASURED ROW MUST NOT SCAN ALIKE. A case that
// scored zero and a case that could not run are opposite facts, so a
// measured row carries a number and an unmeasured one carries a dash
// with its reason indented on the line below -- past the value column,
// so it cannot be read as one.
func TestProbeRendersFiguresAndAbsencesDifferently(t *testing.T) {
	code, out := runProbeWith(t, offering(allowedModel("qwen3.5:9b", models.Attributes{})),
		measuredReport(), nil, nil)

	if code != SetupExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	for _, want := range []string{
		"Measuring qwen3.5:9b with probe suite 1.",
		"  structured validity      1.00         5 of 5 schemas held",
		"  tool call correctness    0.67         2 of 3 definitions (missed: search)",
		"  throughput 8K            49.8 tok/s   92 tokens in 1.848s",
		"  time to first token 8K   1.04 s       first token after 1.041s",
		"  throughput 32K           --",
		"      the 32K throughput case did not answer within 2m0s and was ended.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing:\n  %q\ngot:\n%s", want, out)
		}
	}
}

// THE UNITS DIFFER BETWEEN ROWS, so every figure carries its own. A
// bare 49.8 beside a bare 1.04 is two numbers nobody can compare.
func TestProbeRendersAUnitOnEveryFigureThatHasOne(t *testing.T) {
	_, out := runProbeWith(t, offering(allowedModel("qwen3.5:9b", models.Attributes{})),
		measuredReport(), nil, nil)

	if !strings.Contains(out, "tok/s") {
		t.Errorf("a throughput was printed without its unit:\n%s", out)
	}
	if !strings.Contains(out, "1.04 s") {
		t.Errorf("a duration was printed without its unit:\n%s", out)
	}
	// A RATIO HAS NO UNIT AND MUST NOT ACQUIRE ONE. Asserted against
	// what figureValue would actually emit for a ratio wrongly given
	// the tokens/sec unit -- "1.0 tok/s" -- rather than against
	// "1.00 tok", which that branch can never produce and which
	// therefore made this check unfailable.
	if strings.Contains(out, "1.0 tok/s") {
		t.Errorf("a ratio was given a unit:\n%s", out)
	}
	// And the ratio still renders at two decimals, which is what makes
	// a column of them scannable.
	if !strings.Contains(out, "  structured validity      1.00 ") {
		t.Errorf("a ratio did not render at two decimals:\n%s", out)
	}
}

// D4, said to the person. Not decoration: somebody who reads a 0.67 and
// believes the model has been switched off goes looking for a switch
// that does not exist.
func TestProbeSaysTheFiguresGateNothing(t *testing.T) {
	_, out := runProbeWith(t, offering(allowedModel("m", models.Attributes{})), measuredReport(), nil, nil)
	if !strings.Contains(flat(out), "These figures rank this machine; they gate nothing.") {
		t.Errorf("output:\n%s", out)
	}
}

// THE HEADING COMES BEFORE THE WORK. Somebody who interrupts a
// two-minute 32K generation has to read off the screen what they
// interrupted; a line printed only on completion is the line the wedged
// case never prints.
func TestProbeAnnouncesEachCaseBeforeItRuns(t *testing.T) {
	var gotProgress bool
	_, out := runProbeWith(t, offering(allowedModel("m", models.Attributes{})), measuredReport(), nil,
		func(r *probeRun) {
			inner := r.run
			r.run = func(ctx context.Context, req probe.Request) (probe.Report, error) {
				// The command MUST supply a Progress callback -- without
				// one the suite runs silently and somebody watching a
				// two-minute 32K generation has nothing on screen. The
				// previous spelling of this test fabricated the events
				// it then asserted on, so runProbe could have passed nil
				// and still passed.
				if req.Progress == nil {
					t.Error("runProbe passed no Progress callback; the suite would run silently")
					return inner(ctx, req)
				}
				gotProgress = true
				req.Progress(probe.Event{Case: "structured_validity", Index: 1, Total: 4, Started: true})
				req.Progress(probe.Event{Case: "tool_correctness", Index: 2, Total: 4, Started: true})
				// A FINISHED event must print nothing: the heading is
				// the announcement, and printing again on completion
				// would double every line.
				req.Progress(probe.Event{Case: "structured_validity", Index: 1, Total: 4})
				return inner(ctx, req)
			}
		})

	if !gotProgress {
		t.Fatal("the suite did not run")
	}
	for _, want := range []string{"  [1/4] structured_validity", "  [2/4] tool_correctness"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "[1/4] structured_validity"); n != 1 {
		t.Errorf("the case was announced %d times, want once (a finished event must print nothing)", n)
	}
}

// THE VERSION GATE RUNS FIRST, before this machine's models are probed.
// Somebody who typed --suite 9 asked about the SUITE, and answering
// "this machine offers no models" tells them nothing about the number
// they typed and sends them to policy.yaml to fix a list that was never
// the problem.
func TestProbeChecksTheSuiteVersionBeforeAnythingElse(t *testing.T) {
	probed := false
	code, out := runProbeWith(t, models.Inventory{}, probe.Report{}, nil, func(r *probeRun) {
		r.suite = probe.SuiteVersion + 8
		r.inventory = func(context.Context) models.Inventory {
			probed = true
			return models.Inventory{}
		}
	})

	if code != SetupExitUsage {
		t.Fatalf("exit = %d, want %d", code, SetupExitUsage)
	}
	if probed {
		t.Error("this machine's models were probed behind a refused suite version")
	}
	want := "This machine knows probe suite version 1; you asked for version 9. " +
		"Update the cockpit on this machine, or drop --suite."
	if flat(out) != want {
		t.Fatalf("refusal =\n  %q\nwant\n  %q", flat(out), want)
	}
}

// A machine with nothing to measure says WHY, in the same words
// `memql worker models` uses -- one explanation for one fact, rather
// than two surfaces that can disagree about it.
func TestProbeExplainsAnEmptyInventory(t *testing.T) {
	code, out := runProbeWith(t, models.Inventory{
		Floor: models.FloorVerdict{Met: false, Reason: "this machine has 8 GB of unified memory; the floor is 16 GB."},
	}, probe.Report{}, nil, nil)

	if code != SetupExitPrereq {
		t.Fatalf("exit = %d, want %d", code, SetupExitPrereq)
	}
	if !strings.Contains(flat(out), "this machine has 8 GB of unified memory; the floor is 16 GB.") {
		t.Errorf("the floor's own sentence was not used:\n%s", out)
	}
}

// A --model this machine does not offer LISTS WHAT IT DOES, so the
// operator's next command is on the screen rather than in another one.
func TestProbeListsWhatItOffersWhenTheModelIsWrong(t *testing.T) {
	code, out := runProbeWith(t,
		offering(allowedModel("qwen3.5:9b", models.Attributes{}), allowedModel("gemma4:12b", models.Attributes{})),
		probe.Report{}, nil, func(r *probeRun) { r.modelID = "llama3.1:8b" })

	if code != SetupExitPrereq {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(flat(out), "This machine does not offer llama3.1:8b") {
		t.Errorf("output:\n%s", out)
	}
	for _, want := range []string{"  qwen3.5:9b", "  gemma4:12b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// It measures the ADVERTISED set, not everything installed: a model
// this machine will not serve is one no measurement can affect, and
// measuring it would produce a figure the router can never use.
func TestProbeMeasuresOnlyWhatIsAdvertised(t *testing.T) {
	blocked := models.Info{ID: "blocked:9b", Kind: models.KindOllama, Allowed: false}
	code, out := runProbeWith(t, offering(blocked), probe.Report{}, nil,
		func(r *probeRun) { r.modelID = "blocked:9b" })

	if code != SetupExitPrereq {
		t.Fatalf("exit = %d, want a refusal for a blocked model", code)
	}
	if !strings.Contains(flat(out), "This machine does not offer blocked:9b") {
		t.Errorf("output:\n%s", out)
	}
}

// A runtime failure during the run is reported with the runtime's own
// words and an op-failed code, distinct from a usage error.
func TestProbeReportsARunFailure(t *testing.T) {
	code, out := runProbeWith(t, offering(allowedModel("m", models.Attributes{})),
		probe.Report{}, errors.New("connection refused"), nil)

	if code != SetupExitOpFailed {
		t.Fatalf("exit = %d, want %d", code, SetupExitOpFailed)
	}
	if !strings.Contains(flat(out), "Connection refused.") {
		t.Errorf("output:\n%s", out)
	}
}

// The stable figure keys are spelled out for a person. A reader should
// never have to know that `ttft_8k` is a thing.
func TestFigureLabelsAreSpelledOut(t *testing.T) {
	for key, want := range map[string]string{
		probe.FigureStructuredValidity: "structured validity",
		probe.FigureToolCorrectness:    "tool call correctness",
		probe.FigureThroughput8K:       "throughput 8K",
		probe.FigureTTFT8K:             "time to first token 8K",
		probe.FigureThroughput32K:      "throughput 32K",
		probe.FigureTTFT32K:            "time to first token 32K",
	} {
		if got := figureLabel(key); got != want {
			t.Errorf("figureLabel(%q) = %q, want %q", key, got, want)
		}
	}
	// An unknown key falls back to itself rather than to a blank
	// column: a figure a newer engine names is still rendered.
	if got := figureLabel("video_quality"); got != "video_quality" {
		t.Errorf("figureLabel fell back to %q", got)
	}
}

// The longest label fits the column, so no row wraps into the value.
func TestProbeLabelColumnFitsTheLongestLabel(t *testing.T) {
	for _, key := range []string{
		probe.FigureStructuredValidity, probe.FigureToolCorrectness,
		probe.FigureThroughput8K, probe.FigureTTFT8K,
		probe.FigureThroughput32K, probe.FigureTTFT32K,
	} {
		if n := len(figureLabel(key)); n >= probeFigureLabelWidth {
			t.Errorf("%q is %d wide; the column is %d", figureLabel(key), n, probeFigureLabelWidth)
		}
	}
}
