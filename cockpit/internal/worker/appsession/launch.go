package appsession

import (
	"context"
	"errors"
	"io"

	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
)

// launch.go is the ONE adapter between the harness clients and this
// package's process supervisor.
//
// WHY AN ADAPTER RATHER THAN A SECOND SUPERVISOR. internal/worker/harness
// forks nothing: every process it needs comes from a LaunchFunc the
// caller injects. That is deliberate on both sides. The cockpit already
// has exactly one supervisor -- process.go: own process group, the
// partial-line flush, SIGTERM then a grace window then SIGKILL, and the
// real unnormalised exit code -- and a second one would diverge from it
// in the way that costs somebody a laptop. A cancel that reaps only the
// direct child leaves `claude`'s tools, and their compilers and test
// runners, running on a machine with nothing watching them; that bug is
// worth having in one place rather than two, and this file is what keeps
// it to one.
//
// So a harness turn is supervised by exactly what a plain run was
// supervised by before turns existed. Cancel, the wall-clock ceiling and
// a lost stream all still terminate the GROUP, and the code the engine
// files the run under is still the app's own.

// launcher returns the session's harness.LaunchFunc.
//
// Every process it starts is registered on the session, so cancel and
// teardown reach it. That registration is a single slot rather than a
// list on purpose: a session's turns are SEQUENTIAL (see runTurns), and
// the two harness shapes are process-per-turn (Claude Code, where the
// previous turn's process has already been reaped before the next one
// starts) and one-process-per-session (both Codex clients, which launch
// once in Start). In neither shape are two children of one session alive
// at the same time, so "the current child" is a complete answer and a
// list would only be a longer way to write it.
func (s *session) launcher() harness.LaunchFunc {
	return func(ctx context.Context, dir string, argv []string, env []string, stdin bool) (harness.Process, error) {
		// A cancel that lands BETWEEN two turns must not start the next
		// one. Without this check a follow-up already popped off the
		// queue would fork an agent moments after the server said stop,
		// and the only thing that would end it is the teardown at the
		// far end of the run -- by which time it has already touched
		// somebody's files.
		if err := ctx.Err(); err != nil {
			return nil, errors.New("app session: the session ended before this turn could start: " + err.Error())
		}

		c, err := startChildStdin(dir, argv, env, stdin)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.child = c
		s.mu.Unlock()

		// The cancel path reads s.child and terminates it; a cancel that
		// landed while this process was starting read a nil (or the
		// previous turn's) child and moved on. Re-checking here closes
		// that window deterministically instead of leaving the process
		// alive until the far-away teardown notices it.
		if ctx.Err() != nil {
			c.terminate()
		}
		return &supervisedProcess{c: c}, nil
	}
}

// supervisedProcess presents one supervised child as a harness.Process.
//
// It adds no behaviour of its own -- no reaping, no signalling, no grace
// window. Everything it answers comes from the child, which is the whole
// point of the seam.
type supervisedProcess struct {
	c *child
}

// Stdin is nil unless the harness asked for one. Returning the child's
// field rather than a lazily-created pipe is deliberate: a pipe created
// after Start would not be the process's stdin at all, and a harness
// writing into it would block forever on a reader that does not exist.
func (p *supervisedProcess) Stdin() io.WriteCloser { return p.c.stdin }

func (p *supervisedProcess) Stdout() io.Reader { return p.c.stdout }
func (p *supervisedProcess) Stderr() io.Reader { return p.c.stderr }

// Terminate stops the whole PROCESS GROUP, which is the reason this
// adapter exists at all.
func (p *supervisedProcess) Terminate() { p.c.terminate() }

// Wait reaps the process. Safe from several goroutines and repeatable:
// the child reaps once behind a sync.Once and every caller gets the same
// answer, which is what lets a harness call Wait on its own path while
// the cancel path is inspecting the same process.
func (p *supervisedProcess) Wait() error { return p.c.wait() }

// ExitCode is the app's REAL exit status, unnormalised. The engine reads
// a non-zero exit as a FAILED run rather than an ended one, so flattening
// a 2 to a 1 -- or to a 0 -- misfiles the outcome in a record people read
// back later to decide whether the thing worked.
func (p *supervisedProcess) ExitCode() int { return exitCode(p.c.wait()) }
