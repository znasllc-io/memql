package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

func policyFromDoc(t *testing.T, doc string) (*Policy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	return p, path
}

// TestModelsTuning_LoadsFromPolicyYAML pins the shape the docs promise.
//
// `keep_alive: -1` is written UNQUOTED on purpose: it is the way an owner
// will actually type "never unload", YAML reads it as an integer, and it
// has to arrive as forever rather than as a decode error or as nothing.
func TestModelsTuning_LoadsFromPolicyYAML(t *testing.T) {
	p, _ := policyFromDoc(t, `models:
  allow:
    - qwen3.8:27b
    - qwen3-embedding:0.6b
  parallel: 2
  resident:
    - id: qwen3.8:27b
      keep_alive: 2h
      working_context: 16384
    - id: qwen3-embedding:0.6b
      keep_alive: -1
`)
	got := p.ModelsTuning()
	if got.Parallel != 2 {
		t.Errorf("parallel = %d, want 2", got.Parallel)
	}
	want := []models.Resident{
		{ID: "qwen3.8:27b", KeepAlive: models.KeepAlive{Set: true, For: 2 * time.Hour}, WorkingContext: 16384},
		{ID: "qwen3-embedding:0.6b", KeepAlive: models.KeepAlive{Set: true, Forever: true}},
	}
	if len(got.Resident) != len(want) {
		t.Fatalf("resident = %+v, want %+v", got.Resident, want)
	}
	for i := range want {
		if got.Resident[i] != want[i] {
			t.Errorf("resident[%d] = %+v, want %+v", i, got.Resident[i], want[i])
		}
	}
	if problems := p.ModelsTuningProblems(); len(problems) != 0 {
		t.Errorf("a clean file reported problems: %v", problems)
	}
}

// TestModelsTuning_AbsentIsTheDefault. Nothing here changes what a machine
// does until its owner writes it: no keep_alive is sent, no window is
// pinned, no ceiling is claimed.
func TestModelsTuning_AbsentIsTheDefault(t *testing.T) {
	for name, p := range map[string]*Policy{"default": DefaultPolicy(), "nil": nil} {
		got := p.ModelsTuning()
		if got.Parallel != 0 || len(got.Resident) != 0 {
			t.Errorf("%s policy tunes %+v, want nothing", name, got)
		}
		if problems := p.ModelsTuningProblems(); len(problems) != 0 {
			t.Errorf("%s policy reports problems: %v", name, problems)
		}
	}
}

// TestModelsTuning_ReloadReplacesRatherThanMerges.
//
// Review focus: deleting the block and sending SIGHUP must put every model
// back on Ollama's default without a restart. A merge, which the allow
// lists do, would keep a model loaded for two hours on the strength of a
// line the owner already deleted.
func TestModelsTuning_ReloadReplacesRatherThanMerges(t *testing.T) {
	p, path := policyFromDoc(t, `models:
  parallel: 3
  resident:
    - id: qwen3.8:27b
      keep_alive: -1
`)
	if got := p.ModelsTuning(); got.Parallel != 3 || len(got.Resident) != 1 {
		t.Fatalf("before reload: %+v", got)
	}
	if err := os.WriteFile(path, []byte("models:\n  allow:\n    - qwen3.8:27b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := p.ModelsTuning(); got.Parallel != 0 || len(got.Resident) != 0 {
		t.Errorf("after the block was deleted the policy still tunes %+v", got)
	}
}

// TestModelsTuning_AWrongShapeCostsOnlyItsSetting. A shape a typed decode
// refuses (a word where a number belongs, a list where an entry belongs)
// must never fail the whole policy.yaml: that leaves the worker on defaults
// that allow no model, and it costs every other line the owner wrote. The
// setting is named in one sentence, and the entries beside it stand. The
// allow list is asserted too, because a failed parse takes that line down
// with the rest.
func TestModelsTuning_AWrongShapeCostsOnlyItsSetting(t *testing.T) {
	tests := []struct {
		name         string
		doc          string
		wantSentence string
		wantParallel int
		wantResident int
	}{
		{
			name:         "working_context that is not a number",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 1h\n      working_context: 16k\n",
			wantSentence: `models.resident m: working_context belongs here as a whole number of tokens from 1 to 1048576, not the single word "16k"; working_context is ignored`,
			wantResident: 1,
		},
		{
			name:         "parallel as a quoted number",
			doc:          "models:\n  allow: [m]\n  parallel: \"2\"\n",
			wantSentence: `models.parallel belongs here as a whole number from 1 to 64, not the single word "2"; it is ignored`,
		},
		{
			name:         "parallel as a fraction",
			doc:          "models:\n  allow: [m]\n  parallel: 2.5\n",
			wantSentence: `models.parallel belongs here as a whole number from 1 to 64, not the single word "2.5"; it is ignored`,
		},
		{
			name:         "resident that is a word, not a list",
			doc:          "models:\n  allow: [m]\n  resident: m\n",
			wantSentence: `models.resident: a list of entries belongs here (- id: qwen3.8:27b), not the single word "m", so the block is ignored`,
		},
		{
			name:         "a resident entry that is a word, not a mapping",
			doc:          "models:\n  allow: [m]\n  resident: [m]\n",
			wantSentence: `models.resident entry 1: a mapping with an id belongs here (id: qwen3.8:27b, keep_alive: 2h), not the single word "m", so the entry is ignored`,
		},
		{
			name:         "an id that is a list",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: [m]\n      keep_alive: 1h\n",
			wantSentence: `models.resident entry 1: id is the name of a model, and a list is not one, so the entry is ignored`,
		},
		{
			name:         "keep_alive that is a list",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: [1h]\n      working_context: 8192\n",
			wantSentence: `models.resident m: keep_alive belongs here as a duration like 2h, or -1 for forever, not a list; keep_alive is ignored`,
			wantResident: 1,
		},
		{
			name:         "an unknown key beside a real setting",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 1h\n      working_ctx: 8192\n",
			wantSentence: `models.resident m: takes id, keep_alive and working_context, and "working_ctx" is none of them, so that key is ignored`,
			wantResident: 1,
		},
		{
			name:         "a key written twice in one entry",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 1h\n      keep_alive: 2h\n",
			wantSentence: `models.resident m: keep_alive is written twice, and the first one stands`,
			wantResident: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := policyFromDoc(t, tt.doc)
			if got := p.ModelsAllow(); len(got) != 1 || got[0] != "m" {
				t.Errorf("models.allow = %q, want [m]", got)
			}
			got := p.ModelsTuning()
			if got.Parallel != tt.wantParallel || len(got.Resident) != tt.wantResident {
				t.Errorf("tuning = %+v, want parallel %d and %d resident entries", got, tt.wantParallel, tt.wantResident)
			}
			if !slices.Contains(p.ModelsTuningProblems(), tt.wantSentence) {
				t.Errorf("no problem is exactly %q; got %q", tt.wantSentence, p.ModelsTuningProblems())
			}
		})
	}
}

// TestModelsTuning_AnEntryThatCannotStandIsReportedAndIgnored. Every
// refusal is a sentence an owner can act on, and none of them stops the
// worker or the entries beside it: a typo must cost the one setting, not
// the machine.
func TestModelsTuning_AnEntryThatCannotStandIsReportedAndIgnored(t *testing.T) {
	tests := []struct {
		name         string
		doc          string
		wantProblem  string
		wantParallel int
		wantResident int
	}{
		{
			name:         "parallel below the range",
			doc:          "models:\n  parallel: -2\n",
			wantProblem:  "models.parallel: -2 is outside 1 to 64",
			wantParallel: 0,
		},
		{
			name:         "parallel above the range",
			doc:          "models:\n  parallel: 500\n",
			wantProblem:  "models.parallel: 500 is outside 1 to 64",
			wantParallel: 0,
		},
		{
			name:         "entry without an id",
			doc:          "models:\n  resident:\n    - keep_alive: 2h\n",
			wantProblem:  "models.resident entry 1 has no id",
			wantResident: 0,
		},
		{
			name:         "the same id twice",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 2h\n    - id: m\n      keep_alive: -1\n",
			wantProblem:  "models.resident lists m more than once; the first entry stands",
			wantResident: 1,
		},
		{
			name:         "keep_alive zero would unload after every call",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 0\n",
			wantProblem:  "unloads the model after every call",
			wantResident: 0, // nothing else stands, so the entry is dropped
		},
		{
			name:         "keep_alive zero beside a window keeps the window",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 0\n      working_context: 8192\n",
			wantProblem:  "unloads the model after every call",
			wantResident: 1,
		},
		{
			name:         "keep_alive that is not a duration",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: soon\n      working_context: 8192\n",
			wantProblem:  `keep_alive "soon" is not a duration`,
			wantResident: 1,
		},
		{
			name:         "negative window",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      keep_alive: 1h\n      working_context: -5\n",
			wantProblem:  "working_context -5 is outside 1 to 1048576",
			wantResident: 1, // the keep_alive still stands
		},
		{
			name:         "window past the cap",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n      working_context: 99999999\n",
			wantProblem:  "working_context 99999999 is outside 1 to 1048576",
			wantResident: 0,
		},
		{
			name:         "entry that sets nothing",
			doc:          "models:\n  allow: [m]\n  resident:\n    - id: m\n",
			wantProblem:  "models.resident m sets neither keep_alive nor working_context, so it does nothing",
			wantResident: 0,
		},
		{
			name:         "model the machine does not serve",
			doc:          "models:\n  allow: [other]\n  resident:\n    - id: m\n      keep_alive: -1\n",
			wantProblem:  "models.resident names m, which models.allow does not list",
			wantResident: 1, // reported, not dropped: allowing it later must just work
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := policyFromDoc(t, tt.doc)
			got := p.ModelsTuning()
			if got.Parallel != tt.wantParallel {
				t.Errorf("parallel = %d, want %d", got.Parallel, tt.wantParallel)
			}
			if len(got.Resident) != tt.wantResident {
				t.Errorf("resident = %+v, want %d entries", got.Resident, tt.wantResident)
			}
			var found bool
			for _, s := range p.ModelsTuningProblems() {
				if strings.Contains(s, tt.wantProblem) {
					found = true
				}
			}
			if !found {
				t.Errorf("no problem says %q; got %q", tt.wantProblem, p.ModelsTuningProblems())
			}
		})
	}
}

// TestModelsTuning_AnUnknownKeyUnderModelsIsNamed. A typed decode drops a key
// it has no field for, without a word, so `residnet:` read exactly like an
// owner who never asked to keep a model loaded: nothing changed and nothing
// said why. A key directly under models: that nothing reads is named in one
// sentence, and everything the block does say still stands.
func TestModelsTuning_AnUnknownKeyUnderModelsIsNamed(t *testing.T) {
	const prefix = "models: takes allow, runtimes, pull, parallel and resident, and "
	const suffix = " is none of them, so that key is ignored"

	t.Run("resident mistyped", func(t *testing.T) {
		p, _ := policyFromDoc(t, "models:\n  allow: [m]\n  residnet:\n    - id: m\n      keep_alive: 2h\n")
		if got := p.ModelsTuning(); got.Parallel != 0 || len(got.Resident) != 0 {
			t.Errorf("a mistyped key tuned something: %+v", got)
		}
		if got := p.ModelsAllow(); len(got) != 1 || got[0] != "m" {
			t.Errorf("models.allow = %q, want [m]: one mistyped key must not cost the others", got)
		}
		want := prefix + `"residnet"` + suffix
		if got := p.ModelsTuningProblems(); len(got) != 1 || got[0] != want {
			t.Errorf("problems = %q, want exactly [%q]", got, want)
		}
	})

	t.Run("parallel mistyped", func(t *testing.T) {
		p, _ := policyFromDoc(t, "models:\n  allow: [m]\n  paralel: 2\n")
		if got := p.ModelsTuning(); got.Parallel != 0 {
			t.Errorf("a mistyped key set parallel to %d", got.Parallel)
		}
		want := prefix + `"paralel"` + suffix
		if got := p.ModelsTuningProblems(); len(got) != 1 || got[0] != want {
			t.Errorf("problems = %q, want exactly [%q]", got, want)
		}
	})

	t.Run("a misspelling beside a real setting costs only itself", func(t *testing.T) {
		p, _ := policyFromDoc(t, "models:\n  allow: [m]\n  parallel: 3\n  residnet:\n    - id: m\n      keep_alive: 2h\n")
		if got := p.ModelsTuning(); got.Parallel != 3 {
			t.Errorf("parallel = %d, want 3: the real key beside the typo must stand", got.Parallel)
		}
		want := prefix + `"residnet"` + suffix
		if got := p.ModelsTuningProblems(); len(got) != 1 || got[0] != want {
			t.Errorf("problems = %q, want exactly [%q]", got, want)
		}
	})

	t.Run("several are named in a fixed order", func(t *testing.T) {
		p, _ := policyFromDoc(t, "models:\n  residnet: []\n  alow: [m]\n  paralel: 2\n")
		want := []string{
			prefix + `"alow"` + suffix,
			prefix + `"paralel"` + suffix,
			prefix + `"residnet"` + suffix,
		}
		if got := p.ModelsTuningProblems(); !slices.Equal(got, want) {
			t.Errorf("problems = %q, want %q", got, want)
		}
	})

	t.Run("every key the block takes is known", func(t *testing.T) {
		p, _ := policyFromDoc(t, `models:
  allow: [m]
  pull: false
  parallel: 2
  resident:
    - id: m
      keep_alive: 2h
  runtimes:
    - name: lmstudio
      base_url: http://127.0.0.1:1234/v1
`)
		if got := p.ModelsTuningProblems(); len(got) != 0 {
			t.Errorf("a block of known keys reported problems: %q", got)
		}
	})

	t.Run("a reload that fixes the typo clears it", func(t *testing.T) {
		p, path := policyFromDoc(t, "models:\n  allow: [m]\n  paralel: 2\n")
		if len(p.ModelsTuningProblems()) != 1 {
			t.Fatalf("before the fix: %q", p.ModelsTuningProblems())
		}
		if err := os.WriteFile(path, []byte("models:\n  allow: [m]\n  parallel: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := p.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if got := p.ModelsTuningProblems(); len(got) != 0 {
			t.Errorf("after the fix: %q, want none", got)
		}
		if got := p.ModelsTuning(); got.Parallel != 2 {
			t.Errorf("after the fix parallel = %d, want 2", got.Parallel)
		}
	})
}
