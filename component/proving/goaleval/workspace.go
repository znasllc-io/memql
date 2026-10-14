package goaleval

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

func Prepare(ctx context.Context, parent string, s Scenario, files map[string][]byte) (*Workspace, error) {
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

// Command is bounded and returns the real exit status. A failed command's
// output is evidence, not a successful test result. Toolchain/network settings
// prevent Go from acquiring dependencies or another compiler for a fixture.
func Command(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-p=1", "GOMAXPROCS=2", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.WaitDelay = time.Second
	var output cappedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err != nil {
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
