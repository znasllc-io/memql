package goalfixture

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

// Workspace keeps the immutable fixture outside the model's checkout. The
// evaluator never applies a fix there. Each invocation creates a fresh root;
// it cannot reuse an arbitrary existing checkout or delete somebody's work.
type Workspace struct {
	Root       string            `json:"root"`
	Checkout   string            `json:"checkout"`
	BaseCommit string            `json:"baseCommit"`
	Before     map[string][]byte `json:"-"`
	Preserved  map[string][]byte `json:"-"`
}

func Prepare(ctx context.Context, parent string, s goaleval.Scenario, files map[string][]byte) (*Workspace, error) {
	root, err := os.MkdirTemp(parent, "memql-evaluation-"+s.ID+"-")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Root: root, Checkout: filepath.Join(root, "checkout"), Before: files, Preserved: map[string][]byte{}}
	if err := os.Mkdir(w.Checkout, 0700); err != nil {
		return nil, err
	}
	for name, b := range files {
		if !fs.ValidPath(name) || strings.HasPrefix(name, ".git/") || name == ".git" {
			return nil, fmt.Errorf("unsafe fixture path")
		}
		if err := putFile(w.Checkout, name, b); err != nil {
			return nil, err
		}
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "--", "."}, {"-c", "user.name=MemQL evaluation", "-c", "user.email=evaluation@invalid.example", "commit", "-m", "Versioned evaluation fixture"}} {
		if _, err := Command(ctx, w.Checkout, "git", args...); err != nil {
			return nil, err
		}
	}
	sha, err := Command(ctx, w.Checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	w.BaseCommit = strings.TrimSpace(sha)
	w.Preserved["NOTES.txt"] = append(append([]byte{}, files["NOTES.txt"]...), []byte("Uncommitted operator work: preserve this addition.\n")...)
	w.Preserved["operator-untracked.txt"] = []byte("Untracked operator work: preserve this file.\n")
	w.Preserved[".memql-evaluation.json"] = files[".memql-evaluation.json"]
	for name, b := range w.Preserved {
		if err := putFile(w.Checkout, name, b); err != nil {
			return nil, err
		}
	}
	return w, nil
}

func putFile(root, name string, b []byte) error {
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0600)
}

// A fixture command's watchdogs. Each detects a stuck command; neither is a
// spending limit or the time the work under test gets. They are vars so a test
// can watch one fire.
var (
	// commandBound covers git and the like, which need milliseconds of CPU.
	commandBound = 60 * time.Second

	// goTestBound covers one oracle `go test`, compile and link included, so what
	// it needs moves with the machine's load. The ordinary minute read a busy
	// machine as broken work: train memql#6406's component/proving took 142 s
	// against its usual 7 s and failed. Measured 2026-11-01 in golang:1.27-bookworm
	// with CI's GOGC=50 and GOMEMLIMIT=512MiB on an arm64 host, one run costs 15
	// CPU-seconds on a cold GOCACHE (76 s at --cpus=0.2) and 0.2 on a warm one (1 s).
	//
	// Widening it costs a hung test nothing: -timeout=20s ends one, and `go test`
	// kills a binary that ignores that at 1m20s and reports FAIL (measured: 85 s).
	// Only a wedged toolchain reaches this bound. Five minutes stays under the
	// go-tests step's -timeout=600s per package, so a stall ends in the oracle's
	// own answer instead of the package's panic, and well inside a case's
	// wallClockMinutes.
	//
	// Compiling ahead with `go test -c` was the alternative. It separates compile
	// from test, which -timeout already does, and it protects only a cold compile:
	// CI's own `go test` has warmed the shared GOCACHE before this one runs, so
	// whatever stalled the train was not compile volume. That train's oracle output
	// was lost, so what did stall it is unverified; a bound on the whole command
	// holds whichever phase stalls. A kill is still possible, and
	// ErrOracleDidNotFinish says what one means.
	goTestBound = 5 * time.Minute
)

// errUnfinished marks a command that reported no exit status of its own: it
// could not start, or its watchdog, the case's deadline or a signal ended it.
// What it was asked to check is still unknown.
var errUnfinished = errors.New("did not finish")

// Command is bounded and returns the real exit status. A failed command's
// output is evidence, not a successful test result. Toolchain/network settings
// prevent Go from acquiring dependencies or another compiler for a fixture.
func Command(ctx context.Context, dir, name string, args ...string) (string, error) {
	return commandWithin(ctx, commandBound, dir, name, args...)
}

func commandWithin(ctx context.Context, bound time.Duration, dir, name string, args ...string) (string, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-p=1", "GOMAXPROCS=2", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.WaitDelay = time.Second
	var output cappedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() < 0 {
			return output.String(), fmt.Errorf("%s %w after %s: %w", name, errUnfinished, time.Since(start).Round(time.Second), err)
		}
		return output.String(), fmt.Errorf("%s failed: %w", name, err)
	}
	return output.String(), nil
}

type cappedOutput struct{ strings.Builder }

func (b *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 1024*1024 - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Builder.Write(p)
	}
	return n, nil
}

// Snapshot refuses links, special files, excessive files and changed module
// dependencies. Independent oracles run only the captured package outside the
// worker checkout, so the model cannot edit the oracle or its result file.
func (w *Workspace) Snapshot() (map[string][]byte, error) {
	files := map[string][]byte{}
	var total int64
	err := filepath.WalkDir(w.Checkout, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.Checkout, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains a symlink: %s", rel)
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if !info.Mode().IsRegular() || len(files) >= 100 || total > 2*1024*1024 {
			return fmt.Errorf("fixture exceeds regular-file limits")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err == nil && string(files["go.mod"]) != string(w.Before["go.mod"]) {
		err = fmt.Errorf("fixture module contract was changed")
	}
	return files, err
}
