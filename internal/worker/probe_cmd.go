package worker

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/probe"
)

// `memql worker probe` -- measure a local model against the suite.
//
// The engine runs this over the stream when ModelProbeStart exists
// (memql#5146). This is the same suite, reachable by hand, and it is
// not a stopgap: an operator deciding whether a model is worth keeping
// on their machine is asking exactly what the suite measures, and they
// should not have to wait for a cluster to ask on their behalf.
//
// THE FIGURE COLUMN AND THE REASON COLUMN ARE NOT THE SAME COLUMN. A
// case that scored 0.0 and a case that could not run are opposite
// facts, so a measured row shows a number where an unmeasured row shows
// a dash and hangs its sentence on the line below. Two rows that scan
// alike would be read alike, and the whole point of the absent reason
// is that it says nothing about the model.

const (
	// probeFigureLabelWidth aligns the figure names. The longest is
	// "time to first token 32K" at 23.
	probeFigureLabelWidth = 25
	// probeValueWidth aligns the numbers so a column of them can be
	// compared down the page rather than read one at a time. Wide
	// enough for the value AND its unit: the units DIFFER between rows
	// here -- a ratio, a rate and a duration -- so a bare 36.4 beside a
	// bare 1.04 is two numbers nobody can compare.
	probeValueWidth = 13
)

type probeRun struct {
	out io.Writer

	modelID string
	suite   int

	policyPath string
	inventory  func(ctx context.Context) models.Inventory
	client     func(models.Info) modelcall.Client
	run        func(context.Context, probe.Request) (probe.Report, error)
}

func newProbeRun(configPath, modelID string, suite int) *probeRun {
	discoverer := newDiscoverer()
	policyPath := filepath.Join(filepath.Dir(configPath), "policy.yaml")
	return &probeRun{
		out:        os.Stdout,
		modelID:    modelID,
		suite:      suite,
		policyPath: policyPath,
		inventory:  func(ctx context.Context) models.Inventory { return probeWithPolicy(ctx, discoverer, policyPath) },
		client: func(info models.Info) modelcall.Client {
			return modelcall.NewClient(info, &http.Client{}, os.Getenv)
		},
		run: probe.Run,
	}
}

func handleProbe(args []string) {
	fs := flag.NewFlagSet("worker probe", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml (its directory holds policy.yaml)")
	modelID := fs.String("model", "", "model id to measure (default: the first model this machine offers)")
	suite := fs.Int("suite", probe.SuiteVersion, "probe suite version to run")
	_ = fs.Parse(args)

	os.Exit(runProbe(context.Background(), newProbeRun(*configPath, *modelID, *suite)))
}

func runProbe(ctx context.Context, r *probeRun) int {
	// THE SUITE VERSION IS CHECKED FIRST, before this machine's models
	// are even probed.
	//
	// The order is the point: an operator who typed --suite 9 asked a
	// question about the SUITE, and answering "this machine offers no
	// models" tells them nothing about the number they typed and sends
	// them to policy.yaml to fix a model list that was never the
	// problem. It also keeps the refusal reachable on a machine with
	// nothing pulled, which is where somebody is most likely to be
	// experimenting with the flag.
	if r.suite != probe.SuiteVersion {
		r.paragraph(capitalise(fmt.Sprintf(
			"this machine knows probe suite version %d; you asked for version %d. "+
				"Update the cockpit on this machine, or drop --suite.",
			probe.SuiteVersion, r.suite)))
		return SetupExitUsage
	}

	inv := r.inventory(ctx)

	info, ok := r.pick(inv)
	if !ok {
		return SetupExitPrereq
	}

	r.line(fmt.Sprintf("Measuring %s with probe suite %d.", info.ID, r.suite))
	r.line("")

	report, err := r.run(ctx, probe.Request{
		SuiteVersion: r.suite,
		Model:        info.ID,
		Client:       r.client(info),
		Progress:     r.progress,
	})
	if err != nil {
		r.line("")
		// The suite version was already checked above, so this arm is
		// for a Run that refuses a version for some reason this command
		// did not anticipate -- kept rather than dropped because
		// probe.Run owns that gate and this command must not assume it
		// is the only one.
		if errors.Is(err, probe.ErrUnknownSuite) {
			r.paragraph(capitalise(strings.TrimPrefix(err.Error(), "probe: unknown suite version: ")))
			return SetupExitUsage
		}
		r.paragraph(capitalise(err.Error()) + ".")
		return SetupExitOpFailed
	}

	r.line("")
	r.figures(report)
	r.line("")

	// D4, said to the person. Not decoration: somebody who reads a 0.67
	// and believes the model has been switched off will go looking for
	// a switch that does not exist.
	r.paragraph("These figures rank this machine; they gate nothing. A model that fails a" +
		" case is still offered for every call it advertises.")
	return 0
}

// pick chooses the model to measure, or says why it cannot.
//
// It reads the ADVERTISED set rather than everything installed: a model
// this machine will not serve is one no measurement can affect, and
// measuring it would produce a figure the router can never use.
func (r *probeRun) pick(inv models.Inventory) (models.Info, bool) {
	advertised := inv.Advertised()

	if id := strings.TrimSpace(r.modelID); id != "" {
		for _, m := range advertised {
			if m.ID == id {
				return m, true
			}
		}
		r.paragraph(fmt.Sprintf("This machine does not offer %s, so there is nothing to measure.", id))
		r.line("")
		if len(advertised) == 0 {
			r.paragraph(whyNothing(inv, r.policyPath))
			return models.Info{}, false
		}
		r.line("It offers:")
		for _, m := range advertised {
			r.line("  " + m.ID)
		}
		return models.Info{}, false
	}

	if len(advertised) == 0 {
		r.paragraph("This machine offers no models, so there is nothing to measure. " +
			whyNothing(inv, r.policyPath))
		return models.Info{}, false
	}
	return advertised[0], true
}

// progress prints the heading BEFORE the work, which is this surface's
// rule everywhere: somebody who interrupts a two-minute 32K generation
// has to be able to read off the screen what they interrupted.
func (r *probeRun) progress(e probe.Event) {
	if !e.Started {
		return
	}
	fmt.Fprintf(r.out, "  [%d/%d] %s\n", e.Index, e.Total, e.Case)
}

// figures renders the report.
func (r *probeRun) figures(report probe.Report) {
	for _, f := range report.Figures {
		label := figureLabel(f.Name)
		if !f.Measured() {
			// A DASH, not a zero, and the reason on its own line
			// underneath. The indent puts the sentence past the value
			// column so it cannot be mistaken for one.
			fmt.Fprintf(r.out, "  %-*s%s\n", probeFigureLabelWidth, label, "--")
			for _, line := range wrapText(f.AbsentReason, inferenceWrapWidth-6) {
				fmt.Fprintf(r.out, "      %s\n", line)
			}
			continue
		}
		fmt.Fprintf(r.out, "  %-*s%-*s%s\n",
			probeFigureLabelWidth, label, probeValueWidth, figureValue(f), f.Detail)
	}
}

// figureValue formats a number at the precision its unit deserves. A
// ratio to two decimals, a rate to one, a duration to two -- rendering
// every figure the same way would print "0.67 tokens/sec" style noise
// on one row and lose the difference between 41.2 and 41.24 on another.
func figureValue(f probe.Figure) string {
	switch f.Unit {
	case "":
		// A ratio between 0 and 1, and the two decimals are what make
		// 0.80 and 1.00 the same width -- a column of ratios is read by
		// scanning it, and a ragged one is read row by row.
		return fmt.Sprintf("%.2f", f.Value)
	case "sec":
		return fmt.Sprintf("%.2f s", f.Value)
	case "tokens/sec":
		// "tok/s" rather than "tokens/sec": the full spelling pushes
		// the detail column past 80 characters over SSH, which is where
		// this is actually read.
		return fmt.Sprintf("%.1f tok/s", f.Value)
	default:
		return fmt.Sprintf("%.1f %s", f.Value, f.Unit)
	}
}

// figureLabel spells a stable key out for a person. The keys are the
// engine's and never change; this is the only place they are rendered,
// so a reader never has to know that `ttft_8k` is a thing.
func figureLabel(name string) string {
	switch name {
	case probe.FigureStructuredValidity:
		return "structured validity"
	case probe.FigureToolCorrectness:
		return "tool call correctness"
	case probe.FigureThroughput8K:
		return "throughput 8K"
	case probe.FigureTTFT8K:
		return "time to first token 8K"
	case probe.FigureThroughput32K:
		return "throughput 32K"
	case probe.FigureTTFT32K:
		return "time to first token 32K"
	default:
		return name
	}
}

func (r *probeRun) line(text string) { fmt.Fprintln(r.out, text) }

func (r *probeRun) paragraph(text string) {
	for _, l := range wrapText(text, inferenceWrapWidth) {
		fmt.Fprintln(r.out, l)
	}
}
