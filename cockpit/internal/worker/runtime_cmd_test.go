package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
)

// -----------------------------------------------------------------------------
// The flag's two meanings
// -----------------------------------------------------------------------------

// --runtime MEANS TWO DIFFERENT THINGS, and the mix is REFUSED rather
// than resolved by whichever meaning happens to win. A person who typed
// the wrong combination is better served by a sentence.
func TestRuntimeModeRefusesTheInferenceCombination(t *testing.T) {
	for _, name := range inference.InstallableRuntimes() {
		got, refused := runtimeModeFor(setupFlags{inference: true, runtimeFlag: name})
		if !refused {
			t.Fatalf("--inference --runtime %s was accepted", name)
		}
		want := "--runtime " + name + " installs a speech or image runtime, and --runtime docker or " +
			"native chooses how Ollama runs. They are different questions, so they are different " +
			"commands: run memql worker setup --runtime " + name + " on its own."
		if flat(got) != want {
			t.Fatalf("refusal =\n  %q\nwant\n  %q", flat(got), want)
		}
	}
}

// The symmetric mistake: a MODEL runtime named without --inference.
// Refused with the command that would work, rather than silently
// entering the install flow with a name it does not have.
func TestRuntimeModeRefusesAModelRuntimeWithoutInference(t *testing.T) {
	for _, name := range []string{"docker", "native"} {
		got, refused := runtimeModeFor(setupFlags{runtimeFlag: name})
		if !refused {
			t.Fatalf("--runtime %s alone was accepted", name)
		}
		if !strings.Contains(flat(got), "Run memql worker setup --inference --runtime "+name) {
			t.Fatalf("the refusal must name the command that works: %q", got)
		}
	}
}

// An unknown name names BOTH vocabularies, so a typo reads as a typo.
func TestRuntimeModeRefusesAnUnknownName(t *testing.T) {
	got, refused := runtimeModeFor(setupFlags{runtimeFlag: "whisper"})
	if !refused {
		t.Fatal("--runtime whisper was accepted")
	}
	for _, want := range []string{"docker or native with --inference", "image or kokoro on its own", `"whisper"`} {
		if !strings.Contains(flat(got), want) {
			t.Errorf("the refusal must name %q: %s", want, flat(got))
		}
	}
}

// The two paths that are NOT refusals: an installable runtime on its
// own, and the existing docker|native meaning alongside --inference.
func TestRuntimeModeAcceptsBothRealMeanings(t *testing.T) {
	for _, name := range inference.InstallableRuntimes() {
		got, refused := runtimeModeFor(setupFlags{runtimeFlag: name})
		if refused || got != name {
			t.Fatalf("--runtime %s alone: got (%q, %v)", name, got, refused)
		}
	}
	for _, name := range []string{"docker", "native", ""} {
		got, refused := runtimeModeFor(setupFlags{inference: true, runtimeFlag: name})
		if refused || got != "" {
			t.Fatalf("--inference --runtime %q: got (%q, %v), want the existing path", name, got, refused)
		}
	}
}

// Case and surrounding space do not change the meaning. An operator who
// typed `--runtime Kokoro` gets the install, not the unknown-name
// refusal.
func TestRuntimeModeIsCaseInsensitive(t *testing.T) {
	got, refused := runtimeModeFor(setupFlags{runtimeFlag: "  KOKORO "})
	if refused || got != inference.RuntimeKokoro {
		t.Fatalf("got (%q, %v), want the kokoro install", got, refused)
	}
}

// -----------------------------------------------------------------------------
// The flow
// -----------------------------------------------------------------------------

type fakeRuntimeSetup struct {
	t     *testing.T
	setup *runtimeSetup
	out   *strings.Builder

	ran     [][]string
	present bool
	detail  string
}

func newFakeRuntimeSetup(t *testing.T, host inference.RuntimeHost, name string, mut func(*fakeRuntimeSetup)) *fakeRuntimeSetup {
	t.Helper()
	f := &fakeRuntimeSetup{t: t, out: &strings.Builder{}}
	f.setup = &runtimeSetup{
		out:    f.out,
		in:     strings.NewReader("y\n"),
		name:   name,
		gather: func(context.Context) (inference.RuntimeHost, error) { return host, nil },
		install: func(_ context.Context, cmds []string) error {
			f.ran = append(f.ran, cmds)
			return nil
		},
		probe: func(context.Context) (bool, string) { return f.present, f.detail },
	}
	if mut != nil {
		mut(f)
	}
	return f
}

func (f *fakeRuntimeSetup) run() (int, string) {
	f.t.Helper()
	code := runRuntimeSetup(context.Background(), f.setup)
	return code, f.out.String()
}

func dockerReady() inference.RuntimeHost {
	return inference.RuntimeHost{
		GOOS: "linux", GOARCH: "amd64",
		Docker: inference.DockerFacts{CLIPresent: true, Present: true, Version: "27.3.1"},
	}
}

// IDEMPOTENT RE-RUN: a second run installs nothing and says so in the
// present tense. Printing the install commands again reads as a machine
// that had lost the runtime, and somebody would run them -- producing a
// container name conflict against the one that is already working.
func TestRuntimeSetupIsIdempotent(t *testing.T) {
	h := dockerReady()
	h.KokoroPresent = true
	h.KokoroDetail = "http://127.0.0.1:8880 (0.2.4)"

	f := newFakeRuntimeSetup(t, h, inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.present, f.detail = true, "http://127.0.0.1:8880 (0.2.4)"
	})
	code, out := f.run()

	if code != SetupExitOK {
		t.Fatalf("exit = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if !strings.Contains(flat(out), "Kokoro is already running at http://127.0.0.1:8880 (0.2.4). Nothing to install.") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "docker run") {
		t.Errorf("an install command was printed at a machine that already has the runtime:\n%s", out)
	}
	if len(f.ran) != 0 {
		t.Errorf("a command ran on an idempotent re-run: %v", f.ran)
	}
}

// THE NON-INTERACTIVE REFUSAL: exit 3 and nothing installed, the same
// code and shape --inference uses for a consent it cannot obtain.
func TestRuntimeSetupRefusesUnderNonInteractive(t *testing.T) {
	f := newFakeRuntimeSetup(t, dockerReady(), inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.setup.nonInteractive = true
	})
	code, out := f.run()

	if code != SetupExitRefused {
		t.Fatalf("exit = %d, want %d (a question could not be asked)\n%s", code, SetupExitRefused, out)
	}
	if !strings.Contains(flat(out), "Nothing was installed: --non-interactive cannot answer that question.") {
		t.Errorf("output:\n%s", out)
	}
	if len(f.ran) != 0 {
		t.Errorf("a command ran under --non-interactive: %v", f.ran)
	}
	// The commands are still PRINTED, so a scripted caller's log says
	// what it would have run.
	if !strings.Contains(out, "docker run") {
		t.Errorf("the refusal must still show what it would have run:\n%s", out)
	}
}

// A DECLINED question installs nothing either, and says so without the
// --non-interactive explanation, which would be wrong.
func TestRuntimeSetupRespectsANo(t *testing.T) {
	f := newFakeRuntimeSetup(t, dockerReady(), inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.setup.in = strings.NewReader("n\n")
	})
	code, out := f.run()

	if code != SetupExitRefused {
		t.Fatalf("exit = %d, want %d", code, SetupExitRefused)
	}
	if !strings.Contains(out, "Nothing was installed.") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, "--non-interactive") {
		t.Errorf("a hand-typed no was explained as a --non-interactive refusal:\n%s", out)
	}
}

// THE COMMANDS ARE PRINTED BEFORE THE QUESTION, and the question covers
// all of them at once. Asking per command lets somebody approve half an
// install.
func TestRuntimeSetupPrintsBeforeAsking(t *testing.T) {
	f := newFakeRuntimeSetup(t, dockerReady(), inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.present, f.detail = true, "http://127.0.0.1:8880"
	})
	_, out := f.run()

	cmdAt := strings.Index(out, "docker run")
	askAt := strings.Index(out, "Run them now?")
	if cmdAt < 0 || askAt < 0 {
		t.Fatalf("output:\n%s", out)
	}
	if cmdAt > askAt {
		t.Fatalf("the question came before the commands:\n%s", out)
	}
}

// THE LABEL APPEARS ONLY AFTER THE RUNTIME ANSWERS A PROBE (#400's
// acceptance criterion). A container that started and then died is a
// command that exited zero and a runtime that is not there.
func TestRuntimeSetupReportsAProbeNotAnExitCode(t *testing.T) {
	f := newFakeRuntimeSetup(t, dockerReady(), inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.present = false // the install "succeeded" and nothing answers
	})
	code, out := f.run()

	if code != SetupExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(flat(out), "the label appears only once the runtime answers a probe") {
		t.Errorf("a runtime that never answered was reported as advertised:\n%s", out)
	}
	if strings.Contains(out, "The cluster sees runtime:") {
		t.Errorf("the cluster was promised a label for a runtime that is not answering:\n%s", out)
	}
}

// And when it DOES answer, the sentence still refuses to overstate it:
// labels bind at Register, so the cluster sees it after a reconnect.
func TestRuntimeSetupNeverSaysAvailableNow(t *testing.T) {
	f := newFakeRuntimeSetup(t, dockerReady(), inference.RuntimeKokoro, func(f *fakeRuntimeSetup) {
		f.present, f.detail = true, "http://127.0.0.1:8880 (0.2.4)"
	})
	_, out := f.run()

	if !strings.Contains(flat(out), "once this worker reconnects, which takes a minute or two") {
		t.Errorf("output:\n%s", out)
	}
	for _, forbidden := range []string{"available now", "is now available", "ready to use"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("the closing line overstated what happened (%q):\n%s", forbidden, out)
		}
	}
}

// A refusal exits with the prerequisite code and runs nothing.
func TestRuntimeSetupRefusalExitsPrereq(t *testing.T) {
	h := dockerReady()
	h.Docker = inference.DockerFacts{}
	f := newFakeRuntimeSetup(t, h, inference.RuntimeKokoro, nil)
	code, out := f.run()

	if code != SetupExitPrereq {
		t.Fatalf("exit = %d, want %d\n%s", code, SetupExitPrereq, out)
	}
	if len(f.ran) != 0 {
		t.Errorf("a command ran behind a refusal: %v", f.ran)
	}
	if !strings.Contains(flat(out), "Docker is not installed") {
		t.Errorf("output:\n%s", out)
	}
}

// The macOS note is printed WITH the commands, because a reader who
// knows the model runtime's "no Docker on macOS" rule will otherwise
// read this as a bug.
func TestRuntimeSetupExplainsDockerOnMac(t *testing.T) {
	h := dockerReady()
	h.GOOS, h.GOARCH = "darwin", "arm64"
	f := newFakeRuntimeSetup(t, h, inference.RuntimeKokoro, nil)
	_, out := f.run()

	if !strings.Contains(flat(out),
		"an 82M-parameter speech model runs faster than real time on a CPU") {
		t.Errorf("the macOS note was not printed:\n%s", out)
	}
}
