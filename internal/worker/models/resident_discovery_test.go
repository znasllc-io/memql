package models

import (
	"context"
	"strings"
	"testing"
	"time"
)

// residentFixture is an Ollama with a chat model and an embedder, which is
// the pair every recommended set is.
func residentFixture(t *testing.T) string {
	t.Helper()
	srv, _ := ollamaStub(t, []string{"chat:27b", "embed:0.6b"}, map[string]string{
		"chat:27b":   `{"capabilities":["completion","tools"],"model_info":{"qwen35.context_length":262144}}`,
		"embed:0.6b": `{"capabilities":["embedding"],"model_info":{"qwen3.context_length":32768}}`,
	})
	return srv.URL
}

func byID(inv Inventory) map[string]Info {
	out := map[string]Info{}
	for _, m := range inv.Models {
		out[m.ID] = m
	}
	return out
}

// TestProbeOllama_ResidentSettingsRideTheModelNotTheLabel. keep_alive and
// the working window are this machine's business with its own runtime: the
// engine parses the label and skips a key it does not know, so putting
// either on the label would be a setting the engine can neither read nor
// reject. They ride Info.
func TestProbeOllama_ResidentSettingsRideTheModelNotTheLabel(t *testing.T) {
	url := residentFixture(t)
	inv := discovererFor(url, nil).Probe(context.Background(), Request{
		Allow:    []string{"chat:27b", "embed:0.6b"},
		Parallel: 2,
		Resident: []Resident{
			{ID: "chat:27b", KeepAlive: KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384},
			{ID: "embed:0.6b", KeepAlive: KeepAlive{Set: true, Forever: true}, WorkingContext: 8192},
		},
	})
	got := byID(inv)

	chat := got["chat:27b"]
	if !chat.KeepAlive.Set || chat.KeepAlive.For != 2*time.Hour || chat.WorkingContext != 16384 {
		t.Errorf("chat model resident settings = %+v / %d", chat.KeepAlive, chat.WorkingContext)
	}
	embed := got["embed:0.6b"]
	if !embed.KeepAlive.Forever || embed.WorkingContext != 0 {
		t.Errorf("embedder resident settings = %+v / %d", embed.KeepAlive, embed.WorkingContext)
	}

	// Nothing about residency reaches the wire contract. The only label
	// change is the ceiling, which is the point of models.parallel.
	labels := inv.Labels()
	if want := "ctx=262144,structured=1,max=2,tools=1"; labels[Label("chat:27b")] != want {
		t.Errorf("chat label = %q, want %q", labels[Label("chat:27b")], want)
	}
	if want := "ctx=32768,embeddings=1,max=2"; labels[Label("embed:0.6b")] != want {
		t.Errorf("embedder label = %q, want %q", labels[Label("embed:0.6b")], want)
	}
}

// TestProbeOllama_NoResidentEntryChangesNothing. A machine whose owner
// wrote nothing sends no keep_alive and pins no window, so shipping the
// feature changes no machine until its owner opts in.
func TestProbeOllama_NoResidentEntryChangesNothing(t *testing.T) {
	url := residentFixture(t)
	inv := discovererFor(url, nil).Probe(context.Background(), Request{Allow: []string{"chat:27b", "embed:0.6b"}})
	for id, m := range byID(inv) {
		if m.KeepAlive.Set || m.WorkingContext != 0 {
			t.Errorf("%s carries resident settings with none configured: %+v / %d", id, m.KeepAlive, m.WorkingContext)
		}
		if m.MaxConcurrent != 1 {
			t.Errorf("%s max = %d, want 1", id, m.MaxConcurrent)
		}
	}
	if len(inv.ProbeNotes) != 0 {
		t.Errorf("notes with nothing configured: %v", inv.ProbeNotes)
	}
}

// TestProbeOllama_TheWindowNeverExceedsWhatTheModelCanHold. A window past
// the model's own context would be sent to the runtime as a request it
// cannot honour; the model's ceiling wins.
func TestProbeOllama_TheWindowNeverExceedsWhatTheModelCanHold(t *testing.T) {
	srv, _ := ollamaStub(t, []string{"small:1b"}, map[string]string{
		"small:1b": `{"capabilities":["completion"],"model_info":{"llama.context_length":4096}}`,
	})
	inv := discovererFor(srv.URL, nil).Probe(context.Background(), Request{
		Allow:    []string{"small:1b"},
		Resident: []Resident{{ID: "small:1b", WorkingContext: 16384}},
	})
	if got := byID(inv)["small:1b"].WorkingContext; got != 4096 {
		t.Errorf("working context = %d, want the model's own 4096", got)
	}
}

// TestProbeOllama_ResidentNotesNameWhatCannotWork. Each note is a reason
// an entry is idle, in the channel `memql worker models` already prints.
func TestProbeOllama_ResidentNotesNameWhatCannotWork(t *testing.T) {
	url := residentFixture(t)
	oa := openAIStub(t, []string{"remote:7b"})
	inv := discovererFor(url, nil).Probe(context.Background(), Request{
		Allow: []string{"chat:27b", "embed:0.6b", "remote:7b"},
		Runtimes: []DeclaredRuntime{{
			Name: "lmstudio", BaseURL: oa.URL,
			Models: []DeclaredModel{{ID: "remote:7b", StructuredOutput: true}},
		}},
		Resident: []Resident{
			{ID: "embed:0.6b", KeepAlive: KeepAlive{Set: true, Forever: true}, WorkingContext: 8192},
			{ID: "remote:7b", KeepAlive: KeepAlive{Set: true, Forever: true}},
			{ID: "ghost:70b", KeepAlive: KeepAlive{Set: true, Forever: true}},
		},
	})
	notes := strings.Join(inv.ProbeNotes, "\n")
	for _, want := range []string{
		"embed:0.6b is an embedding model; embeddings run at a fixed window, so working_context is ignored",
		"remote:7b is served by lmstudio, which has no keep_alive; models.resident is ignored for it",
		"ghost:70b is not installed in any runtime this machine probes",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("no note says %q:\n%s", want, notes)
		}
	}
	// A declared runtime's model never carries an Ollama-only setting.
	if m := byID(inv)["remote:7b"]; m.KeepAlive.Set || m.WorkingContext != 0 {
		t.Errorf("an OpenAI-compatible model carries resident settings: %+v / %d", m.KeepAlive, m.WorkingContext)
	}
}

// TestEffectiveParallelism. Whatever the sources say, the answer is never
// zero: the engine reads a missing max as unlimited.
func TestEffectiveParallelism(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		env        string
		managed    int
		want       int
	}{
		{"the owner's policy wins", 2, "4", 3, 2},
		{"the worker's environment beats the unit", 0, "4", 3, 4},
		{"the unit the cockpit wrote", 0, "", 3, 3},
		{"nothing said", 0, "", 0, 1},
		{"a garbage environment is not a source", 0, "nonsense", 0, 1},
		{"a garbage environment falls through to the unit", 0, "-3", 5, 5},
		// Ollama reads the number with ParseUint, which takes no sign: it logs
		// "+2" as invalid and runs one slot. Advertising two would claim a slot
		// that does not exist.
		{"a signed environment is not a source", 0, "+2", 5, 5},
		{"a signed environment with no unit is the default", 0, "+2", 0, 1},
		// A count past what the ceiling can hold would wrap when the machine
		// total is built, and a ceiling of zero is the engine's "unlimited".
		{"a count too big to hold is not a source", 0, "4294967296", 3, 3},
		{"a negative policy value is not a source", -1, "", 0, 1},
		{"a negative unit value is not a source", 0, "", -2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Discoverer{
				Getenv: func(k string) string {
					if k == "OLLAMA_NUM_PARALLEL" {
						return tt.env
					}
					return ""
				},
				ManagedParallel: func() int { return tt.managed },
			}
			got := d.effectiveParallelism(tt.configured)
			if got != tt.want {
				t.Errorf("effectiveParallelism(%d) env=%q unit=%d -> %d, want %d", tt.configured, tt.env, tt.managed, got, tt.want)
			}
			if got < 1 {
				t.Errorf("the ceiling must never be below 1, got %d", got)
			}
		})
	}
	// No seam at all is the common case: a Docker runtime, or a Mac. With no
	// Getenv seam the process environment answers, so the variable is pinned
	// here; an exported OLLAMA_NUM_PARALLEL must not change this case.
	t.Setenv("OLLAMA_NUM_PARALLEL", "")
	var d Discoverer
	if got := d.effectiveParallelism(0); got != 1 {
		t.Errorf("a discoverer with no sources -> %d, want 1", got)
	}
}

// TestParallelSource names where the ceiling came from, so the report can
// say it: a number whose origin is not stated is a number nobody can
// change.
func TestParallelSource(t *testing.T) {
	tests := []struct {
		name       string
		configured int
		env        string
		managed    int
		want       int
		source     string
	}{
		{"policy", 2, "4", 3, 2, "models.parallel in policy.yaml"},
		{"environment", 0, "4", 3, 4, "OLLAMA_NUM_PARALLEL in the worker's environment"},
		{"unit", 0, "", 3, 3, "the memql-ollama.service unit"},
		{"default", 0, "", 0, 1, "the default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Discoverer{
				Getenv: func(k string) string {
					if k == "OLLAMA_NUM_PARALLEL" {
						return tt.env
					}
					return ""
				},
				ManagedParallel: func() int { return tt.managed },
			}
			n, source := d.ParallelSource(tt.configured)
			if n != tt.want || source != tt.source {
				t.Errorf("ParallelSource = %d from %q, want %d from %q", n, source, tt.want, tt.source)
			}
			if n != d.effectiveParallelism(tt.configured) {
				t.Errorf("ParallelSource and effectiveParallelism disagree")
			}
		})
	}
}
