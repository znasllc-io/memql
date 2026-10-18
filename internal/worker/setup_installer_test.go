package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/inference"
)

// The setup paths the installer depends on (memql-cockpit#524): a runtime
// that is installed and stopped, the disk preflight, the resumed pull, and
// the one-line outcome the installers print in their summary.

// macStopped is Apple Silicon with Homebrew's Ollama installed and its
// service stopped: present, and not answering.
func macStopped() inference.Host {
	h := macNeedsOllama()
	h.LookPath = pathWith("brew", "ollama")
	h.Stopped = inference.StoppedFacts{Brew: true}
	return h
}

func TestSetupInference_StoppedRuntimeIsStartedAfterConsent(t *testing.T) {
	f := newFakeSetup(t, macStopped())
	f.setup.in = strings.NewReader("y\n")
	var started [][]string
	f.setup.start = func(_ context.Context, commands []string) error {
		started = append(started, commands)
		return nil
	}
	readyAsked := 0
	f.setup.ready = func(context.Context, string) error { readyAsked++; return nil }
	f.setup.probe = func(context.Context, string) error { return errors.New("connection refused") }

	code, out := f.run()
	if code != SetupExitOK {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
	if len(started) != 1 || strings.Join(started[0], " ") != "brew services start ollama" {
		t.Fatalf("the stopped runtime must be started with brew services, ran %q", started)
	}
	if readyAsked != 1 {
		t.Errorf("the setup must wait for the runtime to answer after starting it, asked %d times", readyAsked)
	}
	for _, want := range []string{
		"Ollama is installed and is not answering, so it has to be started before a model can be pulled.",
		"This command will run: brew services start ollama",
		"The runtime is started.",
	} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("transcript must carry %q:\n%s", want, out)
		}
	}
	if len(f.pulled) == 0 {
		t.Error("the pulls must run once the runtime answers")
	}
}

func TestSetupInference_StoppedRuntimeNonInteractiveAsksNothingAndExitsThree(t *testing.T) {
	f := newFakeSetup(t, macStopped())
	f.setup.nonInteractive = true
	f.setup.probe = func(context.Context, string) error { return errors.New("connection refused") }
	f.setup.start = func(context.Context, []string) error { t.Fatal("nothing may start without consent"); return nil }

	code, out := f.run()
	if code != SetupExitRefused {
		t.Fatalf("exit = %d, want %d:\n%s", code, SetupExitRefused, out)
	}
	if !strings.Contains(out, "Nothing was started: --non-interactive cannot answer that question.") {
		t.Errorf("the transcript must say nothing was started:\n%s", out)
	}
	if len(f.pulled) != 0 {
		t.Error("no pull may run against a runtime that is not answering")
	}
}

// A service that was itself still starting answers the second look, and
// nothing is started twice.
func TestSetupInference_StoppedRuntimeThatAnswersTheSecondLookIsLeftAlone(t *testing.T) {
	f := newFakeSetup(t, macStopped())
	f.setup.probe = func(context.Context, string) error { return nil }
	f.setup.start = func(context.Context, []string) error {
		t.Fatal("a runtime that answers must not be started")
		return nil
	}
	if code, out := f.run(); code != SetupExitOK {
		t.Fatalf("exit = %d, want 0:\n%s", code, out)
	}
}

func TestSetupInference_StoppedRuntimeNobodyCanStartIsOperationFailed(t *testing.T) {
	h := macStopped()
	h.Stopped = inference.StoppedFacts{}
	f := newFakeSetup(t, h)
	f.setup.probe = func(context.Context, string) error { return errors.New("connection refused") }
	code, out := f.run()
	if code != SetupExitOpFailed {
		t.Fatalf("exit = %d, want %d:\n%s", code, SetupExitOpFailed, out)
	}
	want := "Ollama is installed and is not answering, and this command cannot tell how it was installed. Start it (open the Ollama app, or run ollama serve in a terminal) and run this again."
	if !strings.Contains(flat(out), want) {
		t.Errorf("the transcript must carry the stopped sentence:\n%s", out)
	}
	if f.setup.reason != want {
		t.Errorf("reason = %q, want the same sentence for the installer's summary", f.setup.reason)
	}
}

// The preflight refuses before the first byte moves, on the bytes still
// missing and free space measured now -- never on a total that counts
// what is already on disk.
func TestSetupInference_DiskPreflightRefusesBeforeAnyPull(t *testing.T) {
	h := macServing()
	h.ModelsDir = "/Users/op/.ollama/models"
	f := newFakeSetup(t, h)
	f.setup.missing = func(_ context.Context, dir, id string) (uint64, bool) {
		if dir != "/Users/op/.ollama/models" {
			t.Errorf("preflight read %q, want the runtime's models directory", dir)
		}
		return 10_000_000_000, true
	}
	f.setup.free = func(string) uint64 { return 8_000_000_000 }

	code, out := f.run()
	if code != SetupExitOpFailed {
		t.Fatalf("exit = %d, want %d:\n%s", code, SetupExitOpFailed, out)
	}
	if len(f.pulled) != 0 {
		t.Errorf("no pull may start when the models cannot fit, pulled %q", f.pulled)
	}
	for _, want := range []string{"There is not enough disk for these models", "8.0 GB is free", "/Users/op/.ollama/models"} {
		if !strings.Contains(flat(out), want) {
			t.Errorf("the refusal must carry %q:\n%s", want, out)
		}
	}
}

func TestSetupInference_DiskPreflightPassesWhenTheModelsAreAlreadyThere(t *testing.T) {
	h := macServing()
	h.ModelsDir = "/Users/op/.ollama/models"
	f := newFakeSetup(t, h)
	f.setup.missing = func(context.Context, string, string) (uint64, bool) { return 0, true }
	f.setup.free = func(string) uint64 { return 2_000_000_000 }
	// On disk already: the runtime reports each layer complete at once.
	f.progress["qwen3.8:27b"] = []inference.Progress{
		{Status: "pulling manifest"},
		{Status: "pulling 8934d96d3f08", Completed: 4_661_211_808, Total: 4_661_211_808},
		{Status: "success"},
	}
	if code, out := f.run(); code != SetupExitOK {
		t.Fatalf("models already on disk need no room, exit = %d:\n%s", code, out)
	}
}

// A resumed pull: the big layer is already on disk when the runtime first
// reports it, so it needs no room. Judging the total refused this pull.
func TestPullDisplay_ResumedLayersNeedNoRoom(t *testing.T) {
	var b strings.Builder
	stopped := false
	d := newPullDisplay(&b, false, 1_000_000_000, func() { stopped = true })
	d.handle(inference.Progress{Status: "pulling aaaa", Completed: 4_000_000_000, Total: 4_100_000_000})
	d.handle(inference.Progress{Status: "pulling aaaa", Completed: 4_100_000_000, Total: 4_100_000_000})
	d.handle(inference.Progress{Status: "pulling bbbb", Completed: 0, Total: 500_000_000})
	if stopped || d.overflow("m") != nil {
		t.Fatalf("100 MB left of the resumed layer plus a 500 MB layer fit in 1 GB; refused:\n%s", b.String())
	}
	// And a layer that genuinely does not fit is still refused.
	d.handle(inference.Progress{Status: "pulling cccc", Completed: 0, Total: 900_000_000})
	if !stopped || d.overflow("m") == nil {
		t.Fatal("1.5 GB still to write into 1 GB free must be refused")
	}
}

func TestRecordSetupReasonWritesOneLineForTheInstaller(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reason")
	t.Setenv(setupReasonEnv, path)
	recordSetupReason("This machine has 8 GB of unified memory;\n  the floor is 16 GB.")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "This machine has 8 GB of unified memory; the floor is 16 GB.\n" {
		t.Errorf("reason file = %q, want one line", got)
	}
	recordSetupReason(strings.Repeat("x", 1000))
	got, _ = os.ReadFile(path)
	if len(got) > maxSetupReason+5 || !strings.HasSuffix(string(got), "...\n") {
		t.Errorf("a long reason must be cut short, got %d bytes", len(got))
	}
	t.Setenv(setupReasonEnv, "")
	recordSetupReason("nothing to write")
}

// Below the floor, the outcome line is the floor's own sentence: the
// installer prints "This machine can't run local models: <it>".
func TestSetupInference_RefusalIsTheRecordedReason(t *testing.T) {
	f := newFakeSetup(t, belowFloor())
	path := filepath.Join(t.TempDir(), "reason")
	t.Setenv(setupReasonEnv, path)
	if code := runInferenceSetup(context.Background(), f.setup); code != SetupExitPrereq {
		t.Fatalf("exit = %d, want %d", code, SetupExitPrereq)
	}
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != "This machine has 8 GB of unified memory; the floor is 16 GB." {
		t.Errorf("recorded reason = %q", got)
	}
}
