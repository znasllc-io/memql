package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// models_cmd_section_test.go pins how `memql worker models` assembles the
// tuning half of its report: the gate that decides whether it prints, and that
// every input handleModels learns reaches the printer. The printer's own
// branches are pinned in models_cmd_tuning_test.go; the plan's rule that every
// report sentence is asserted is met for a whole section by the golden below.

// fakeTuningRuntime answers what the tuning section asks the machine and
// remembers that it was asked.
type fakeTuningRuntime struct {
	n         int
	source    string
	loaded    []models.Loaded
	loadedErr error

	configured []int // each argument ParallelSource was given
	loadedAsks int
}

func (f *fakeTuningRuntime) ParallelSource(configured int) (int, string) {
	f.configured = append(f.configured, configured)
	return f.n, f.source
}

func (f *fakeTuningRuntime) Loaded(context.Context) ([]models.Loaded, error) {
	f.loadedAsks++
	return f.loaded, f.loadedErr
}

// sectionHost is the machine the section is printed on: a fixed clock, ollama
// off PATH, and a managed runtime at a known path.
func sectionHost(rt *fakeTuningRuntime) tuningHost {
	return tuningHost{
		Runtime:       rt,
		Now:           reportNow,
		LookPath:      func(string) (string, error) { return "", errors.New("not on PATH") },
		ManagedOllama: func() string { return "/home/op/.memql/ollama/runtime/bin/ollama" },
	}
}

func ollamaAnswered(inv models.Inventory) models.Inventory {
	inv.RuntimeVersions = map[string]string{models.KindOllama: "0.34.2"}
	return inv
}

// TestModelsTuningSection_TheGate. The section answers "did it take", and has
// nothing to say about a machine with no Ollama and nothing configured: the
// inventory above it already says why. It prints when Ollama answered with a
// model (its version is read only then), when models.resident has an entry, or
// when the policy has a problem -- and a closed gate asks the machine nothing,
// so a command with no Ollama pays no second round trip for a section it will
// not print.
func TestModelsTuningSection_TheGate(t *testing.T) {
	plain := servingInventory(offeredModel("llama3.1:8b", models.Attributes{MaxConcurrent: 1}))
	tests := []struct {
		name   string
		inv    models.Inventory
		policy string
		want   bool
	}{
		{"nothing answered and nothing configured", plain, "models:\n  allow: [llama3.1:8b]\n", false},
		{"no policy at all", plain, "", false},
		{"Ollama answered with a model", ollamaAnswered(plain), "models:\n  allow: [llama3.1:8b]\n", true},
		{"an entry in models.resident", plain, "models:\n  allow: [llama3.1:8b]\n  resident:\n    - id: llama3.1:8b\n      keep_alive: 2h\n", true},
		{"a problem with the policy", plain, "models:\n  allow: [llama3.1:8b]\n  parallel: 500\n", true},
		{"a mistyped key", plain, "models:\n  allow: [llama3.1:8b]\n  residnet: []\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := &fakeTuningRuntime{n: 1, source: "the default"}
			var out strings.Builder
			got := printModelsTuning(context.Background(), &out, sectionHost(rt), tt.inv, tuningPolicy(t, tt.policy))
			if got != tt.want {
				t.Errorf("printed = %v, want %v:\n%s", got, tt.want, out.String())
			}
			if tt.want != (out.Len() > 0) {
				t.Errorf("printed = %v but %d bytes were written:\n%s", got, out.Len(), out.String())
			}
			if !tt.want && (len(rt.configured) != 0 || rt.loadedAsks != 0) {
				t.Errorf("a closed gate still asked the machine: %d ceiling lookups, %d /api/ps questions", len(rt.configured), rt.loadedAsks)
			}
			if tt.want && (len(rt.configured) != 1 || rt.loadedAsks != 1) {
				t.Errorf("an open gate asked %d ceiling lookups and %d /api/ps questions, want one of each", len(rt.configured), rt.loadedAsks)
			}
		})
	}
}

// TestModelsTuningSection_EveryInputReachesThePrinter. handleModels hands the
// printer six things, and each one dropped from that hand-off leaves every
// printer test green: the printer is tested with them set by hand. Losing
// Resident, for one, brings back "nothing is configured to stay loaded" for an
// owner who configured something.
func TestModelsTuningSection_EveryInputReachesThePrinter(t *testing.T) {
	// models.resident names a model models.allow does not list: nothing here
	// serves it, so it is in neither the inventory's kept models nor the
	// listing, and the only way the section can know an entry was written is
	// the policy's own tuning being passed through.
	policy := tuningPolicy(t, "models:\n  allow: [other:7b]\n  parallel: 3\n  resident:\n    - id: qwen3.8:27b\n      keep_alive: 2h\n")
	blocked := offeredModel("qwen3.8:27b", models.Attributes{MaxConcurrent: 3})
	blocked.Allowed = false
	blocked.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	rt := &fakeTuningRuntime{
		n: 7, source: "a fake source",
		loaded:    []models.Loaded{{ID: "other:7b", SizeBytes: 5_000_000_000, VRAMBytes: 5_000_000_000, ContextLength: 4096, ExpiresAt: reportNow.Add(time.Minute)}},
		loadedErr: nil,
	}
	var out strings.Builder
	inv := servingInventory(offeredModel("other:7b", models.Attributes{MaxConcurrent: 3}), blocked)
	if !printModelsTuning(context.Background(), &out, sectionHost(rt), inv, policy) {
		t.Fatal("the section was not printed")
	}
	got := out.String()

	// The ceiling: the policy's models.parallel is what the machine is asked
	// about, and the answer is what is printed.
	if len(rt.configured) != 1 || rt.configured[0] != 3 {
		t.Errorf("the ceiling was looked up with models.parallel %v, want [3]", rt.configured)
	}
	if !strings.Contains(got, "Requests per Ollama model: 7, from a fake source.") {
		t.Errorf("the ceiling and its source did not reach the printer:\n%s", got)
	}
	// Resident: the entry was written, and nothing here serves it.
	if !strings.Contains(flat(got), noneServedLine) {
		t.Errorf("the policy's models.resident did not reach the printer (it would say nothing is configured):\n%s", got)
	}
	if strings.Contains(got, "nothing is configured to stay loaded") {
		t.Errorf("an owner who wrote an entry was told nothing is configured:\n%s", got)
	}
	// Problems.
	if !strings.Contains(got, "models.resident names qwen3.8:27b, which models.allow does not list") {
		t.Errorf("the policy's problems did not reach the printer:\n%s", got)
	}
	// What Ollama holds, and the clock: one minute out.
	if want := "other:7b" + strings.Repeat(" ", 23) + "5.0 GB, 100% on the GPU, window 4096, unloads in 1m"; !strings.Contains(got, want) {
		t.Errorf("what Ollama holds, or the clock, did not reach the printer:\n%s", got)
	}

	// And a runtime that cannot be asked is a line in the report, not an error
	// that stops it.
	rt = &fakeTuningRuntime{n: 1, source: "the default", loadedErr: errors.New("no answer from Ollama's /api/ps at http://127.0.0.1:11434 (connection refused)")}
	out.Reset()
	printModelsTuning(context.Background(), &out, sectionHost(rt), inv, policy)
	if !strings.Contains(out.String(), "Could not read what Ollama has loaded: no answer from Ollama's /api/ps at http://127.0.0.1:11434 (connection refused)") {
		t.Errorf("the reason Ollama could not be asked did not reach the printer:\n%s", out.String())
	}
}

// goldenSection is the whole tuning section of a tuned machine, byte for byte:
// the 27B and its embedder kept loaded, the 27B loaded while the card was busy
// so that 65% of it runs on the CPU, the embedder kept forever, a third model
// nobody tuned, and one mistyped key in policy.yaml. Every sentence the
// section can say on this path is here, in one place; a reword is a decision
// made in this diff.
const goldenSection = `
Residency and concurrency

  Requests per Ollama model: 2, from the memql-ollama.service unit.

  Kept loaded (policy.yaml models.resident):
    qwen3-embedding:0.6b           kept loaded forever
    qwen3.8:27b                    kept loaded for 2h0m0s, at a working context of 16384

  Ollama has loaded now:
    qwen3.8:27b                    18.4 GB, 35% on the GPU, window 16384, unloads in 1h58m
      The rest runs on the CPU, which is slow, and stays that way until the model reloads.
      Free GPU memory, then run: /home/op/.memql/ollama/runtime/bin/ollama stop qwen3.8:27b
    qwen3-embedding:0.6b           1.4 GB, 100% on the GPU, window 8192, stays loaded

  Problems with models.parallel and models.resident:
    - models: takes allow, runtimes, pull, parallel and resident, and "paralel" is none of them, so that key is ignored
`

func TestModelsTuningSection_Golden(t *testing.T) {
	policy := tuningPolicy(t, `models:
  allow:
    - qwen3.8:27b
    - qwen3-embedding:0.6b
    - other:7b
  paralel: 2
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
    - id: qwen3-embedding:0.6b
      keep_alive: -1
`)
	chat := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	chat.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	chat.WorkingContext = 16384
	embed := offeredModel("qwen3-embedding:0.6b", models.Attributes{Embeddings: true, MaxConcurrent: 2})
	embed.KeepAlive = models.KeepAlive{Set: true, Forever: true}
	other := offeredModel("other:7b", models.Attributes{MaxConcurrent: 2})
	// In the order Probe returns them: sorted by id.
	inv := ollamaAnswered(servingInventory(other, embed, chat))

	rt := &fakeTuningRuntime{
		n: 2, source: "the memql-ollama.service unit",
		loaded: []models.Loaded{
			{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 6_500_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(118 * time.Minute)},
			{ID: "qwen3-embedding:0.6b", SizeBytes: 1_400_000_000, VRAMBytes: 1_400_000_000, ContextLength: 8192, ExpiresAt: reportNow.AddDate(300, 0, 0)},
		},
	}
	var out strings.Builder
	if !printModelsTuning(context.Background(), &out, sectionHost(rt), inv, policy) {
		t.Fatal("the section was not printed")
	}
	if got := out.String(); got != goldenSection {
		t.Errorf("the tuning section changed.\n--- got ---\n%s\n--- want ---\n%s", got, goldenSection)
	}
}
