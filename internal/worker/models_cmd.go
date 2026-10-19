package worker

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// `memql worker models` (memql-cockpit#363).
//
// It answers ONE question, and the question is the reason the command
// exists: *why is my laptop not in the model list?* From the portal that
// has five plausible answers -- below the hardware floor, no runtime
// installed, not in models.allow, not signed in, simply asleep -- and all
// five render identically, as an absence. Only the machine can tell them
// apart, so the answer is printed here.
//
// It runs the SAME discovery the worker runs, so what it prints is what
// the cluster would be told. A second implementation could disagree with
// the first, and the operator would have no way to know which one lied.

const modelsProbeTimeout = 15 * time.Second

func handleModels(args []string) {
	fs := flag.NewFlagSet("worker models", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to worker.yaml")
	var pull, allow repeatedFlag
	fs.Var(&pull, "pull", "pull this model id into the local runtime; repeatable")
	fs.Var(&allow, "allow", "add this model id to models.allow in policy.yaml; repeatable")
	_ = fs.Parse(args)

	policyPath := filepath.Join(filepath.Dir(*configPath), "policy.yaml")
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// THE ACTS RUN BEFORE THE REPORT, and the report is then the state
	// they left behind rather than the one they found. A command that
	// pulled a model and printed an inventory probed a second earlier
	// would tell the operator their pull did nothing.
	//
	// The probe below is deliberately NOT given the timeout a pull runs
	// under: a multi-gigabyte pull is minutes to hours of legitimate
	// work, and the caller's own interrupt is the only sensible deadline
	// for it (inference.Pull carries no client timeout for the same
	// reason).
	if len(pull) > 0 || len(allow) > 0 {
		s := newInferenceSetup(*configPath, nil, false, "")
		if code := runModelActs(context.Background(), s, cleanModelIDs(pull), cleanModelIDs(allow)); code != SetupExitOK {
			os.Exit(code)
		}
		// models.allow may have just changed, and the report has to
		// read what is on disk now.
		if reloaded, err := tools.LoadPolicy(policyPath); err == nil {
			policy = reloaded
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsProbeTimeout)
	defer cancel()

	// Probe rather than Discover: an operator below the floor still wants
	// to see that they have three models pulled, because "you have models
	// but this machine cannot serve them" and "you have no models" send
	// them to entirely different places.
	d := newDiscoverer()
	// This runs in the owner's shell and the worker does not: the ceiling it
	// prints is the one the worker's own environment gives, not the shell's.
	followWorkerEnvironment(d, *configPath)
	inv := d.Probe(ctx, modelsRequest(policy))
	printModelInventory(os.Stdout, inv, policyPath)
	printModelsTuning(ctx, os.Stdout, tuningHost{Runtime: d, Now: time.Now()}, inv, policy)
}

// printModelInventory writes the report. Split out so the shape is
// assertable without a machine that has Ollama on it.
func printModelInventory(w io.Writer, inv models.Inventory, policyPath string) {
	fmt.Fprintln(w, "Local models on this machine")
	fmt.Fprintln(w, "")

	fmt.Fprintf(w, "Hardware floor: %s\n", floorLine(inv.Floor))
	if inv.Floor.Detail != "" {
		fmt.Fprintf(w, "                %s\n", inv.Floor.Detail)
	}
	fmt.Fprintln(w, "")

	if len(inv.Models) == 0 {
		fmt.Fprintln(w, "Runtimes: none found.")
	} else {
		fmt.Fprintln(w, "Models found:")
		for _, m := range inv.Models {
			fmt.Fprintf(w, "  %-32s %s\n", m.ID, modelVerdict(m, inv.Floor))
			fmt.Fprintf(w, "  %-32s runtime %s (%s) at %s\n", "", m.Runtime, m.Kind, m.BaseURL)
			fmt.Fprintf(w, "  %-32s %s\n", "", attributeLine(m.Attributes))
		}
	}

	if len(inv.ProbeNotes) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Notes:")
		for _, n := range inv.ProbeNotes {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}

	advertised := inv.Advertised()
	fmt.Fprintln(w, "")
	if len(advertised) == 0 {
		fmt.Fprintf(w, "This machine offers no models. %s\n", whyNothing(inv, policyPath))
		return
	}

	fmt.Fprintf(w, "This machine offers %d model(s). Registration labels:\n", len(advertised))
	labels := inv.Labels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := labels[k]; v != "" {
			fmt.Fprintf(w, "  %s=%s\n", k, v)
		} else {
			fmt.Fprintf(w, "  %s\n", k)
		}
	}
	fmt.Fprintf(w, "  capability %s, machine concurrency %d\n",
		models.Capability, modelRegistrationFor(inv).Concurrency)
}

func floorLine(v models.FloorVerdict) string {
	if v.Met {
		return "met"
	}
	return "NOT met -- " + v.Reason
}

func modelVerdict(m models.Info, floor models.FloorVerdict) string {
	switch {
	case !floor.Met:
		return "not offered (machine is below the hardware floor)"
	case !m.Allowed:
		return "present, BLOCKED (not in policy.yaml models.allow)"
	default:
		return "offered"
	}
}

// attributeLine renders what would be claimed, naming the ABSENCES too.
// A capability this machine does not advertise is why a model is passed
// over for a structured prompt, and "structured output: not advertised"
// is the sentence that explains a machine which is in the catalog and
// still never picked.
//
// IT ENUMERATES THE ATTRIBUTES BY HAND, and the list therefore has to be
// kept level with models.Attributes -- this file's own promise is that
// what it prints is what the cluster would be told, and it shipped for
// one release without params, quant or tools, which are exactly the
// three the engine ranks and routes on. The failure is silent in both
// directions: the operator reads a complete-looking line and concludes
// their machine claims nothing about size, while the label beside it
// claims a size all along.
//
// Size and quantization lead, because they are what an operator matches
// against `ollama list` -- the parameter COUNT rides the label for the
// engine's numeric ranking, and this is the spelling a person compares.
func attributeLine(a models.Attributes) string {
	parts := []string{}
	if a.Params > 0 {
		parts = append(parts, humanParams(a.Params))
	} else {
		parts = append(parts, "size not advertised")
	}
	if q := strings.TrimSpace(a.Quant); q != "" {
		parts = append(parts, q)
	} else {
		parts = append(parts, "quantization not advertised")
	}
	if a.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%d context", a.ContextWindow))
	} else {
		parts = append(parts, "context not advertised")
	}
	if a.Embeddings {
		parts = append(parts, "embeddings")
	}
	if a.Tools {
		parts = append(parts, "tools")
	} else {
		parts = append(parts, "tools: not advertised")
	}
	if a.StructuredOutput {
		parts = append(parts, "structured output")
	} else {
		parts = append(parts, "structured output: not advertised")
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("max %d concurrent", a.MaxConcurrent))
	}
	// The four modalities, listed only when CLAIMED.
	//
	// Unlike tools and structured output above, an absent modality is
	// NOT spelled out as "not advertised". Those two are asked about on
	// every model, so their absence is news; a chat model that does not
	// generate images is the ordinary case, and four "not advertised"
	// clauses on every row would bury the two that matter under noise
	// nobody reads. What is missing is legible from what is present.
	if m := modalityWords(a); len(m) > 0 {
		parts = append(parts, strings.Join(m, ", "))
	}
	return strings.Join(parts, ", ")
}

// modalityWords names the modalities a model claims, in the label's own
// order so the line and the label read the same way round.
func modalityWords(a models.Attributes) []string {
	var out []string
	if a.Vision {
		out = append(out, "vision")
	}
	if a.AudioIn {
		out = append(out, "transcription")
	}
	if a.AudioOut {
		out = append(out, "speech")
	}
	if a.ImageGen {
		out = append(out, "image generation")
	}
	return out
}

// whyNothing names the actual reason rather than leaving the operator to
// infer one, which is the whole point of the command.
func whyNothing(inv models.Inventory, policyPath string) string {
	if !inv.Floor.Met {
		return inv.Floor.Reason
	}
	if len(inv.Models) == 0 {
		// The fix is now ONE COMMAND rather than a shopping list. It
		// installs the runtime this platform can actually serve from,
		// pulls a model, and writes models.allow -- which is the
		// sequence an operator following the old sentence got wrong
		// most often, by installing Ollama and stopping there.
		return "No model runtime answered. Run `memql worker setup --inference` to install one and pull a model, or declare an OpenAI-compatible endpoint under models.runtimes in " + policyPath + "."
	}
	return "Every model found is blocked. Add the ones this machine should serve to models.allow in " + policyPath + ", then send the worker a SIGHUP."
}
