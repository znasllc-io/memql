package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

var reportNow = time.Date(2026, 10, 18, 12, 30, 0, 0, time.UTC)

func renderTuning(t *testing.T, inv models.Inventory, r tuningReport) string {
	t.Helper()
	if r.Now.IsZero() {
		r.Now = reportNow
	}
	var b strings.Builder
	printModelTuning(&b, inv, r)
	return b.String()
}

func tunedInventory() models.Inventory {
	chat := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	chat.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	chat.WorkingContext = 16384
	embed := offeredModel("qwen3-embedding:0.6b", models.Attributes{Embeddings: true, MaxConcurrent: 2})
	embed.KeepAlive = models.KeepAlive{Set: true, Forever: true}
	plain := offeredModel("other:7b", models.Attributes{MaxConcurrent: 2})
	return servingInventory(chat, embed, plain)
}

// TestModelsTuningReport_SaysWhatIsConfigured. The command exists to
// answer "did it take", and the first half of that is what the cockpit
// will tell Ollama.
func TestModelsTuningReport_SaysWhatIsConfigured(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the memql-ollama.service unit"})
	for _, want := range []string{
		"Requests per model: 2, from the memql-ollama.service unit.",
		"qwen3.8:27b",
		"kept loaded for 2h0m0s, at a working context of 16384",
		"qwen3-embedding:0.6b",
		"kept loaded forever",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	// A model with no entry is not listed as configured.
	if strings.Contains(out, "other:7b") {
		t.Errorf("a model with no models.resident entry was listed:\n%s", out)
	}
}

func TestModelsTuningReport_NothingConfiguredSaysWhatOllamaDoesInstead(t *testing.T) {
	inv := servingInventory(offeredModel("llama3.1:8b", models.Attributes{MaxConcurrent: 1}))
	out := renderTuning(t, inv, tuningReport{Parallel: 1, ParallelSource: "the default"})
	for _, want := range []string{
		"nothing is configured to stay loaded",
		"five minutes after its last call",
		"models.resident",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// TestModelsTuningReport_SaysWhatOllamaHoldsNow is the second half: the
// settings are only intentions until the runtime shows them kept.
func TestModelsTuningReport_SaysWhatOllamaHoldsNow(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		Loaded: []models.Loaded{
			{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 18_400_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(118 * time.Minute)},
			{ID: "qwen3-embedding:0.6b", SizeBytes: 1_400_000_000, VRAMBytes: 1_400_000_000, ContextLength: 8192, ExpiresAt: reportNow.AddDate(300, 0, 0)},
		},
	})
	for _, want := range []string{
		"Ollama has loaded now:",
		"qwen3.8:27b",
		"18.4 GB, 100% on the GPU, window 16384, unloads in 1h58m",
		"1.4 GB, 100% on the GPU, window 8192, stays loaded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// TestModelsTuningReport_AModelPartlyOnTheCPUIsCalledOut. A resident model
// loaded while the card was busy is the failure residency makes
// permanent, and the line has to say what to do about it.
func TestModelsTuningReport_AModelPartlyOnTheCPUIsCalledOut(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		Loaded: []models.Loaded{
			{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 6_500_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)},
		},
	})
	for _, want := range []string{
		"35% on the GPU",
		"The rest runs on the CPU, which is slow, and stays that way until the model reloads.",
		"ollama stop qwen3.8:27b",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

func TestModelsTuningReport_NothingLoadedAndNothingReadable(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the default"})
	if !strings.Contains(out, "Ollama has nothing loaded now") {
		t.Errorf("an empty runtime must read as empty:\n%s", out)
	}
	out = renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		LoadedErr: errors.New("no answer from Ollama's /api/ps at http://127.0.0.1:11434 (status 404)"),
	})
	// Review focus: an unreadable runtime is a line in the report, never
	// an error that stops the command from printing the rest.
	if !strings.Contains(out, "Could not read what Ollama has loaded: no answer from Ollama's /api/ps") {
		t.Errorf("the reason must be printed:\n%s", out)
	}
	if !strings.Contains(out, "Requests per model: 2") {
		t.Errorf("the rest of the report must survive an unreadable runtime:\n%s", out)
	}
}

func TestModelsTuningReport_PrintsTheProblems(t *testing.T) {
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 1, ParallelSource: "the default",
		Problems: []string{"models.parallel: 500 is outside 1 to 64 and is ignored"},
	})
	if !strings.Contains(out, "Problems with models.parallel and models.resident:") ||
		!strings.Contains(out, "- models.parallel: 500 is outside 1 to 64 and is ignored") {
		t.Errorf("problems are not printed:\n%s", out)
	}
}

// keptSection is the kept-loaded list: the lines under its heading, up to the
// blank line that ends it.
func keptSection(out string) string {
	_, rest, found := strings.Cut(out, "Kept loaded (policy.yaml models.resident):\n")
	if !found {
		return ""
	}
	section, _, _ := strings.Cut(rest, "\n\n")
	return section
}

// TestModelsTuningReport_KeptLoadedListsOnlyWhatIsServed. An entry for a model
// models.allow does not list is inert: this machine serves it no call, so
// nothing keeps it loaded. The problems line already says so, and the kept
// list must not claim the opposite.
func TestModelsTuningReport_KeptLoadedListsOnlyWhatIsServed(t *testing.T) {
	p := tuningPolicy(t, "models:\n  allow:\n    - qwen3-embedding:0.6b\n  resident:\n    - id: qwen3.8:27b\n      keep_alive: 2h\n    - id: qwen3-embedding:0.6b\n      keep_alive: -1\n")
	blocked := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	blocked.Allowed = false
	blocked.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	served := offeredModel("qwen3-embedding:0.6b", models.Attributes{Embeddings: true, MaxConcurrent: 2})
	served.KeepAlive = models.KeepAlive{Set: true, Forever: true}
	out := renderTuning(t, servingInventory(blocked, served), tuningReport{
		Parallel: 2, ParallelSource: "the default", Problems: p.ModelsTuningProblems(),
	})
	kept := keptSection(out)
	if strings.Contains(kept, "qwen3.8:27b") {
		t.Errorf("a model models.allow does not list is under the kept-loaded heading:\n%s", out)
	}
	if !strings.Contains(kept, "qwen3-embedding:0.6b") {
		t.Errorf("a served model is missing from the kept-loaded list:\n%s", out)
	}
	if !strings.Contains(out, "models.resident names qwen3.8:27b, which models.allow does not list") {
		t.Errorf("the problems line must still name the blocked model:\n%s", out)
	}
}

// calloutLine is the line the CPU callout prints under a model that is mostly
// on the CPU, trimmed, so the command it names can be compared exactly.
func calloutLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Free GPU memory, then run:") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// TestModelsTuningReport_CPUCalloutNamesACommandThatRuns. The command a person
// is told to run has to run as typed. With ollama on PATH it is the bare name.
// Without it, the managed runtime's binary is named by its absolute path,
// since the unit runs that binary and nothing else puts it on PATH. With
// neither, the bare name, as before.
func TestModelsTuningReport_CPUCalloutNamesACommandThatRuns(t *testing.T) {
	const managed = "/home/op/.memql/ollama/runtime/bin/ollama"
	partly := []models.Loaded{{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 6_500_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)}}
	onPath := func(string) (string, error) { return "/usr/local/bin/ollama", nil }
	offPath := func(string) (string, error) {
		return "", errors.New(`exec: "ollama": executable file not found in $PATH`)
	}
	cases := []struct {
		name     string
		lookPath func(string) (string, error)
		managed  string
		want     string
	}{
		{"on PATH", onPath, managed, "Free GPU memory, then run: ollama stop qwen3.8:27b"},
		{"not on PATH, managed runtime installed", offPath, managed, "Free GPU memory, then run: " + managed + " stop qwen3.8:27b"},
		{"not on PATH, no managed runtime", offPath, "", "Free GPU memory, then run: ollama stop qwen3.8:27b"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			out := renderTuning(t, tunedInventory(), tuningReport{
				Parallel: 2, ParallelSource: "the default", Loaded: partly,
				LookPath:      tt.lookPath,
				ManagedOllama: func() string { return tt.managed },
			})
			if got := calloutLine(out); got != tt.want {
				t.Errorf("the callout says %q, want %q:\n%s", got, tt.want, out)
			}
		})
	}
}

// TestManagedOllamaBinary_IsWhereTheRuntimeInstallsIt. The path the callout
// prints is the one the runtime's facts give its binary, so it is asked of the
// same construction the Stage uses, and it is empty until the binary exists.
func TestManagedOllamaBinary_IsWhereTheRuntimeInstallsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := managedOllamaBinary(); got != "" {
		t.Fatalf("no runtime installed, but managedOllamaBinary = %q", got)
	}
	bin := filepath.Join(inference.DefaultNativeFacts(home, "").RuntimeDir, "bin", "ollama")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := managedOllamaBinary(); got != bin {
		t.Errorf("managedOllamaBinary = %q, want %q", got, bin)
	}
}
