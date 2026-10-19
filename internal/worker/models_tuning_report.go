package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
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
	// Resident is the entries models.resident stands on (policy.ModelsTuning),
	// so the kept section can tell an empty setting from one nothing here serves.
	Resident []models.Resident
	// Loaded is what Ollama holds now; LoadedErr is why it could not say.
	Loaded    []models.Loaded
	LoadedErr error
	// Now is the clock, so the report is assertable.
	Now time.Time
	// LookPath and ManagedOllama are the lookups behind the command the CPU
	// callout prints. Nil means the real ones.
	LookPath      func(file string) (string, error)
	ManagedOllama func() string
}

// tuningRuntime is what the tuning section asks the machine: the per-model
// ceiling and where it came from, and what Ollama holds in memory now.
// *models.Discoverer is the real one.
type tuningRuntime interface {
	ParallelSource(configured int) (int, string)
	Loaded(ctx context.Context) ([]models.Loaded, error)
}

// tuningHost is everything about the machine the section reads, so the section
// is a function of its inputs and assertable without one.
type tuningHost struct {
	Runtime tuningRuntime
	// Now is the clock the expiries are measured against.
	Now time.Time
	// LookPath and ManagedOllama are the lookups behind the command the CPU
	// callout prints; nil means the real ones (tuningReport).
	LookPath      func(file string) (string, error)
	ManagedOllama func() string
}

// printModelsTuning writes the tuning half of `memql worker models`, after the
// inventory, and reports whether it wrote anything.
//
// It answers "did it take". It is printed when Ollama answered with at least
// one model (its version is read only then), when models.resident has an entry,
// or when the policy has a problem. Otherwise there is nothing to tune, and the
// inventory above says why. A closed gate asks the machine nothing: a command
// with no Ollama pays no second round trip for a section it will not print.
//
// This is the one place the section's inputs are gathered. Each is handed to
// the printer by name, and TestModelsTuningSection_EveryInputReachesThePrinter
// holds them to it: a dropped one leaves every test of the printer green and
// gives an owner who configured something the report of one who did not.
func printModelsTuning(ctx context.Context, w io.Writer, host tuningHost, inv models.Inventory, policy *tools.Policy) bool {
	tuning := policy.ModelsTuning()
	problems := policy.ModelsTuningProblems()
	if inv.RuntimeVersions[models.KindOllama] == "" && len(tuning.Resident) == 0 && len(problems) == 0 {
		return false
	}
	n, source := host.Runtime.ParallelSource(tuning.Parallel)
	loaded, loadedErr := host.Runtime.Loaded(ctx)
	printModelTuning(w, inv, tuningReport{
		Parallel: n, ParallelSource: source, Problems: problems, Resident: tuning.Resident,
		Loaded: loaded, LoadedErr: loadedErr, Now: host.Now,
		LookPath: host.LookPath, ManagedOllama: host.ManagedOllama,
	})
	return true
}

// managedOllamaBinary is the ollama the managed runtime unpacked, at the path
// its unit runs it from, or empty when none is on disk. The path comes from the
// facts the install writes its unit against, so the command the report prints
// is the binary the unit runs.
func managedOllamaBinary() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	bin := filepath.Join(inference.DefaultNativeFacts(home, "").RuntimeDir, "bin", "ollama")
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return bin
}

// stopCommand is the `ollama` a person runs to unload a model, written as one
// word for their shell: the bare name when ollama is on PATH. Otherwise the
// managed runtime's binary by its absolute path when it is installed, because
// the runtime is not put on PATH and its unit runs that binary by path. The path
// runs through the owner's home, so it is quoted when it holds a space or a
// character the shell reads (shellWord). The bare name when neither is found.
func stopCommand(r tuningReport) string {
	lookPath := r.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("ollama"); err == nil {
		return "ollama"
	}
	managed := r.ManagedOllama
	if managed == nil {
		managed = managedOllamaBinary
	}
	if bin := managed(); bin != "" {
		return shellWord(bin)
	}
	return "ollama"
}

// shellWord is s written as one word to a POSIX shell: bare when every
// character is one the shell leaves alone, otherwise inside single quotes, with
// each single quote in it closed out, escaped with a backslash and reopened.
// The report prints commands to paste, and a path with a space or a
// metacharacter in it is otherwise two words, or a command.
func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return !shellSafe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSafe is a character that needs no quoting anywhere in a word: letters,
// digits and _ @ % + : , . / -  (not = or ~, which a shell reads at the start of
// a word).
func shellSafe(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("_@%+:,./-", r)
}

func printModelTuning(w io.Writer, inv models.Inventory, r tuningReport) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Residency and concurrency")
	fmt.Fprintln(w, "")
	// Ollama models only: a model on a runtime declared under models.runtimes
	// takes the max_concurrent its declaration states, not this figure.
	fmt.Fprintf(w, "  Requests per Ollama model: %d, from %s.\n", r.Parallel, r.ParallelSource)
	fmt.Fprintln(w, "")

	// Only what this machine serves: Advertised is what it offers, which is
	// allowed by models.allow and only above the hardware floor. An entry for
	// any other model is inert -- no call is served for it, so nothing keeps it
	// loaded.
	var kept []models.Info
	for _, m := range inv.Advertised() {
		if m.KeepAlive.Set || m.WorkingContext > 0 {
			kept = append(kept, m)
		}
	}
	switch {
	case len(kept) > 0:
		fmt.Fprintln(w, "  Kept loaded (policy.yaml models.resident):")
		for _, m := range kept {
			var parts []string
			if m.KeepAlive.Set {
				parts = append(parts, "kept loaded "+keepAliveWords(m.KeepAlive))
			}
			if m.WorkingContext > 0 {
				parts = append(parts, fmt.Sprintf("at a working context of %d", m.WorkingContext))
			}
			if !m.KeepAlive.Set {
				// The window is held and the model is not: with no keep_alive it
				// still unloads when Ollama's own timeout says (five minutes unless
				// OLLAMA_KEEP_ALIVE changed it). Under a heading that says "kept
				// loaded" the line has to say so, or it reads as a promise.
				parts = append(parts, "but not kept loaded: with no keep_alive, by default Ollama unloads it five minutes after its last call")
			}
			fmt.Fprintf(w, "    %-30s %s\n", m.ID, strings.Join(parts, ", "))
		}
	case len(r.Resident) > 0:
		// An entry was written and this machine serves none of them. Its reason
		// is printed above (the floor, the notes) or below (the problems line).
		fmt.Fprintln(w, "  Kept loaded: none of the models.resident entries is served by this machine; the reasons are above and below.")
	default:
		// "By default": five minutes is Ollama's own timeout, and a drop-in that
		// sets OLLAMA_KEEP_ALIVE changes it.
		fmt.Fprintln(w, "  Kept loaded: nothing is configured to stay loaded. By default, Ollama unloads")
		fmt.Fprintln(w, "  each model five minutes after its last call. Add models.resident to")
		fmt.Fprintln(w, "  policy.yaml to change that, then send the worker a SIGHUP.")
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
			// Only a size Ollama stated can say how much of the model is on the
			// GPU. Without one the share reads zero, and "the rest runs on the
			// CPU" would be said of a model nobody measured.
			//
			// KNOWN GAP: a machine with no GPU at all reads 0% on every model, and
			// this has no structured way to know there is none -- the hardware
			// floor's verdict carries it only in a sentence. Such a machine is
			// below the floor and serves nothing.
			if l.SizeBytes > 0 && l.GPUPercent() < 100 {
				fmt.Fprintln(w, "      The rest runs on the CPU, which is slow, and stays that way until the model reloads.")
				fmt.Fprintf(w, "      Free GPU memory, then run: %s stop %s\n", stopCommand(r), shellWord(l.ID))
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
	parts := []string{"size not reported"}
	if l.SizeBytes > 0 {
		parts = []string{
			fmt.Sprintf("%.1f GB", float64(l.SizeBytes)/1e9),
			fmt.Sprintf("%d%% on the GPU", l.GPUPercent()),
		}
	}
	if l.ContextLength > 0 {
		parts = append(parts, fmt.Sprintf("window %d", l.ContextLength))
	}
	switch {
	case l.ExpiresAt.IsZero():
	case l.Forever(now):
		parts = append(parts, "stays loaded")
	case !l.ExpiresAt.After(now):
		// At or before now: it is going, or Ollama has not got round to it.
		parts = append(parts, "unloads now")
	default:
		parts = append(parts, "unloads in "+untilWords(l.ExpiresAt.Sub(now)))
	}
	text := strings.Join(parts, ", ")
	// "35% on the GPU" is a sentence about a fault; the pieces read as a
	// list, so the fault gets its own line (printModelTuning).
	return text
}

// untilWords renders a wait the way an owner thinks of one: hours and
// minutes, or seconds when it is nearly up. d is positive: a wait that has run
// out is not a wait, and the caller says "now" for it.
func untilWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		h := int(d.Hours())
		return fmt.Sprintf("%dh%dm", h, int(d.Minutes())-h*60)
	}
}
