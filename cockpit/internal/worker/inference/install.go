package inference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// install.go runs the commands Decide already chose, and nothing else.
//
// THE LINE PRINTED AND THE ARGV RUN ARE THE SAME THING. That is the whole
// contract of this file. A person is shown `p.Install` and answers a
// question about it; if what then executes is a shell that expands `$HOME`
// or follows a pipe, they approved a string and the machine ran a program.
// So every command is split on whitespace into a plain argv, there is no
// `sh -c` anywhere in this package, and a command carrying any character a
// shell would have treated specially is REFUSED rather than guessed at
// (design D2, and CLAUDE.md's "never sudo, never curl | sh").
//
// Refusing a sudo command is the same rule seen from the other side. The
// refusals in plan.go already tell a person to run `sudo systemctl start
// docker` themselves, and that is fine: the rule is about what THIS
// PROCESS executes, not about what a sentence may name. Nothing in Decide
// emits a sudo line today; the guard is what keeps that a decision rather
// than an accident somebody notices on a production machine.

// Runner runs one install command, already split into argv.
//
// It is a seam so a test can assert exactly which argv ran, in order, with
// no brew and no docker on the runner -- and so a caller that wants to
// print rather than execute (a dry run, the portal's "here is what it
// would do") needs no second implementation of the consent flow.
type Runner func(ctx context.Context, argv []string) error

// The named errors. Each one is a DIFFERENT thing for the caller to do:
// exit 3 and print the interactive command (consent), print the plan's own
// sentence (refused), or report a bug in the plan (the rest). A single
// "install failed" would collapse the first two, and `--non-interactive`
// in the installers keys its whole behaviour off telling them apart.
var (
	// ErrConsentRefused is the `--non-interactive` exit-3 case: the
	// commands were shown and not approved, or there was nobody to ask.
	ErrConsentRefused = errors.New("nothing was installed, because the commands were not approved")

	// ErrRefused wraps the plan's own refusal sentence. Read it with
	// errors.Is; print err.Error(), which carries the sentence verbatim.
	ErrRefused = errors.New("this machine cannot be set up to serve models")

	// ErrNothingToRun is a plan with no refusal, no runtime present and no
	// commands. Decide cannot produce one (plan.go holds that with a
	// property test), so it is a wiring mistake rather than a machine
	// state -- and reporting a successful install of nothing is exactly
	// how a machine ends up advertising a runtime it does not have.
	ErrNothingToRun = errors.New("the plan named no commands to run, so nothing was installed")

	// ErrSudo is a command this process will not execute at any privilege.
	ErrSudo = errors.New("an install command starts with sudo, and this command never runs one")

	// ErrUnsafeCommand is a command that would need a shell.
	ErrUnsafeCommand = errors.New("an install command carries a character that needs a shell, and this command has no shell to run it in")
)

// InstallRuntime installs the runtime the plan chose, after consent.
//
// consent is asked ONCE, with the whole list. Asking per command lets
// somebody approve `brew install ollama`, decline `brew services start
// ollama`, and be left with a binary and no runtime -- a machine that is
// neither installed nor refused, and whose next step pulls against a
// socket nothing is listening on. A nil consent counts as NO, because the
// caller with no way to ask is an automation, and a nil that meant yes
// would install software on somebody's machine over a forgotten argument.
//
// run may be nil, which takes ExecRunner(os.Stdout): the person is
// watching, and `brew install ollama` is worth watching.
//
// stage runs the plan's Stage, when it has one, between the consent and
// the first command; nil takes StageRuntime, which fetches for real.
func InstallRuntime(ctx context.Context, p Plan, consent func(commands []string) bool, run Runner, stage Stager) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.Refusal != "" {
		return fmt.Errorf("%w: %s", ErrRefused, p.Refusal)
	}
	// Already serving. Idempotent by design (D2): running `setup
	// --inference` twice must not ask a second question, and must not
	// re-run a `docker run` whose only outcome would be a container name
	// conflict.
	if p.RuntimePresent {
		return nil
	}
	if len(p.Install) == 0 {
		return ErrNothingToRun
	}

	// EVERY command is split before ANY is run, and before the question is
	// asked. A plan whose second line cannot be executed is a plan that
	// was never going to finish, and finding that out after the first line
	// has installed Homebrew's Ollama leaves the machine half-built with a
	// consent already spent.
	argvs, err := splitAll(p.Install)
	if err != nil {
		return err
	}

	if consent == nil || !consent(p.Install) {
		return ErrConsentRefused
	}
	// The stage runs FIRST, after the one consent: the archive and the
	// unit file are what `systemctl --user enable --now` starts, and
	// enabling a unit whose file is not there yet is the half-built
	// machine this ordering exists to prevent.
	if p.Stage != nil {
		if stage == nil {
			stage = StageRuntime
		}
		if err := stage(ctx, *p.Stage, nil); err != nil {
			return fmt.Errorf("staging the runtime: %w", err)
		}
	}
	return runAll(ctx, p.Install, argvs, run)
}

// RunCommands runs an already-consented list.
//
// It exists for `setup --runtime`, which asks its own question in its
// own words and would otherwise have to build a Plan to reach
// InstallRuntime's consent callback -- a shape that reads as though a
// runtime install were a model-runtime install, which it is not. The
// SPLIT-EVERYTHING-FIRST rule and the name-the-failing-command rule are
// shared rather than reimplemented, because those are the two that stop
// a half-built machine.
func RunCommands(ctx context.Context, commands []string, run Runner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(commands) == 0 {
		return ErrNothingToRun
	}
	argvs, err := splitAll(commands)
	if err != nil {
		return err
	}
	return runAll(ctx, commands, argvs, run)
}

func splitAll(commands []string) ([][]string, error) {
	argvs := make([][]string, 0, len(commands))
	for _, cmd := range commands {
		argv, err := splitCommand(cmd)
		if err != nil {
			return nil, err
		}
		argvs = append(argvs, argv)
	}
	return argvs, nil
}

func runAll(ctx context.Context, commands []string, argvs [][]string, run Runner) error {
	if run == nil {
		run = ExecRunner(os.Stdout)
	}
	for i, argv := range argvs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := run(ctx, argv); err != nil {
			// The COMMAND is named, not just the error. "exit status 1"
			// on its own sends a person to their shell history to work
			// out which of two lines produced it.
			return fmt.Errorf("%s: %w", commands[i], err)
		}
	}
	return nil
}

// argvRune is the character set an install command may contain.
//
// An ALLOW list rather than a deny list, for the reason every other list
// in this repository is: a deny list of shell metacharacters is a list
// somebody has to keep complete, and the character it misses is the one
// that matters. Everything Decide emits -- flags, image tags, volume
// mounts, host:port pairs, /dev paths -- is covered, and anything else is
// a line a human should read before this process runs it.
func argvRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	return strings.ContainsRune("-_./:=,+@", r)
}

// splitCommand turns one printed line into the argv that will run.
//
// Whitespace-separated, with no quoting rules at all: a command that
// NEEDED quoting is one this splitter would get wrong, and it is refused
// instead. The round trip is asserted in the tests -- strings.Join(argv,
// " ") must equal the line the person was shown.
func splitCommand(cmd string) ([]string, error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil, ErrNothingToRun
	}
	if fields[0] == "sudo" || fields[0] == "doas" || fields[0] == "pkexec" {
		return nil, fmt.Errorf("%w: %q", ErrSudo, cmd)
	}
	for _, f := range fields {
		for _, r := range f {
			if !argvRune(r) {
				return nil, fmt.Errorf("%w: %q", ErrUnsafeCommand, cmd)
			}
		}
	}
	return fields, nil
}

// ExecRunner is the Runner that actually installs, streaming the child's
// output to out as it arrives. A nil out discards it.
//
// IT IS DELIBERATELY NOT THE APP-SESSION SUPERVISOR, and the two things
// that supervisor is built for are both wrong here:
//
//   - Its child runs in its OWN PROCESS GROUP, because an app session is
//     autonomous and the only way to stop it is a signal the cockpit
//     sends. These commands run in front of a person at a terminal, whose
//     Ctrl-C reaches the foreground process group -- so inheriting the
//     group is what makes Ctrl-C stop `brew install`, and taking a new one
//     would leave them pressing it at a process that no longer hears them.
//   - Its partial-line flush exists because an app's progress bar has to
//     be chunked onto a gRPC stream without going silent for ten minutes.
//     Here stdout is wired straight through to the terminal, so there is
//     no buffer between the child and the person and nothing to flush.
//
// What is left is short and bounded: `brew install ollama` and a
// `docker run -d` that returns as soon as the container id is printed. A
// pull is the long one, and it does not run here at all -- see pull.go,
// which does not spawn a process either.
func ExecRunner(out io.Writer) Runner {
	return func(ctx context.Context, argv []string) error {
		if len(argv) == 0 {
			return ErrNothingToRun
		}
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			return fmt.Errorf("%s is not on this machine's PATH", argv[0])
		}
		cmd := exec.CommandContext(ctx, bin, argv[1:]...)
		cmd.Stdout = out
		cmd.Stderr = out
		// Nothing is going to type at it. A closed stdin turns a command
		// that would have prompted into one that fails immediately and
		// says so, rather than a `setup --inference` that appears to hang
		// under a LaunchAgent with the question on a terminal nobody has.
		cmd.Stdin = nil
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", argv[0], err)
		}
		return nil
	}
}
