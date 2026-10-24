package worker

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

// The command is the only surface this feature gives a person, so what
// it PRINTS is what is asserted here -- the preamble, each refusal, and
// the closing block, in the shape the epic's terminal-UX brief fixes.
// The exit codes are asserted beside them, because an install script
// reading `$?` and a person reading the prose are two callers of the
// same run and both have to be right.

// -----------------------------------------------------------------------------
// Fixture machines
// -----------------------------------------------------------------------------

// noPath is a machine where nothing is on PATH. EVERY fixture sets it:
// inference.Host falls back to exec.LookPath when the field is nil, so a
// fixture without one would find whatever the CI runner has installed
// and a "no Ollama" case would pass or fail on the runner's image.
func noPath(string) (string, error) { return "", errors.New("not found") }

// flat collapses the wrapping so a sentence can be asserted as one
// string. WHERE a paragraph wraps is a rendering decision about the
// terminal it is read on; WHAT it says is the contract, and a test that
// pinned the wrap points would fail on a reword that changed neither.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func pathWith(names ...string) func(string) (string, error) {
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
	}
	return func(name string) (string, error) {
		if have[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

// macServing: an Apple Silicon machine already running Ollama.
func macServing() inference.Host {
	return inference.Host{
		GOOS: "darwin", GOARCH: "arm64",
		Floor:         models.FloorVerdict{Met: true, Detail: "Apple M2 Max, 32 GB, macOS 15"},
		Hardware:      macHardware(),
		OllamaServing: true,
		FreeDisk:      500_000_000_000,
		LookPath:      noPath,
	}
}

// macHardware is the inventory that goes with the Mac fixtures. It
// MATCHES their floor Detail on purpose: the preamble prints the floor
// verdict and the class on adjacent lines, and a fixture whose two
// halves disagreed would be asserting a screen no real machine shows.
func macHardware() hardware.Inventory {
	return hardware.Inventory{
		Chip:        "Apple M2 Max",
		MemoryBytes: 32 << 30,
		GPU:         &hardware.GPU{Name: "Apple M2 Max", VRAMBytes: 32 << 30, Backend: hardware.BackendMetal},
		CPUCores:    12,
		OSVersion:   "macOS 15.1",
	}
}

// macNeedsOllama: Apple Silicon with Homebrew and no runtime.
func macNeedsOllama() inference.Host {
	return inference.Host{
		GOOS: "darwin", GOARCH: "arm64",
		Floor:    models.FloorVerdict{Met: true, Detail: "Apple M2 Max, 32 GB, macOS 15"},
		Hardware: macHardware(),
		FreeDisk: 500_000_000_000,
		LookPath: pathWith("brew"),
	}
}

// belowFloor: the machine the floor exists to keep out.
func belowFloor() inference.Host {
	return inference.Host{
		GOOS: "darwin", GOARCH: "arm64",
		Floor: models.FloorVerdict{
			Reason: "this machine has 8 GB of unified memory; the floor is 16 GB.",
			Detail: "apple silicon, 8 GB, macOS 15",
		},
		Hardware: hardware.Inventory{
			Chip:        "Apple M1",
			MemoryBytes: 8 << 30,
			GPU:         &hardware.GPU{Name: "Apple M1", VRAMBytes: 8 << 30, Backend: hardware.BackendMetal},
			OSVersion:   "macOS 15.1",
		},
		LookPath: noPath,
	}
}

// linuxNoToolkit: Docker answering, the GPU unreachable from a container.
func linuxNoToolkit() inference.Host {
	return inference.Host{
		GOOS: "linux", GOARCH: "amd64",
		Floor: models.FloorVerdict{Met: true, Detail: "NVIDIA GeForce RTX 4090, 24 GB VRAM"},
		Hardware: hardware.Inventory{
			Chip:        "AMD Ryzen 9 7950X",
			MemoryBytes: 128 << 30,
			GPU:         &hardware.GPU{Name: "NVIDIA GeForce RTX 4090", VRAMBytes: 24 << 30, Backend: hardware.BackendCUDA},
			CPUCores:    16,
			OSVersion:   "Ubuntu 24.04.1 LTS",
		},
		Docker: inference.DockerFacts{
			CLIPresent: true, Present: true, Version: "27.1.1",
			GPUVendor: inference.GPUVendorNVIDIA,
		},
		FreeDisk: 900_000_000_000,
		LookPath: noPath,
	}
}

// linuxReady: Docker with GPU passthrough and no container yet.
func linuxReady() inference.Host {
	h := linuxNoToolkit()
	h.Docker.GPUToolkit = true
	return h
}

// linuxNative is the machine that started all this: an RTX 4090 on a
// distribution with no container toolkit in any repository, Docker
// running the k3d cluster beside it, and nothing listening on 11434.
// Under the 2026-09-08 record it needs nothing from root.
func linuxNative() inference.Host {
	h := linuxNoToolkit()
	h.Native = inference.DefaultNativeFacts("/home/op", "")
	h.Native.Vendor = inference.GPUVendorNVIDIA
	h.Native.Devices = true
	h.LookPath = pathWith("docker", "nvidia-smi", "systemctl")
	return h
}

// -----------------------------------------------------------------------------
// The fake machine the flow runs against
// -----------------------------------------------------------------------------

type fakeSetup struct {
	t     *testing.T
	setup *inferenceSetup
	out   *strings.Builder

	pulled       []string
	allowed      []string
	ran          [][]string
	consentWasNi bool
	readvertised bool

	pullErr     error
	allowErr    error
	readvertErr error
	stageErr    error
	progress    map[string][]inference.Progress
	inv         models.Inventory

	// The native Linux stage, and the order everything happened in:
	// "stage <runtime dir>" and "run <argv>" lines, because the stage
	// running BEFORE the first command is the property worth pinning.
	staged []inference.Stage
	events []string
}

func newFakeSetup(t *testing.T, host inference.Host) *fakeSetup {
	t.Helper()
	f := &fakeSetup{t: t, out: &strings.Builder{}, progress: map[string][]inference.Progress{}}
	f.setup = &inferenceSetup{
		out:        f.out,
		in:         strings.NewReader(""),
		policyPath: "/home/op/.memql/policy.yaml",
		gather:     func(context.Context) (inference.Host, error) { return host, nil },
		base:       func() string { return "http://127.0.0.1:11434" },
		// The REAL InstallRuntime decides on the consent, so "a nil
		// consent counts as no" is exercised rather than restated: a
		// fake that reimplemented the rule could agree with a version
		// of it that no longer exists.
		install: func(ctx context.Context, p inference.Plan, consent func([]string) bool, _ inference.Runner, _ inference.Stager) error {
			f.consentWasNi = consent == nil
			return inference.InstallRuntime(ctx, p, consent, func(_ context.Context, argv []string) error {
				f.ran = append(f.ran, argv)
				f.events = append(f.events, "run "+strings.Join(argv, " "))
				return nil
			}, func(_ context.Context, st inference.Stage, _ func(inference.Progress)) error {
				f.staged = append(f.staged, st)
				f.events = append(f.events, "stage "+st.RuntimeDir)
				return f.stageErr
			})
		},
		pull: func(_ context.Context, _, model string, onProgress func(inference.Progress)) error {
			f.pulled = append(f.pulled, model)
			for _, p := range f.progress[model] {
				onProgress(p)
			}
			return f.pullErr
		},
		allow: func(_ string, ids ...string) error {
			f.allowed = append(f.allowed, ids...)
			return f.allowErr
		},
		readvertise: func(context.Context) error { f.readvertised = true; return f.readvertErr },
		inventory:   func(context.Context) models.Inventory { return f.inv },
		pullAllowed: func() bool { return true },
	}
	return f
}

func (f *fakeSetup) run() (int, string) {
	f.t.Helper()
	code := SetupExitCode(f.setup.run(context.Background()))
	return code, f.out.String()
}

// -----------------------------------------------------------------------------
// The happy path
// -----------------------------------------------------------------------------

func TestSetupInference_HappyPath(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.progress["qwen3.8:27b"] = onePull(4_661_211_808)
	f.progress["qwen3-embedding:0.6b"] = onePull(274_302_450)
	f.inv = servingInventory(
		offeredModel("qwen3.8:27b", models.Attributes{
			ContextWindow: 131072, StructuredOutput: true, Tools: true,
			Params: 8_030_000_000, Quant: "Q4_K_M", MaxConcurrent: 1,
		}),
		offeredModel("qwen3-embedding:0.6b", models.Attributes{
			ContextWindow: 2048, Embeddings: true,
			Params: 137_000_000, Quant: "F16", MaxConcurrent: 1,
		}),
	)

	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d", code, SetupExitOK)
	}

	// The preamble is the whole diagnosis in three lines.
	for _, want := range []string{
		"Setting this machine up to run local models.",
		"  Hardware   Apple M2 Max, 32 GB, macOS 15 -- meets the floor",
		"  Runtime    Ollama, already running at http://127.0.0.1:11434",
		"  Class      24 -- 24.0 GB usable, 75% of 32 GB unified",
		"  Models     qwen3.8:27b, qwen3-embedding:0.6b",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the preamble must carry %q:\n%s", want, out)
		}
	}

	// Both defaults, in the order the plan states them: one general
	// model and one embedding model, because the operations this fleet
	// serves locally are both kinds.
	if got := strings.Join(f.pulled, ","); got != "qwen3.8:27b,qwen3-embedding:0.6b" {
		t.Errorf("pulled = %q, want the default pair", got)
	}
	if got := strings.Join(f.allowed, ","); got != "qwen3.8:27b,qwen3-embedding:0.6b" {
		t.Errorf("allowed = %q, want both models", got)
	}
	if !f.readvertised {
		t.Error("the running worker was never signalled, so the cluster would never see the models")
	}

	// The last line is the payoff: what the cluster will see, in the
	// cluster's own words.
	for _, want := range []string{
		"Allowed in /home/op/.memql/policy.yaml",
		"This machine now offers 2 models. The cluster will see:",
		"8B, Q4_K_M, 131072 context, tools, structured output",
		"137M, F16, 2048 context, embeddings",
		"The running worker was signalled and re-advertises within a minute or two.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the closing block must carry %q:\n%s", want, out)
		}
	}

	// Never "available now". Labels are bound at Register and Heartbeat
	// carries none, so a person sent to the Fleet page for a row that is
	// not due yet reads the delay as a failure.
	if strings.Contains(strings.ToLower(out), "available now") || strings.Contains(strings.ToLower(out), "immediately") {
		t.Errorf("the re-advertise sentence must not promise the cluster sees it at once:\n%s", out)
	}
}

// The same run on a TERMINAL: the bar is redrawn in place, and the
// closing block is byte-identical to the one a log file gets. The
// display is the only thing the TTY check changes.
func TestSetupInference_HappyPathOnATerminal(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.tty = true
	f.progress["qwen3.8:27b"] = onePull(4_661_211_808)
	f.progress["qwen3-embedding:0.6b"] = onePull(274_302_450)
	f.inv = servingInventory(
		offeredModel("qwen3.8:27b", models.Attributes{
			ContextWindow: 131072, StructuredOutput: true, Tools: true,
			Params: 8_030_000_000, Quant: "Q4_K_M", MaxConcurrent: 1,
		}),
		offeredModel("qwen3-embedding:0.6b", models.Attributes{
			ContextWindow: 2048, Embeddings: true,
			Params: 137_000_000, Quant: "F16", MaxConcurrent: 1,
		}),
	)

	code, out := f.run()
	t.Logf("transcript (carriage returns shown as \\r):\n%s", strings.ReplaceAll(out, "\r", "\\r"))

	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d", code, SetupExitOK)
	}
	if !strings.Contains(out, "\r") {
		t.Errorf("a terminal must be redrawn in place:\n%q", out)
	}
	for _, want := range []string{
		"Pulling qwen3.8:27b",
		"  4.7 GB total",
		"[========================================] 100%   4.7 GB / 4.7 GB",
		"Pulling qwen3-embedding:0.6b",
		"  274.3 MB total",
		"This machine now offers 2 models. The cluster will see:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the terminal transcript must carry %q:\n%s", want, out)
		}
	}
}

// The models the person asked for beat the defaults, and a duplicate is
// pulled once. Two pulls of the same id would also print it twice in the
// closing block, which reads as two models on the machine.
func TestSetupInference_ModelFlagOverridesTheDefaults(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.modelIDs = []string{"qwen2.5:7b", " qwen2.5:7b ", "", "hf.co/owner/repo:Q4_K_M"}
	f.inv = servingInventory(offeredModel("qwen2.5:7b", models.Attributes{ContextWindow: 32768}))

	code, out := f.run()
	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if got := strings.Join(f.pulled, ","); got != "qwen2.5:7b,hf.co/owner/repo:Q4_K_M" {
		t.Errorf("pulled = %q, want the ids as typed, deduplicated", got)
	}
	if strings.Contains(out, "qwen3.8:27b") {
		t.Errorf("a default must not be pulled beside an explicit --model:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

// A refusal from Decide is a PREREQUISITE (4), never a consent refusal
// (3). Nothing was asked and nothing was declined -- the machine is
// missing something -- and an install script that read this as "run it
// again interactively" would send the operator to a prompt that would
// refuse them identically.
func TestSetupInference_BelowTheFloorIsPrerequisite(t *testing.T) {
	f := newFakeSetup(t, belowFloor())
	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitPrereq {
		t.Errorf("exit code = %d, want %d (prerequisite missing)", code, SetupExitPrereq)
	}
	if !strings.Contains(out, "This machine cannot serve local models.") {
		t.Errorf("the refusal must say so plainly:\n%s", out)
	}
	// The FLOOR'S OWN sentence, not a second one written here.
	if !strings.Contains(flat(out), "the floor is 16 GB") {
		t.Errorf("the floor's own reason must be printed verbatim:\n%s", out)
	}
	// Without this line a person reads "cannot serve local models" and
	// concludes their pairing is broken.
	if !strings.Contains(flat(out), "It stays a full worker for everything else.") {
		t.Errorf("the refusal must say the machine is still a worker:\n%s", out)
	}
	if len(f.pulled) != 0 || len(f.allowed) != 0 {
		t.Errorf("a refused machine must pull and allow nothing; pulled=%v allowed=%v", f.pulled, f.allowed)
	}
}

// Docker present, no GPU toolkit: the same code, a different sentence,
// and the sudo commands are LABELLED as the person's to run. A line the
// cockpit prints and a line the cockpit runs look identical on screen.
// Reachable only under --runtime docker now: without the flag the same
// machine takes the native path below and needs nothing from root.
func TestSetupInference_NoGPUToolkitNamesThePackageAndTheSudoRule(t *testing.T) {
	f := newFakeSetup(t, linuxNoToolkit())
	f.setup.runtimeFlag = "docker"
	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitPrereq {
		t.Errorf("exit code = %d, want %d", code, SetupExitPrereq)
	}
	if !strings.Contains(out, "This machine cannot serve local models yet.") {
		t.Errorf("a fixable refusal keeps the \"yet\":\n%s", out)
	}
	if !strings.Contains(flat(out), "nvidia-container-toolkit") {
		t.Errorf("the refusal must name the package:\n%s", out)
	}
	if !strings.Contains(flat(out), "for you to run yourself") {
		t.Errorf("a printed sudo command must be labelled as the person's to run:\n%s", out)
	}
	// And the way out, which is the whole reason the flag exists.
	if !strings.Contains(flat(out), "drop --runtime docker") {
		t.Errorf("a Docker refusal must name the native default as the way out:\n%s", out)
	}
	if len(f.staged) != 0 || len(f.ran) != 0 {
		t.Errorf("a refusal must stage and run nothing; staged %d, ran %v", len(f.staged), f.ran)
	}
}

// THE MACHINE THAT STARTED ALL THIS, without the flag: the runtime is
// staged as the user, the unit enabled, and nothing about Docker, the
// toolkit or sudo reaches the screen.
func TestSetupInference_LinuxDefaultStagesTheRuntimeAsTheUser(t *testing.T) {
	f := newFakeSetup(t, linuxNative())
	f.setup.in = strings.NewReader("y\n")
	f.inv = servingInventory(offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 131072}))

	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	// Say it before you do it, in both halves: the stage as sentences,
	// the commands as the lines that run.
	for _, want := range []string{
		"Ollama is not installed. This machine needs it to serve models.",
		"Ollama will run as your user, from ~/.memql",
		"This will:",
		"  download ollama-linux-amd64.tar.zst from https://github.com/ollama/ollama/releases/latest/download",
		"unpack it into /home/op/.memql/ollama/runtime",
		"  write /home/op/.config/systemd/user/memql-ollama.service",
		"and then run:",
		"  systemctl --user daemon-reload",
		"  systemctl --user enable --now memql-ollama.service",
		"Run them now? [y/N]",
		"The runtime is installed and started.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the install ask must carry %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{"sudo", "nvidia-container-toolkit", "docker run"} {
		if strings.Contains(out, banned) {
			t.Errorf("the native path must not mention %q:\n%s", banned, out)
		}
	}
	// The stage ran ONCE, before the first command, and named this
	// machine's archive and unit.
	want := []string{
		"stage /home/op/.memql/ollama/runtime",
		"run systemctl --user daemon-reload",
		"run systemctl --user enable --now memql-ollama.service",
	}
	if strings.Join(f.events, "|") != strings.Join(want, "|") {
		t.Errorf("events = %v, want %v", f.events, want)
	}
	if len(f.staged) != 1 || len(f.staged[0].Archives) != 1 || f.staged[0].Archives[0].Name != "ollama-linux-amd64.tar.zst" {
		t.Fatalf("staged = %+v, want the amd64 archive", f.staged)
	}
	if f.staged[0].UnitPath != "/home/op/.config/systemd/user/memql-ollama.service" {
		t.Errorf("unit at %q", f.staged[0].UnitPath)
	}
	if len(f.pulled) == 0 {
		t.Error("an installed runtime must go on to pull")
	}
}

// A unit that was enabled and never answered is exit 5, and the failure
// names the log: an install command exiting zero is a service asked to
// start, not one that listens, and the next step pulls against the socket.
func TestSetupInference_RuntimeSilentAfterInstallIsOperationFailed(t *testing.T) {
	f := newFakeSetup(t, linuxNative())
	f.setup.in = strings.NewReader("y\n")
	f.setup.ready = func(context.Context, string) error {
		return errors.New("nothing answered at http://127.0.0.1:11434 within 30s")
	}

	err := f.setup.run(context.Background())
	if code := SetupExitCode(err); code != SetupExitOpFailed {
		t.Fatalf("exit code = %d, want %d: %v", code, SetupExitOpFailed, err)
	}
	if err == nil || !strings.Contains(err.Error(), "Its log is /home/op/.memql/state/ollama.log") {
		t.Errorf("the failure must name the runtime's log: %v", err)
	}
	if len(f.pulled) != 0 {
		t.Errorf("nothing may be pulled against a runtime that is not answering; pulled %v", f.pulled)
	}
}

// --non-interactive on the native path: the stage and the commands are on
// screen, nothing is fetched, nothing is written.
func TestSetupInference_NonInteractiveStagesNothing(t *testing.T) {
	f := newFakeSetup(t, linuxNative())
	f.setup.nonInteractive = true
	code, out := f.run()
	if code != SetupExitRefused {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitRefused, out)
	}
	if len(f.staged) != 0 || len(f.ran) != 0 {
		t.Errorf("staged %d, ran %v; want nothing without an answer", len(f.staged), f.ran)
	}
	if !strings.Contains(out, "download ollama-linux-amd64.tar.zst") {
		t.Errorf("the stage must be on screen even though it did not run:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// Consent, and exit 3
// -----------------------------------------------------------------------------

// --non-interactive that would have had to ask exits 3: "refused,
// required confirmation not provided". Not 4 (nothing is absent that the
// operator has to install first -- this command would have installed it)
// and not 5 (nothing was attempted). The installers key their whole
// behaviour off telling those apart.
func TestSetupInference_NonInteractiveInstallExitsThreeAndInstallsNothing(t *testing.T) {
	f := newFakeSetup(t, macNeedsOllama())
	f.setup.nonInteractive = true

	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitRefused {
		t.Fatalf("exit code = %d, want %d (refused: no confirmation)", code, SetupExitRefused)
	}
	if !f.consentWasNi {
		t.Error("--non-interactive must pass a nil consent, which InstallRuntime reads as no")
	}
	if len(f.ran) != 0 {
		t.Errorf("nothing may be installed without an answer; ran %v", f.ran)
	}
	// Say it before you do it: the commands are on screen even though
	// none of them ran.
	for _, want := range []string{
		"Ollama is not installed. This machine needs it to serve models.",
		"These commands will run:",
		"  brew install ollama",
		"  brew services start ollama",
		"Nothing was installed: --non-interactive cannot answer that question.",
		"Run this again without it, or run the commands above yourself.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the install ask must carry %q:\n%s", want, out)
		}
	}
	// The question is not printed when there is nobody to answer it.
	if strings.Contains(out, "[y/N]") {
		t.Errorf("--non-interactive must not print a prompt it will not read:\n%s", out)
	}
	// Asked once, where a Linux operator's colleague will ask it.
	if !strings.Contains(flat(out), "Docker is not offered on macOS") {
		t.Errorf("the macOS install ask must say why Docker is not on offer:\n%s", out)
	}
	if len(f.pulled) != 0 {
		t.Errorf("a refused install must not go on to pull; pulled %v", f.pulled)
	}
}

// Anything but an explicit yes is a no, and the exit code is the same 3.
func TestSetupInference_DeclinedConsentExitsThree(t *testing.T) {
	for _, answer := range []string{"", "\n", "n\n", "no\n", "later\n"} {
		f := newFakeSetup(t, macNeedsOllama())
		f.setup.in = strings.NewReader(answer)
		code, out := f.run()
		if code != SetupExitRefused {
			t.Errorf("answer %q: exit code = %d, want %d\n%s", answer, code, SetupExitRefused, out)
		}
		if len(f.ran) != 0 {
			t.Errorf("answer %q: ran %v, want nothing", answer, f.ran)
		}
		if !strings.Contains(out, "Run them now? [y/N]") {
			t.Errorf("answer %q: the question must be asked with the capital on the default:\n%s", answer, out)
		}
	}
}

func TestSetupInference_AcceptedConsentRunsTheCommandsInOrder(t *testing.T) {
	f := newFakeSetup(t, macNeedsOllama())
	f.setup.in = strings.NewReader("y\n")
	f.inv = servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072}))

	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if len(f.ran) != 2 ||
		strings.Join(f.ran[0], " ") != "brew install ollama" ||
		strings.Join(f.ran[1], " ") != "brew services start ollama" {
		t.Fatalf("ran %v, want both brew commands in order", f.ran)
	}
	if !strings.Contains(out, "The runtime is installed and started.") {
		t.Errorf("the install must say what is now true:\n%s", out)
	}
	if len(f.pulled) == 0 {
		t.Error("an accepted install must go on to pull")
	}
}

// -----------------------------------------------------------------------------
// --runtime
// -----------------------------------------------------------------------------

// A combination the platform cannot serve is REFUSED, never ignored. An
// operator who passed --runtime docker on a Mac and watched a successful
// setup would believe their models came from a container, and every
// later question they asked would start from a false premise.
func TestSetupInference_RuntimeFlagRefusesDockerOnMacOS(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.runtimeFlag = "docker"
	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitUsage {
		t.Errorf("exit code = %d, want %d (bad invocation)", code, SetupExitUsage)
	}
	if !strings.Contains(flat(out), "a container has no access to the GPU") {
		t.Errorf("the refusal must say why:\n%s", out)
	}
	if len(f.pulled) != 0 {
		t.Errorf("a refused --runtime must pull nothing; pulled %v", f.pulled)
	}
}

// The one override that IS honoured: a Linux machine already serving
// natively has nothing to install, so nothing about the platform is
// being worked around.
func TestSetupInference_RuntimeNativeAcceptedWhenLinuxAlreadyServes(t *testing.T) {
	h := linuxReady()
	h.OllamaServing = true
	f := newFakeSetup(t, h)
	f.setup.runtimeFlag = "native"
	f.inv = servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072}))

	code, out := f.run()
	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if !strings.Contains(out, "Ollama, already running") {
		t.Errorf("the preamble must report the runtime it found:\n%s", out)
	}
}

// --runtime native on Linux names the default and changes nothing.
func TestSetupInference_RuntimeNativeOnLinuxIsTheDefault(t *testing.T) {
	f := newFakeSetup(t, linuxNative())
	f.setup.runtimeFlag = "native"
	f.setup.in = strings.NewReader("y\n")
	f.inv = servingInventory(offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 131072}))
	code, out := f.run()
	if code != SetupExitOK {
		t.Errorf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if len(f.staged) != 1 {
		t.Errorf("staged %d, want the native runtime staged once", len(f.staged))
	}
}

// --runtime docker on a Linux machine with the toolkit takes the
// container path, and the disk figure follows it to Docker's volume.
func TestSetupInference_RuntimeDockerOnLinuxTakesTheContainer(t *testing.T) {
	h := linuxReady()
	h.DockerFreeDisk = 123_000_000_000
	f := newFakeSetup(t, h)
	f.setup.runtimeFlag = "docker"
	f.setup.in = strings.NewReader("y\n")
	f.inv = servingInventory(offeredModel("qwen3.8:27b", models.Attributes{ContextWindow: 131072}))
	code, out := f.run()
	if code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if len(f.staged) != 0 {
		t.Errorf("the container path stages nothing; staged %+v", f.staged)
	}
	if len(f.ran) != 1 || f.ran[0][0] != "docker" {
		t.Errorf("ran %v, want the docker run line", f.ran)
	}
}

func TestSetupInference_UnknownRuntimeIsUsage(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.runtimeFlag = "podman"
	err := f.setup.run(context.Background())
	if got := SetupExitCode(err); got != SetupExitUsage {
		t.Errorf("exit code = %d, want %d", got, SetupExitUsage)
	}
	if err == nil || !strings.Contains(err.Error(), "--runtime takes docker or native") {
		t.Errorf("err = %v, want a sentence naming the two values", err)
	}
}

// -----------------------------------------------------------------------------
// Failures during the work
// -----------------------------------------------------------------------------

func TestSetupInference_PullFailureIsOperationFailed(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.pullErr = errors.New("pulling llama3.1:8b: the model runtime could not pull the model: no such model")

	code, out := f.run()
	if code != SetupExitOpFailed {
		t.Errorf("exit code = %d, want %d", code, SetupExitOpFailed)
	}
	// The runtime's own words: they are the only thing that tells "no
	// such model" from "no space left on device".
	if !strings.Contains(flat(out), "no such model") {
		t.Errorf("the runtime's own message must reach the terminal:\n%s", out)
	}
	if len(f.allowed) != 0 {
		t.Errorf("a model that failed to pull must never be allowed; allowed %v", f.allowed)
	}
	if f.readvertised {
		t.Error("nothing changed, so nothing should have been re-advertised")
	}
}

// models.pull: false is a refusal the operator can lift, and it is named
// with the file it lives in. A machine that quietly ignored a pull is
// indistinguishable from one that is offline.
func TestSetupInference_PullBlockedByPolicy(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.pullAllowed = func() bool { return false }

	code, out := f.run()
	if code != SetupExitPrereq {
		t.Errorf("exit code = %d, want %d", code, SetupExitPrereq)
	}
	if !strings.Contains(flat(out), "models.pull is false in /home/op/.memql/policy.yaml") {
		t.Errorf("the refusal must name the switch and the file:\n%s", out)
	}
	if len(f.pulled) != 0 {
		t.Errorf("nothing may be pulled; pulled %v", f.pulled)
	}
}

// A worker that is not running is an ORDINARY state -- a machine paired
// and not yet started -- and policy.yaml is already on disk, so the
// command succeeds and says what is true.
func TestSetupInference_NoRunningWorkerStillSucceeds(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.readvertErr = inference.ErrNoWorker
	f.inv = servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072}))

	code, out := f.run()
	if code != SetupExitOK {
		t.Errorf("exit code = %d, want %d\n%s", code, SetupExitOK, out)
	}
	if !strings.Contains(flat(out), "reads policy.yaml at startup") {
		t.Errorf("the sentence must say what happens next:\n%s", out)
	}
}

// -----------------------------------------------------------------------------
// The progress display
// -----------------------------------------------------------------------------

// onePull scripts a single-layer pull the way Ollama reports one.
func onePull(total uint64) []inference.Progress {
	return []inference.Progress{
		{Status: "pulling manifest"},
		{Status: "pulling 8934d96d3f08", Total: total},
		{Status: "pulling 8934d96d3f08", Total: total, Completed: total / 2},
		{Status: "pulling 8934d96d3f08", Total: total, Completed: total},
		{Status: "verifying sha256 digest"},
		{Status: "writing manifest"},
		{Status: "success"},
	}
}

// twoLayerPull is the shape a naive bar gets wrong: a SMALL layer
// finishes before a LARGE one is announced, so a single bar keyed on the
// runtime's per-layer counts reaches the end and starts again.
func twoLayerPull() []inference.Progress {
	return []inference.Progress{
		{Status: "pulling manifest"},
		{Status: "pulling aaaaaaaaaaaa", Total: 100_000_000},
		{Status: "pulling aaaaaaaaaaaa", Total: 100_000_000, Completed: 100_000_000},
		{Status: "pulling bbbbbbbbbbbb", Total: 4_000_000_000},
		{Status: "pulling bbbbbbbbbbbb", Total: 4_000_000_000, Completed: 2_000_000_000},
		{Status: "pulling bbbbbbbbbbbb", Total: 4_000_000_000, Completed: 4_000_000_000},
		{Status: "verifying sha256 digest"},
		{Status: "success"},
	}
}

var percentOnScreen = regexp.MustCompile(`(\d+)%`)

func percentages(t *testing.T, out string) []int {
	t.Helper()
	var out2 []int
	for _, m := range percentOnScreen.FindAllStringSubmatch(out, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparseable percentage %q", m[1])
		}
		out2 = append(out2, n)
	}
	return out2
}

// THE NUMBERS ON SCREEN NEVER GO BACKWARDS. Ollama restarts Completed
// and Total for every blob, so the bytes are accumulated across the
// layers seen and the bar is HELD rather than rewound when a later
// layer's total lands. A bar that reaches the end and jumps to zero is
// indistinguishable from a download that failed and restarted, which is
// the whole failure this display exists to prevent.
func TestPullDisplay_PerLayerResetsNeverRewindTheScreen(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var b strings.Builder
		d := newPullDisplay(&b, tty, 0, nil)
		var lastCompleted, lastTotal uint64
		var lastPercent int
		for _, p := range twoLayerPull() {
			d.handle(p)
			if d.completed < lastCompleted {
				t.Errorf("tty=%v: completed went backwards: %d then %d", tty, lastCompleted, d.completed)
			}
			if d.total < lastTotal {
				t.Errorf("tty=%v: the total went backwards: %d then %d", tty, lastTotal, d.total)
			}
			if d.percent < lastPercent {
				t.Errorf("tty=%v: the percentage went backwards: %d then %d", tty, lastPercent, d.percent)
			}
			lastCompleted, lastTotal, lastPercent = d.completed, d.total, d.percent
		}
		d.finish()

		seen := percentages(t, b.String())
		for i := 1; i < len(seen); i++ {
			if seen[i] < seen[i-1] {
				t.Errorf("tty=%v: a percentage on screen went backwards (%d after %d):\n%s",
					tty, seen[i], seen[i-1], b.String())
			}
		}
		// The aggregate is 4.1 GB, not either layer on its own.
		if !strings.Contains(b.String(), "4.1 GB") {
			t.Errorf("tty=%v: the bytes must aggregate across layers:\n%s", tty, b.String())
		}
		// A total that GREW is announced rather than swapped silently
		// under the bar.
		if !strings.Contains(b.String(), "100 MB total") || !strings.Contains(b.String(), "4.1 GB total") {
			t.Errorf("tty=%v: each growth of the known total must be stated:\n%s", tty, b.String())
		}
	}
}

// Redraw in place ONLY on a terminal. Two thousand carriage returns in
// an install script's log file is the failure to avoid; a log that says
// nothing at all for the eight minutes of a four-gigabyte download is
// the other one, so a decile counts as a state change.
func TestPullDisplay_NonTTYPrintsLinesAndNoCarriageReturns(t *testing.T) {
	var b strings.Builder
	d := newPullDisplay(&b, false, 0, nil)
	// A hundred events inside one layer: the log must not grow with
	// them.
	d.handle(inference.Progress{Status: "pulling manifest"})
	for i := 0; i <= 100; i++ {
		d.handle(inference.Progress{
			Status: "pulling 8934d96d3f08", Total: 1_000_000_000,
			Completed: uint64(i) * 10_000_000,
		})
	}
	d.handle(inference.Progress{Status: "success"})
	d.finish()

	out := b.String()
	if strings.Contains(out, "\r") {
		t.Errorf("a non-terminal must never be redrawn in place:\n%q", out)
	}
	lines := strings.Count(out, "\n")
	if lines > 16 {
		t.Errorf("a non-terminal got %d lines for one pull; one per state change is the rule:\n%s", lines, out)
	}
	if lines < 3 {
		t.Errorf("a non-terminal got %d lines; a silent multi-gigabyte download is a hang to whoever reads the log:\n%s", lines, out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("the log must reach the end:\n%s", out)
	}
}

func TestPullDisplay_TTYRedrawsInPlace(t *testing.T) {
	var b strings.Builder
	d := newPullDisplay(&b, true, 0, nil)
	for _, p := range onePull(4_661_211_808) {
		d.handle(p)
	}
	d.finish()

	out := b.String()
	if !strings.Contains(out, "\r") {
		t.Errorf("a terminal must be redrawn in place:\n%q", out)
	}
	if !strings.Contains(out, "[========================================] 100%") {
		t.Errorf("the bar must fill:\n%s", out)
	}
	if !strings.Contains(out, "4.7 GB / 4.7 GB") {
		t.Errorf("bytes, not vibes -- completed and total must both be on the line:\n%s", out)
	}
	// finish() closes the in-place line, so whatever is printed next
	// starts on a fresh one rather than halfway along a progress bar.
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("finish must end the line:\n%q", out)
	}
}

// -----------------------------------------------------------------------------
// Disk
// -----------------------------------------------------------------------------

// THE DISK CHECK CANNOT BE A PRE-FLIGHT. The model's size is knowable
// only once the runtime's own stream reports a layer total, so the check
// lives in the progress handler -- and when it fires it names BOTH
// numbers and the model, which is what design section 5 asks for.
func TestPullDisplay_RefusesWhenThePullWouldExceedFreeDisk(t *testing.T) {
	var b strings.Builder
	stopped := false
	d := newPullDisplay(&b, false, 2_100_000_000, func() { stopped = true })
	d.handle(inference.Progress{Status: "pulling manifest"})
	d.handle(inference.Progress{Status: "pulling 8934d96d3f08", Total: 4_661_211_808})

	if !stopped {
		t.Error("the pull must be stopped rather than run the volume out of space")
	}
	// Events that were already in flight when the stop landed draw
	// nothing more.
	d.handle(inference.Progress{Status: "pulling 8934d96d3f08", Total: 4_661_211_808, Completed: 4_661_211_808})
	if strings.Contains(b.String(), "100%") {
		t.Errorf("a stopped pull must not keep drawing:\n%s", b.String())
	}

	err := d.overflow("llama3.1:8b")
	if err == nil {
		t.Fatal("overflow() = nil, want a refusal")
	}
	for _, want := range []string{"llama3.1:8b", "4.7 GB", "2.1 GB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q: %v", want, err)
		}
	}
}

// Free space of zero is NOT ESTABLISHED rather than "no space": the
// statfs behind it fails on a volume this process cannot see, and
// refusing on a fact that could not be read would block a machine with a
// terabyte free.
func TestPullDisplay_UnknownFreeSpaceRefusesNothing(t *testing.T) {
	var b strings.Builder
	stopped := false
	d := newPullDisplay(&b, false, 0, func() { stopped = true })
	for _, p := range onePull(4_661_211_808) {
		d.handle(p)
	}
	if stopped {
		t.Error("an unreadable free-space figure must not stop a pull")
	}
	if d.overflow("llama3.1:8b") != nil {
		t.Error("an unreadable free-space figure must not produce a refusal")
	}
}

func TestSetupInference_DiskRefusalNamesBothNumbers(t *testing.T) {
	h := macServing()
	h.FreeDisk = 2_100_000_000
	f := newFakeSetup(t, h)
	// Keyed on the FIRST id of the recommended set, which is what the
	// flow will actually ask for. A fixture keyed on some other model
	// fires no progress at all, and the disk refusal this test exists
	// for is then never reached.
	f.progress["qwen3.8:27b"] = onePull(4_661_211_808)
	f.pullErr = errors.New("pulling qwen3.8:27b was cancelled: context canceled")

	code, out := f.run()
	t.Logf("transcript:\n%s", out)

	if code != SetupExitOpFailed {
		t.Errorf("exit code = %d, want %d", code, SetupExitOpFailed)
	}
	// The disk sentence WINS over the cancellation the stop produced:
	// "was cancelled" is true and says nothing an operator can act on.
	if strings.Contains(out, "context canceled") {
		t.Errorf("the cancellation must not be what is reported:\n%s", out)
	}
	for _, want := range []string{"not enough disk", "4.7 GB", "2.1 GB", "qwen3.8:27b"} {
		if !strings.Contains(strings.ToLower(flat(out)), strings.ToLower(want)) {
			t.Errorf("the refusal must name %q:\n%s", want, out)
		}
	}
	// The bar stops where the pull stopped. Cancellation is not instant,
	// and a bar that ran on to 100% for a download being aborted would
	// tell the operator the opposite of the sentence underneath it.
	if strings.Contains(out, "100%") {
		t.Errorf("the display must stop when the pull is stopped:\n%s", out)
	}
	if len(f.allowed) != 0 {
		t.Errorf("a model that was not pulled must never be allowed; allowed %v", f.allowed)
	}
}

// -----------------------------------------------------------------------------
// `memql worker models --pull / --allow`
// -----------------------------------------------------------------------------

// Pull BEFORE allow. Allowing a model puts it in front of the cluster's
// router and pulling one puts it on the disk; the other order leaves a
// window in which the machine advertises a model it cannot serve.
func TestModelActs_PullThenAllow(t *testing.T) {
	f := newFakeSetup(t, macServing())
	var order []string
	inner := f.setup.pull
	f.setup.pull = func(ctx context.Context, base, model string, on func(inference.Progress)) error {
		order = append(order, "pull:"+model)
		return inner(ctx, base, model, on)
	}
	innerAllow := f.setup.allow
	f.setup.allow = func(path string, ids ...string) error {
		order = append(order, "allow:"+strings.Join(ids, ","))
		return innerAllow(path, ids...)
	}

	if code := runModelActs(context.Background(), f.setup, []string{"llama3.1:8b"}, []string{"llama3.1:8b"}); code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, f.out.String())
	}
	if got := strings.Join(order, " "); got != "pull:llama3.1:8b allow:llama3.1:8b" {
		t.Errorf("order = %q, want the pull first", got)
	}
	if !f.readvertised {
		t.Error("an allow that nobody re-advertised is a model the cluster still cannot see")
	}
}

// A --pull on its own changes nothing the cluster can see, because
// models.allow is default-deny. Reporting a successful pull and stopping
// there is indistinguishable, from the portal, from a command that never
// ran.
func TestModelActs_PullAloneNamesTheAllowCommand(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.policyPath = t.TempDir() + "/policy.yaml"

	if code := runModelActs(context.Background(), f.setup, []string{"llama3.1:8b"}, nil); code != SetupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, SetupExitOK, f.out.String())
	}
	out := f.out.String()
	if len(f.allowed) != 0 {
		t.Errorf("--pull must not write models.allow; allowed %v", f.allowed)
	}
	if !strings.Contains(out, "memql worker models --allow llama3.1:8b") {
		t.Errorf("the next command must be named:\n%s", out)
	}
}

func TestModelActs_PullRefusedByPolicy(t *testing.T) {
	f := newFakeSetup(t, macServing())
	f.setup.pullAllowed = func() bool { return false }
	if code := runModelActs(context.Background(), f.setup, []string{"llama3.1:8b"}, nil); code != SetupExitPrereq {
		t.Errorf("exit code = %d, want %d", code, SetupExitPrereq)
	}
	if len(f.pulled) != 0 {
		t.Errorf("nothing may be pulled; pulled %v", f.pulled)
	}
}

// -----------------------------------------------------------------------------
// Flags, and the pre-flight that must not change
// -----------------------------------------------------------------------------

// THE EXISTING `worker setup` IS UNTOUCHED WHEN --inference IS ABSENT.
// It is reached by `worker pair` on every machine that enrolls, most of
// which are below the hardware floor and were never going to serve a
// model; folding a model setup into it would mean an enrollment could
// fail for a reason that has nothing to do with the permissions it was
// asking about.
func TestSetupFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want setupFlags
	}{
		{"bare", nil, setupFlags{}},
		// The pre-existing flag on its own must NOT reach the new path.
		{"non-interactive alone", []string{"--non-interactive"}, setupFlags{nonInteractive: true}},
		{"inference", []string{"--inference"}, setupFlags{inference: true}},
		{
			"everything",
			[]string{"--inference", "--non-interactive", "--runtime", "native", "--model", "a", "--model", "b"},
			setupFlags{inference: true, nonInteractive: true, runtimeFlag: "native", modelIDs: []string{"a", "b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSetupFlags(tc.args)
			if got.inference != tc.want.inference {
				t.Errorf("inference = %v, want %v", got.inference, tc.want.inference)
			}
			if got.nonInteractive != tc.want.nonInteractive {
				t.Errorf("nonInteractive = %v, want %v", got.nonInteractive, tc.want.nonInteractive)
			}
			if got.runtimeFlag != tc.want.runtimeFlag {
				t.Errorf("runtimeFlag = %q, want %q", got.runtimeFlag, tc.want.runtimeFlag)
			}
			if strings.Join(got.modelIDs, ",") != strings.Join(tc.want.modelIDs, ",") {
				t.Errorf("modelIDs = %v, want %v", got.modelIDs, tc.want.modelIDs)
			}
			// The config path is where policy.yaml is found, so it is
			// never empty.
			if got.configPath == "" {
				t.Error("configPath must default to the worker.yaml path")
			}
		})
	}
}

// The pre-flight the flag does not touch still succeeds on this build.
// A regression here aborts an enrollment with nothing wrong with it.
func TestSetupWithoutInferenceStillRunsThePreflight(t *testing.T) {
	if parseSetupFlags([]string{"--non-interactive"}).inference {
		t.Fatal("--non-interactive alone must not select the inference flow")
	}
	if BuildHasComputerUse() {
		t.Skip("computeruse build: the real pre-flight probes the machine")
	}
	if err := runSetupWizard(); err != nil {
		t.Errorf("the headless pre-flight returned %v, want nil", err)
	}
}

// -----------------------------------------------------------------------------
// Rendering helpers
// -----------------------------------------------------------------------------

// The bytes are spelled the way `ollama list` spells them: decimal
// units. A binary "4.3 GiB" beside Ollama's own "4.7 GB" for the same
// blob reads as two different files.
func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{274_302_450, "274.3 MB"},
		{4_661_211_808, "4.7 GB"},
		{2_100_000_000, "2.1 GB"},
		{16_000_000_000, "16 GB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The label carries the parameter COUNT because the engine ranks on it
// numerically; this is the spelling the operator matches against their
// own `ollama list`.
func TestHumanParams(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{8_030_000_000, "8B"},
		{7_620_000_000, "7.6B"},
		{137_000_000, "137M"},
		{70_000_000_000, "70B"},
		{512, "512 parameters"},
	} {
		if got := humanParams(tc.in); got != tc.want {
			t.Errorf("humanParams(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A refusal wrapped to a width nobody set is a 300-column line over SSH
// on a bad connection, which is a sentence nobody finishes reading.
func TestWrapText(t *testing.T) {
	lines := wrapText(strings.Repeat("word ", 60), inferenceWrapWidth)
	if len(lines) < 2 {
		t.Fatalf("a long paragraph must wrap; got %d line(s)", len(lines))
	}
	for _, l := range lines {
		if len(l) > inferenceWrapWidth {
			t.Errorf("line of %d characters exceeds the width: %q", len(l), l)
		}
	}
	if strings.Join(lines, " ") != strings.TrimSpace(strings.Repeat("word ", 60)) {
		t.Error("wrapping must not lose or add words")
	}
}

// The copy rules, held on every sentence this file composes: no emoji,
// no exclamation mark, no all-caps shouting, no "unfortunately". These
// are read by somebody who is blocked, in a terminal, over SSH.
func TestSetupInferenceCopyRules(t *testing.T) {
	transcripts := map[string]func() string{}
	for name, host := range map[string]inference.Host{
		"below floor":  belowFloor(),
		"no toolkit":   linuxNoToolkit(),
		"needs ollama": macNeedsOllama(),
		"serving":      macServing(),
	} {
		h := host
		transcripts[name] = func() string {
			f := newFakeSetup(t, h)
			f.setup.nonInteractive = true
			f.inv = servingInventory(offeredModel("llama3.1:8b", models.Attributes{ContextWindow: 131072}))
			_, out := f.run()
			return out
		}
	}
	for name, get := range transcripts {
		out := get()
		// Escaped rather than pasted so this file stays pure ASCII: a
		// check mark and an arrow are exactly what must never reach a
		// terminal over SSH on a bad line, and they must not reach this
		// source either.
		for _, banned := range []string{"!", "unfortunately", "Unfortunately", "ERROR:", "\u2713", "\u2192"} {
			if strings.Contains(out, banned) {
				t.Errorf("%s: the transcript carries %q:\n%s", name, banned, out)
			}
		}
		for _, r := range out {
			if r > 126 {
				t.Errorf("%s: the transcript carries a non-ASCII rune %q:\n%s", name, r, out)
			}
		}
	}
}

// os.Stderr is where an UNCLASSIFIED failure goes; a refusal already
// printed in full goes nowhere twice. A second copy under a composed
// refusal is a lower-case fragment in capitals the copy rules forbid.
func TestAlreadySaidPrintsNothingTwice(t *testing.T) {
	err := alreadySaid(SetupExitPrereq)
	if err.Error() != "" {
		t.Errorf("alreadySaid must carry no message; got %q", err.Error())
	}
	if got := SetupExitCode(err); got != SetupExitPrereq {
		t.Errorf("SetupExitCode = %d, want %d", got, SetupExitPrereq)
	}
	// The other direction: a failure nobody composed prose for still
	// carries its message, so runInferenceSetup has something to print.
	if msg := setupFailed("this machine could not be inspected: %v", errors.New("boom")).Error(); msg == "" {
		t.Error("an unclassified failure must carry a message for stderr")
	}
}

func TestSetupInferenceRefusesUnavailableCustomEndpointBeforeInstall(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:11435", "http://remote.example:11434", "https://localhost:11434"} {
		t.Run(base, func(t *testing.T) {
			f := newFakeSetup(t, linuxNative())
			f.setup.base = func() string { return base }
			f.setup.in = strings.NewReader("yes\n")
			err := f.setup.run(context.Background())
			if SetupExitCode(err) != SetupExitPrereq || !strings.Contains(err.Error(), "OLLAMA_HOST") {
				t.Errorf("expected endpoint refusal naming OLLAMA_HOST, got %v", err)
			}
			if len(f.staged) > 0 || len(f.ran) > 0 || len(f.pulled) > 0 || len(f.allowed) > 0 {
				t.Errorf("mutated machine before endpoint refusal: events=%v pulled=%v", f.events, f.pulled)
			}
		})
	}
}
