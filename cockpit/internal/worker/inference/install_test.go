package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// recorder is the Runner seam: it records the argv of every command it
// was handed, in order, and runs nothing. Every test in this file asserts
// on that recording rather than on a machine's state, because the thing
// worth pinning is WHICH ARGV RAN -- a runtime install that quietly
// shelled out to something other than the line the person approved is the
// failure this package's consent step exists to prevent, and it would
// leave no trace on a machine that already had Homebrew.
type recorder struct {
	ran  [][]string
	fail map[int]error
}

func (r *recorder) run(_ context.Context, argv []string) error {
	i := len(r.ran)
	r.ran = append(r.ran, append([]string(nil), argv...))
	return r.fail[i]
}

func yes(commands []string) bool { return true }
func no(commands []string) bool  { return false }

// TestInstallRuntimeRunsThePlansCommandsInOrder. The order is load-bearing
// on macOS: `brew services start ollama` before `brew install ollama`
// starts a runtime that is not there yet.
func TestInstallRuntimeRunsThePlansCommandsInOrder(t *testing.T) {
	rec := &recorder{}
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama, installBrewOllamaStart}}

	if err := InstallRuntime(context.Background(), p, yes, rec.run, nil); err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	want := [][]string{
		{"brew", "install", "ollama"},
		{"brew", "services", "start", "ollama"},
	}
	if !reflect.DeepEqual(rec.ran, want) {
		t.Errorf("ran %v, want %v", rec.ran, want)
	}
}

// TestInstallRuntimeSplitsTheDockerLineWithoutAShell pins the argv of the
// two lines the Linux plan produces. They are the ones with the flags an
// operator's security posture depends on -- the loopback bind and the
// restart policy -- and a splitter that dropped or joined a token would
// publish an unauthenticated model server to the LAN with every test in
// plan_test.go still green.
func TestInstallRuntimeSplitsTheDockerLineWithoutAShell(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{
			name: "nvidia",
			line: installDockerNVIDIA,
			want: []string{
				"docker", "run", "-d", "--name", "ollama", "--restart", "unless-stopped",
				"--gpus=all", "-v", "ollama:/root/.ollama",
				"-p", "127.0.0.1:11434:11434", "ollama/ollama",
			},
		},
		{
			name: "amd",
			line: installDockerAMD,
			want: []string{
				"docker", "run", "-d", "--name", "ollama", "--restart", "unless-stopped",
				"--device", "/dev/kfd", "--device", "/dev/dri", "-v", "ollama:/root/.ollama",
				"-p", "127.0.0.1:11434:11434", "ollama/ollama:rocm",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			p := Plan{Runtime: RuntimeDocker, Install: []string{tc.line}}
			if err := InstallRuntime(context.Background(), p, yes, rec.run, nil); err != nil {
				t.Fatalf("InstallRuntime: %v", err)
			}
			if !reflect.DeepEqual(rec.ran, [][]string{tc.want}) {
				t.Errorf("ran %v, want %v", rec.ran, tc.want)
			}
		})
	}
}

// TestInstallRuntimeWithARefusingConsentRunsNothing is design D2 in one
// test: nothing is installed that nobody approved. The named error is what
// the CLI turns into exit 3.
func TestInstallRuntimeWithARefusingConsentRunsNothing(t *testing.T) {
	rec := &recorder{}
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama, installBrewOllamaStart}}

	err := InstallRuntime(context.Background(), p, no, rec.run, nil)
	if !errors.Is(err, ErrConsentRefused) {
		t.Fatalf("err = %v, want ErrConsentRefused", err)
	}
	if len(rec.ran) != 0 {
		t.Errorf("a refused consent ran %v", rec.ran)
	}
}

// TestInstallRuntimeWithNoConsentFunctionRunsNothing. A nil consent is
// what a caller with no way to ask has -- an automation, a scripted
// install, `--non-interactive`. It must read as NO. A nil that meant yes
// would install software on somebody's machine because a caller forgot to
// wire an argument, which is the same outcome the whole consent step
// exists to prevent and one nobody would see in a diff.
func TestInstallRuntimeWithNoConsentFunctionRunsNothing(t *testing.T) {
	rec := &recorder{}
	p := Plan{Runtime: RuntimeDocker, Install: []string{installDockerNVIDIA}}

	err := InstallRuntime(context.Background(), p, nil, rec.run, nil)
	if !errors.Is(err, ErrConsentRefused) {
		t.Fatalf("err = %v, want ErrConsentRefused", err)
	}
	if len(rec.ran) != 0 {
		t.Errorf("a nil consent ran %v", rec.ran)
	}
}

// TestInstallRuntimeAsksOnceWithEveryCommand. Asking per command would let
// somebody approve `brew install ollama`, decline the start line, and be
// left with a binary and no runtime -- a half-installed machine that
// reports neither success nor a refusal.
func TestInstallRuntimeAsksOnceWithEveryCommand(t *testing.T) {
	rec := &recorder{}
	var asked [][]string
	consent := func(commands []string) bool {
		asked = append(asked, append([]string(nil), commands...))
		return true
	}
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama, installBrewOllamaStart}}

	if err := InstallRuntime(context.Background(), p, consent, rec.run, nil); err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("asked %d times, want exactly one question", len(asked))
	}
	if !reflect.DeepEqual(asked[0], p.Install) {
		t.Errorf("asked about %v, want the whole plan %v", asked[0], p.Install)
	}
}

// TestInstallRuntimeStopsAtTheFirstFailure. `brew services start ollama`
// after a failed `brew install ollama` starts nothing and would leave the
// caller reading the second command's error instead of the first's.
func TestInstallRuntimeStopsAtTheFirstFailure(t *testing.T) {
	boom := errors.New("exit status 1")
	rec := &recorder{fail: map[int]error{0: boom}}
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama, installBrewOllamaStart}}

	err := InstallRuntime(context.Background(), p, yes, rec.run, nil)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the runner's own error", err)
	}
	if !strings.Contains(err.Error(), installBrewOllama) {
		t.Errorf("err = %q, want the command that failed named in it", err)
	}
	if len(rec.ran) != 1 {
		t.Errorf("ran %v, want to have stopped after the first", rec.ran)
	}
}

// TestInstallRuntimeNeverRunsSudo. No plan in this package emits one, and
// the guard is what makes that a RULE rather than a fact that happens to
// hold today: the refusal sentences already tell a person to run a sudo
// command themselves, and the line between printing one and executing one
// is the whole of design D2.
func TestInstallRuntimeNeverRunsSudo(t *testing.T) {
	rec := &recorder{}
	p := Plan{Runtime: RuntimeDocker, Install: []string{"sudo systemctl start docker"}}

	err := InstallRuntime(context.Background(), p, yes, rec.run, nil)
	if !errors.Is(err, ErrSudo) {
		t.Fatalf("err = %v, want ErrSudo", err)
	}
	if len(rec.ran) != 0 {
		t.Errorf("ran %v, want nothing", rec.ran)
	}
}

// TestInstallRuntimeRefusesACommandThatNeedsAShell. `curl | sh` is named
// in CLAUDE.md and in design D2, and the reason the check is on the
// CHARACTERS rather than on the word curl is that the person consented to
// a STRING: anything a shell would have expanded -- a pipe, a $VAR, a
// backtick, a redirect -- means the argv that ran is not the line they
// read.
func TestInstallRuntimeRefusesACommandThatNeedsAShell(t *testing.T) {
	for _, line := range []string{
		"curl -fsSL https://ollama.com/install.sh | sh",
		"docker run ollama/ollama > /dev/null",
		"brew install $PACKAGE",
		"docker run -e HOME=`pwd` ollama/ollama",
		"brew install ollama; rm -rf /",
		"sh -c 'brew install ollama'",
	} {
		rec := &recorder{}
		p := Plan{Runtime: RuntimeNative, Install: []string{line}}
		err := InstallRuntime(context.Background(), p, yes, rec.run, nil)
		if !errors.Is(err, ErrUnsafeCommand) {
			t.Errorf("%q: err = %v, want ErrUnsafeCommand", line, err)
		}
		if len(rec.ran) != 0 {
			t.Errorf("%q ran %v, want nothing", line, rec.ran)
		}
	}
}

// TestInstallRuntimeChecksEveryCommandBeforeItAsks. A person who says yes
// and then gets a refusal has been asked a question that was never going
// to be honoured, and on a two-command plan the first one has already run
// by then.
func TestInstallRuntimeChecksEveryCommandBeforeItAsks(t *testing.T) {
	rec := &recorder{}
	asked := false
	consent := func([]string) bool { asked = true; return true }
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama, "brew services start ollama && echo done"}}

	if err := InstallRuntime(context.Background(), p, consent, rec.run, nil); !errors.Is(err, ErrUnsafeCommand) {
		t.Fatalf("err = %v, want ErrUnsafeCommand", err)
	}
	if asked {
		t.Error("asked about a plan it was never going to run")
	}
	if len(rec.ran) != 0 {
		t.Errorf("ran %v, want nothing", rec.ran)
	}
}

// TestInstallRuntimeOnAPlanThatRefuses carries the plan's own sentence
// out. The sentences are the product (plan.go): a caller that got a
// generic "install failed" would print something less useful than the
// refusal it already had in hand.
func TestInstallRuntimeOnAPlanThatRefuses(t *testing.T) {
	rec := &recorder{}
	asked := false
	p := Plan{Refusal: refusalNoDocker}

	err := InstallRuntime(context.Background(), p, func([]string) bool { asked = true; return true }, rec.run, nil)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), refusalNoDocker) {
		t.Errorf("err = %q, want the plan's refusal verbatim", err)
	}
	if asked || len(rec.ran) != 0 {
		t.Errorf("asked=%v ran=%v, want neither", asked, rec.ran)
	}
}

// TestInstallRuntimeIsANoOpWhenTheRuntimeIsPresent. Design D2 asks for
// idempotence, and this is where it lives: running `setup --inference`
// twice must not ask a second time and must not print a `docker run` that
// would only produce a container name conflict.
func TestInstallRuntimeIsANoOpWhenTheRuntimeIsPresent(t *testing.T) {
	rec := &recorder{}
	asked := false
	p := Plan{Runtime: RuntimeNative, RuntimePresent: true}

	if err := InstallRuntime(context.Background(), p, func([]string) bool { asked = true; return true }, rec.run, nil); err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	if asked || len(rec.ran) != 0 {
		t.Errorf("asked=%v ran=%v, want neither", asked, rec.ran)
	}
}

// TestInstallRuntimeOnASilentPlan. Decide never produces one -- a property
// test in plan_test.go holds that -- so this is the total-function
// direction: a plan with no refusal, no runtime present and no commands
// must not report a successful install of nothing.
func TestInstallRuntimeOnASilentPlan(t *testing.T) {
	rec := &recorder{}
	if err := InstallRuntime(context.Background(), Plan{Runtime: RuntimeNative}, yes, rec.run, nil); !errors.Is(err, ErrNothingToRun) {
		t.Fatalf("err = %v, want ErrNothingToRun", err)
	}
}

// TestInstallRuntimeOnACancelledContext. `setup --inference` runs in front
// of a person, and a Ctrl-C between the plan and the consent prompt must
// not be answered with a Homebrew install.
func TestInstallRuntimeOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &recorder{}
	p := Plan{Runtime: RuntimeNative, Install: []string{installBrewOllama}}

	if err := InstallRuntime(ctx, p, yes, rec.run, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(rec.ran) != 0 {
		t.Errorf("ran %v after a cancel", rec.ran)
	}
}

// TestEveryPlanThisPackageProducesIsRunnable walks Decide's own install
// lines through the same splitter InstallRuntime uses. It is the test that
// catches a future vendor line pasted in with a pipe or a shell variable
// in it: plan_test.go asserts the STRING, and only this asserts that the
// string is one this process can execute without a shell.
func TestEveryPlanThisPackageProducesIsRunnable(t *testing.T) {
	for _, line := range []string{installBrewOllama, installBrewOllamaStart, installDockerNVIDIA, installDockerAMD, installSystemdReload, installSystemdEnable} {
		argv, err := splitCommand(line)
		if err != nil {
			t.Errorf("%q: %v", line, err)
			continue
		}
		if strings.Join(argv, " ") != line {
			t.Errorf("%q round-tripped as %q; the line printed and the argv run must be the same thing", line, strings.Join(argv, " "))
		}
	}
}

// TestExecRunnerStreamsOutputAndReportsTheExitCode drives the default
// runner against this test binary re-invoked as a child, so it needs no
// brew, no docker and no shell script on disk.
func TestExecRunnerStreamsOutputAndReportsTheExitCode(t *testing.T) {
	t.Setenv("MEMQL_INFERENCE_HELPER", "1")

	var out bytes.Buffer
	run := ExecRunner(&out)

	if err := run(context.Background(), []string{os.Args[0], "-test.run=TestInstallHelperProcess", "--", "0"}); err != nil {
		t.Fatalf("a child that exits 0: %v", err)
	}
	if !strings.Contains(out.String(), "downloading ollama") {
		t.Errorf("output %q, want the child's own line streamed through", out.String())
	}

	out.Reset()
	err := run(context.Background(), []string{os.Args[0], "-test.run=TestInstallHelperProcess", "--", "7"})
	if err == nil {
		t.Fatal("a child that exits 7 must be an error")
	}
	if !strings.Contains(err.Error(), "7") {
		t.Errorf("err = %q, want the exit code in it", err)
	}
}

// TestExecRunnerNamesABinaryThatIsNotOnPath. "exec: \"brew\": executable
// file not found in $PATH" is a sentence about Go's exec package; the
// person is being told that Homebrew is not installed.
func TestExecRunnerNamesABinaryThatIsNotOnPath(t *testing.T) {
	err := ExecRunner(nil)(context.Background(), []string{"memql-no-such-binary-exists", "--version"})
	if err == nil {
		t.Fatal("want an error for a binary that is not on PATH")
	}
	if !strings.Contains(err.Error(), "memql-no-such-binary-exists") {
		t.Errorf("err = %q, want the binary named", err)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("err = %q, want it to say where it looked", err)
	}
}

// TestInstallHelperProcess is not a test. It is the child ExecRunner runs
// above, in the stdlib's own idiom: without the environment variable it
// returns immediately, so a normal `go test` run passes straight over it.
func TestInstallHelperProcess(t *testing.T) {
	if os.Getenv("MEMQL_INFERENCE_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "downloading ollama")
	code := 0
	args := os.Args
	if len(args) > 0 && args[len(args)-1] == "7" {
		code = 7
	}
	os.Exit(code)
}

// TestInstallRuntimeStagesBeforeTheFirstCommand. The unit file and the
// archive are what `systemctl --user enable --now` starts; enabling a
// unit whose file is not there yet is the half-built machine the ordering
// exists to prevent.
func TestInstallRuntimeStagesBeforeTheFirstCommand(t *testing.T) {
	var events []string
	run := func(_ context.Context, argv []string) error {
		events = append(events, "run "+strings.Join(argv, " "))
		return nil
	}
	stage := func(_ context.Context, s Stage, _ func(Progress)) error {
		events = append(events, "stage "+s.RuntimeDir)
		return nil
	}
	p := Plan{
		Runtime: RuntimeNative,
		Install: []string{installSystemdReload, installSystemdEnable},
		Stage:   &Stage{RuntimeDir: "/home/op/.memql/ollama/runtime"},
	}
	if err := InstallRuntime(context.Background(), p, yes, run, stage); err != nil {
		t.Fatalf("InstallRuntime: %v", err)
	}
	want := []string{
		"stage /home/op/.memql/ollama/runtime",
		"run systemctl --user daemon-reload",
		"run systemctl --user enable --now memql-ollama.service",
	}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events %v, want %v", events, want)
	}
}

// TestInstallRuntimeAFailedStageRunsNoCommand. A checksum that did not
// match, a download that stopped: no unit is enabled over it.
func TestInstallRuntimeAFailedStageRunsNoCommand(t *testing.T) {
	boom := errors.New("the downloaded archive does not match the release's checksum")
	rec := &recorder{}
	stage := func(context.Context, Stage, func(Progress)) error { return boom }
	p := Plan{Runtime: RuntimeNative, Install: []string{installSystemdReload, installSystemdEnable}, Stage: &Stage{}}

	err := InstallRuntime(context.Background(), p, yes, rec.run, stage)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the stage's own error", err)
	}
	if len(rec.ran) != 0 {
		t.Errorf("ran %v after a failed stage, want nothing", rec.ran)
	}
}

// TestInstallRuntimeStagesNothingWithoutConsent. The stage is a 1.4 GB
// download and a file under ~/.config; both are behind the same yes as
// the commands.
func TestInstallRuntimeStagesNothingWithoutConsent(t *testing.T) {
	staged := false
	stage := func(context.Context, Stage, func(Progress)) error { staged = true; return nil }
	p := Plan{Runtime: RuntimeNative, Install: []string{installSystemdReload}, Stage: &Stage{}}
	for _, consent := range []func([]string) bool{no, nil} {
		if err := InstallRuntime(context.Background(), p, consent, (&recorder{}).run, stage); !errors.Is(err, ErrConsentRefused) {
			t.Fatalf("err = %v, want ErrConsentRefused", err)
		}
	}
	if staged {
		t.Error("staged without consent")
	}
}
