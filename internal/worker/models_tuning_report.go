package worker

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// The tuning half of `memql worker models`: what this machine will tell
// Ollama about keeping models loaded and how many requests to take at once,
// and -- the half that matters -- what Ollama actually holds now. A setting
// is an intention until the runtime shows it kept, and a resident model
// that was loaded while the card was busy is the failure residency makes
// permanent, so the report asks.
//
// Design record: docs/superpowers/specs/2026-10-18-fleet-tuning-design.md.

// tuningReport is everything printModelTuning prints beyond the inventory.
type tuningReport struct {
	// Parallel and ParallelSource are the per-model ceiling and where it
	// came from (models.Discoverer.ParallelSource).
	Parallel       int
	ParallelSource string
	// Problems are policy.ModelsTuningProblems.
	Problems []string
	// Loaded is what Ollama holds now; LoadedErr is why it could not say.
	Loaded    []models.Loaded
	LoadedErr error
	// Now is the clock, so the report is assertable.
	Now time.Time
}

func printModelTuning(w io.Writer, inv models.Inventory, r tuningReport) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Residency and concurrency")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "  Requests per model: %d, from %s.\n", r.Parallel, r.ParallelSource)
	fmt.Fprintln(w, "")

	var kept []models.Info
	for _, m := range inv.Models {
		if m.KeepAlive.Set || m.WorkingContext > 0 {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		fmt.Fprintln(w, "  Kept loaded: nothing is configured to stay loaded. Ollama unloads each model")
		fmt.Fprintln(w, "  five minutes after its last call. Add models.resident to policy.yaml to change")
		fmt.Fprintln(w, "  that, then send the worker a SIGHUP.")
	} else {
		fmt.Fprintln(w, "  Kept loaded (policy.yaml models.resident):")
		for _, m := range kept {
			var parts []string
			if m.KeepAlive.Set {
				parts = append(parts, "kept loaded "+keepAliveWords(m.KeepAlive))
			}
			if m.WorkingContext > 0 {
				parts = append(parts, fmt.Sprintf("at a working context of %d", m.WorkingContext))
			}
			fmt.Fprintf(w, "    %-30s %s\n", m.ID, strings.Join(parts, ", "))
		}
	}
	fmt.Fprintln(w, "")

	switch {
	case r.LoadedErr != nil:
		fmt.Fprintf(w, "  Could not read what Ollama has loaded: %v\n", r.LoadedErr)
	case len(r.Loaded) == 0:
		fmt.Fprintln(w, "  Ollama has nothing loaded now. The next call loads a model, which takes a few")
		fmt.Fprintln(w, "  seconds from a warm disk cache and up to forty from a cold one.")
	default:
		fmt.Fprintln(w, "  Ollama has loaded now:")
		for _, l := range r.Loaded {
			fmt.Fprintf(w, "    %-30s %s\n", l.ID, loadedWords(l, r.Now))
			if l.GPUPercent() < 100 {
				fmt.Fprintln(w, "      The rest runs on the CPU, which is slow, and stays that way until the model reloads.")
				fmt.Fprintf(w, "      Free GPU memory, then run: ollama stop %s\n", l.ID)
			}
		}
	}

	if len(r.Problems) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  Problems with models.parallel and models.resident:")
		for _, p := range r.Problems {
			fmt.Fprintf(w, "    - %s\n", p)
		}
	}
}

func keepAliveWords(k models.KeepAlive) string {
	if k.Forever {
		return "forever"
	}
	return "for " + k.For.String()
}

// loadedWords is one line of what Ollama holds: size, how much of it is on
// the GPU, the window it was loaded at, and when it goes.
func loadedWords(l models.Loaded, now time.Time) string {
	parts := []string{
		fmt.Sprintf("%.1f GB", float64(l.SizeBytes)/1e9),
		fmt.Sprintf("%d%% on the GPU", l.GPUPercent()),
	}
	if l.ContextLength > 0 {
		parts = append(parts, fmt.Sprintf("window %d", l.ContextLength))
	}
	switch {
	case l.ExpiresAt.IsZero():
	case l.Forever(now):
		parts = append(parts, "stays loaded")
	default:
		parts = append(parts, "unloads in "+untilWords(l.ExpiresAt.Sub(now)))
	}
	text := strings.Join(parts, ", ")
	// "35% on the GPU" is a sentence about a fault; the pieces read as a
	// list, so the fault gets its own line (printModelTuning).
	return text
}

// untilWords renders a wait the way an owner thinks of one: hours and
// minutes, or seconds when it is nearly up.
func untilWords(d time.Duration) string {
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		return fmt.Sprintf("%dh%dm", h, int(d.Minutes())-h*60)
	}
}
