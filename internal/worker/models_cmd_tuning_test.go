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
		"Requests per Ollama model: 2, from the memql-ollama.service unit.",
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
	// BY DEFAULT: the five minutes is Ollama's own default, and a drop-in that
	// sets OLLAMA_KEEP_ALIVE changes it, so the report must not state it flat.
	const want = "Kept loaded: nothing is configured to stay loaded. By default, Ollama unloads each model " +
		"five minutes after its last call. Add models.resident to policy.yaml to change that, then send the worker a SIGHUP."
	if !strings.Contains(flat(out), want) {
		t.Errorf("the report does not say %q:\n%s", want, out)
	}
	if strings.Contains(flat(out), ". Ollama unloads each model") {
		t.Errorf("the five minutes is stated as if it were not a default:\n%s", out)
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
		// ollama is on PATH, so the bare name is the command that runs. Both
		// lookups are set so the output does not depend on this machine's PATH
		// or on whether a managed runtime is installed.
		LookPath:      func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		ManagedOllama: func() string { return "" },
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
	if !strings.Contains(out, "Requests per Ollama model: 2") {
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

// noneServedLine is the kept section's sentence when models.resident has
// entries and this machine serves none of them. The reasons are printed above
// it (the hardware floor, the notes) or below it (the problems line).
const noneServedLine = "Kept loaded: none of the models.resident entries is served by this machine; the reasons are above and below."

// TestModelsTuningReport_OnlyBlockedEntriesSaysNoneIsServed. An entry for a
// model models.allow does not list stands in the policy but nothing here
// serves it. The kept section must not say nothing is configured: the owner
// wrote an entry, and "add models.resident" would tell them to write one.
func TestModelsTuningReport_OnlyBlockedEntriesSaysNoneIsServed(t *testing.T) {
	p := tuningPolicy(t, "models:\n  allow:\n    - qwen3-embedding:0.6b\n  resident:\n    - id: qwen3.8:27b\n      keep_alive: 2h\n")
	blocked := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	blocked.Allowed = false
	blocked.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	out := renderTuning(t, servingInventory(blocked), tuningReport{
		Parallel: 2, ParallelSource: "the default",
		Problems: p.ModelsTuningProblems(), Resident: p.ModelsTuning().Resident,
	})
	if !strings.Contains(out, noneServedLine) {
		t.Errorf("an entry nothing here serves must be said so:\n%s", out)
	}
	if strings.Contains(out, "nothing is configured to stay loaded") || strings.Contains(out, "Add models.resident") {
		t.Errorf("the owner wrote an entry, so the report must not say nothing is configured:\n%s", out)
	}
	if !strings.Contains(out, "models.resident names qwen3.8:27b, which models.allow does not list") {
		t.Errorf("the problems line must still name the blocked entry:\n%s", out)
	}
}

// TestModelsTuningReport_BelowTheFloorAllowedEntryIsNotKept. The hardware floor
// decides whether this machine serves a model at all (Inventory.Advertised). On
// a machine below it an allowed entry is inert, so it is not listed as kept.
func TestModelsTuningReport_BelowTheFloorAllowedEntryIsNotKept(t *testing.T) {
	p := tuningPolicy(t, "models:\n  allow:\n    - qwen3.8:27b\n  resident:\n    - id: qwen3.8:27b\n      keep_alive: 2h\n")
	allowed := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	allowed.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	inv := models.Inventory{
		Floor:  models.FloorVerdict{Reason: "this machine has 8 GB of unified memory; the floor is 16 GB."},
		Models: []models.Info{allowed},
	}
	out := renderTuning(t, inv, tuningReport{
		Parallel: 2, ParallelSource: "the default", Resident: p.ModelsTuning().Resident,
	})
	if strings.Contains(out, "Kept loaded (policy.yaml models.resident):") {
		t.Errorf("a machine below the hardware floor serves nothing, so nothing is kept:\n%s", out)
	}
	if !strings.Contains(out, noneServedLine) {
		t.Errorf("an allowed entry below the floor must be said to be unserved:\n%s", out)
	}
}

// TestModelsTuningReport_TrulyEmptyResidentKeepsTheOldSentence. With no entry in
// models.resident the kept section says what Ollama does instead and how to add
// one. The new sentence is for entries that nothing serves, and must not show.
func TestModelsTuningReport_TrulyEmptyResidentKeepsTheOldSentence(t *testing.T) {
	inv := servingInventory(offeredModel("llama3.1:8b", models.Attributes{MaxConcurrent: 1}))
	out := renderTuning(t, inv, tuningReport{Parallel: 1, ParallelSource: "the default"})
	for _, want := range []string{"nothing is configured to stay loaded", "Add models.resident to policy.yaml to change"} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("with models.resident empty the report must say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "none of the models.resident entries") {
		t.Errorf("with no entry written there is none to say is unserved:\n%s", out)
	}
}

// TestLoadedWords pins the one line Ollama's state is printed as, branch by
// branch. An expiry at or before now is "unloads now": it used to print
// "unloads in now".
func TestLoadedWords(t *testing.T) {
	const gb = 1_000_000_000
	full := func(expires time.Time) models.Loaded {
		return models.Loaded{ID: "m", SizeBytes: 18 * gb, VRAMBytes: 18 * gb, ContextLength: 16384, ExpiresAt: expires}
	}
	tests := []struct {
		name string
		l    models.Loaded
		want string
	}{
		{"hours and minutes", full(reportNow.Add(118 * time.Minute)), "18.0 GB, 100% on the GPU, window 16384, unloads in 1h58m"},
		{"whole hours", full(reportNow.Add(2 * time.Hour)), "18.0 GB, 100% on the GPU, window 16384, unloads in 2h0m"},
		{"minutes", full(reportNow.Add(17*time.Minute + 30*time.Second)), "18.0 GB, 100% on the GPU, window 16384, unloads in 17m"},
		{"one minute", full(reportNow.Add(time.Minute)), "18.0 GB, 100% on the GPU, window 16384, unloads in 1m"},
		{"seconds", full(reportNow.Add(45 * time.Second)), "18.0 GB, 100% on the GPU, window 16384, unloads in 45s"},
		{"this instant", full(reportNow), "18.0 GB, 100% on the GPU, window 16384, unloads now"},
		{"already past", full(reportNow.Add(-5 * time.Second)), "18.0 GB, 100% on the GPU, window 16384, unloads now"},
		{"forever", full(reportNow.AddDate(300, 0, 0)), "18.0 GB, 100% on the GPU, window 16384, stays loaded"},
		{"no expiry stated", full(time.Time{}), "18.0 GB, 100% on the GPU, window 16384"},
		{"no window stated", models.Loaded{ID: "m", SizeBytes: 18 * gb, VRAMBytes: 18 * gb, ExpiresAt: reportNow.Add(time.Hour)}, "18.0 GB, 100% on the GPU, unloads in 1h0m"},
		{"partly on the CPU", models.Loaded{ID: "m", SizeBytes: 20 * gb, VRAMBytes: 7 * gb, ContextLength: 8192, ExpiresAt: reportNow.Add(time.Hour)}, "20.0 GB, 35% on the GPU, window 8192, unloads in 1h0m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := loadedWords(tt.l, reportNow)
			if got != tt.want {
				t.Errorf("loadedWords = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "in now") {
				t.Errorf("%q reads \"in now\"", got)
			}
		})
	}
}

// TestModelsTuningReport_AWindowOnlyEntryIsNotKeptLoaded. An entry that sets a
// working_context and no keep_alive pins the window the model loads at, and
// does nothing about when it unloads: Ollama still drops it after its default
// five minutes. It stands under the kept-loaded heading with its sentence, and
// the sentence must not read as though the model stays.
func TestModelsTuningReport_AWindowOnlyEntryIsNotKeptLoaded(t *testing.T) {
	chat := offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 262144, MaxConcurrent: 2})
	chat.WorkingContext = 16384
	embed := offeredModel("qwen3-embedding:0.6b", models.Attributes{Embeddings: true, MaxConcurrent: 2})
	embed.KeepAlive = models.KeepAlive{Set: true, For: 2 * time.Hour}
	out := renderTuning(t, servingInventory(embed, chat), tuningReport{Parallel: 2, ParallelSource: "the default"})
	kept := keptSection(out)

	const windowOnly = "at a working context of 16384, but not kept loaded: with no keep_alive, by default Ollama unloads it five minutes after its last call"
	var chatLine, embedLine string
	for _, line := range strings.Split(kept, "\n") {
		switch {
		case strings.Contains(line, "qwen3.8:27b"):
			chatLine = flat(line)
		case strings.Contains(line, "qwen3-embedding:0.6b"):
			embedLine = flat(line)
		}
	}
	if want := "qwen3.8:27b " + windowOnly; chatLine != want {
		t.Errorf("the window-only line is %q, want %q", chatLine, want)
	}
	if strings.Contains(chatLine, "kept loaded for") || strings.Contains(chatLine, "kept loaded forever") {
		t.Errorf("a model with no keep_alive reads as kept loaded: %q", chatLine)
	}
	// And an entry that does set keep_alive is unchanged.
	if want := "qwen3-embedding:0.6b kept loaded for 2h0m0s"; embedLine != want {
		t.Errorf("the keep_alive line is %q, want %q", embedLine, want)
	}
}

// TestShellWord. The report prints a command for a person to paste, and a path
// with a space or a shell metacharacter in it is not one word to their shell:
// it runs the wrong thing, or nothing. A word of plain characters stays bare,
// so the common case reads as it always did.
func TestShellWord(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ollama", "ollama"},
		{"/home/op/.memql/ollama/runtime/bin/ollama", "/home/op/.memql/ollama/runtime/bin/ollama"},
		{"qwen3.8:27b", "qwen3.8:27b"},
		{"hf.co/org/model:Q4_K_M", "hf.co/org/model:Q4_K_M"},
		{"/home/Jane Doe/.memql/ollama/runtime/bin/ollama", "'/home/Jane Doe/.memql/ollama/runtime/bin/ollama'"},
		{"/home/o'brien/bin/ollama", `'/home/o'\''brien/bin/ollama'`},
		{"/home/$USER/bin/ollama", "'/home/$USER/bin/ollama'"},
		{"/home/a;b/ollama", "'/home/a;b/ollama'"},
		{"/home/a&b/ollama", "'/home/a&b/ollama'"},
		{"/home/a*b/ollama", "'/home/a*b/ollama'"},
		{"/home/a`b/ollama", "'/home/a`b/ollama'"},
		{`/home/a\b/ollama`, `'/home/a\b/ollama'`},
		{"/home/(a)/ollama", "'/home/(a)/ollama'"},
		{"~/bin/ollama", "'~/bin/ollama'"},
		{"a=b", "'a=b'"},
		{"two\nlines", "'two\nlines'"},
		{"'", `''\'''`},
		{"", "''"},
	}
	for _, tt := range tests {
		if got := shellWord(tt.in); got != tt.want {
			t.Errorf("shellWord(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// TestModelsTuningReport_CPUCalloutQuotesAPathWithASpace. The managed runtime
// lives under the owner's home, and a home with a space in it (a Linux account
// named for a person, or a Mac) must not turn the printed command into two
// words.
func TestModelsTuningReport_CPUCalloutQuotesAPathWithASpace(t *testing.T) {
	home := filepath.Join(t.TempDir(), "Jane Doe")
	t.Setenv("HOME", home)
	bin := filepath.Join(inference.DefaultNativeFacts(home, "").RuntimeDir, "bin", "ollama")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	partly := []models.Loaded{{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 6_500_000_000, ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)}}
	// Not on PATH, so the managed binary is the command; the real lookup of it
	// (a nil ManagedOllama) is what reads HOME.
	out := renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default", Loaded: partly,
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	})
	if want := "Free GPU memory, then run: '" + bin + "' stop qwen3.8:27b"; calloutLine(out) != want {
		t.Errorf("the callout says %q, want %q", calloutLine(out), want)
	}

	// And an apostrophe in the path is escaped the POSIX way.
	out = renderTuning(t, tunedInventory(), tuningReport{
		Parallel: 2, ParallelSource: "the default", Loaded: partly,
		LookPath:      func(string) (string, error) { return "", errors.New("not found") },
		ManagedOllama: func() string { return "/home/o'brien/.memql/ollama/runtime/bin/ollama" },
	})
	if want := `Free GPU memory, then run: '/home/o'\''brien/.memql/ollama/runtime/bin/ollama' stop qwen3.8:27b`; calloutLine(out) != want {
		t.Errorf("the callout says %q, want %q", calloutLine(out), want)
	}
}

// TestModelsTuningReport_NoCPUCalloutWithoutASize. A size Ollama did not state
// is not a share on the GPU of zero: "0% on the GPU" and a fault to fix would
// be said of a model nobody measured. The line says the size is not reported
// and nothing is called out; a model whose size IS known and whose GPU share is
// low still is.
//
// What this does not cover: a machine with no GPU at all reads 0% on every
// model, and the report has no structured way to know the machine has none
// (the hardware floor's verdict carries it only in a sentence). Such a machine
// is below the floor and serves nothing, so its report is the "none served"
// one; the callout there is a known gap.
func TestModelsTuningReport_NoCPUCalloutWithoutASize(t *testing.T) {
	look := func(string) (string, error) { return "/usr/local/bin/ollama", nil }
	unknown := []models.Loaded{{ID: "qwen3.8:27b", ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)}}
	out := renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the default", Loaded: unknown, LookPath: look})
	if strings.Contains(out, "The rest runs on the CPU") || calloutLine(out) != "" {
		t.Errorf("a model of unknown size was called out as running on the CPU:\n%s", out)
	}
	if want := "qwen3.8:27b                    size not reported, window 16384, unloads in 1h0m"; !strings.Contains(out, want) {
		t.Errorf("the report does not say %q:\n%s", want, out)
	}
	if strings.Contains(out, "0% on the GPU") || strings.Contains(out, "0.0 GB") {
		t.Errorf("a size nobody stated was printed as a measurement:\n%s", out)
	}

	known := []models.Loaded{{ID: "qwen3.8:27b", SizeBytes: 18_400_000_000, VRAMBytes: 0, ContextLength: 16384, ExpiresAt: reportNow.Add(time.Hour)}}
	out = renderTuning(t, tunedInventory(), tuningReport{Parallel: 2, ParallelSource: "the default", Loaded: known, LookPath: look})
	if !strings.Contains(out, "18.4 GB, 0% on the GPU") || calloutLine(out) == "" {
		t.Errorf("a model of known size with nothing on the GPU must still be called out:\n%s", out)
	}
}
