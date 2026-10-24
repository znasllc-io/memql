package appsession

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A checkout is an input, even when it was obtained during this session.
// Only its changed files are candidates for output. Explicitly recorded writes
// remain evidence even after the app commits them. Git metadata and caches never
// become artifacts; the journal carries the commands and source provenance.
func repositoryInputs(root string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	read := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root, "-c", "core.fsmonitor=false"}, args...)...)
		cmd.WaitDelay = time.Second
		var out artifactGitOutput
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	tracked, err := read("ls-files", "--cached", "-z")
	if err != nil {
		return nil, fmt.Errorf("read checkout index: %w", err)
	}
	if len(tracked) == 0 {
		return map[string]bool{}, nil
	}
	if _, err := read("rev-parse", "--verify", "HEAD"); err != nil {
		// An initialized repository with no commit contains new work, not a
		// checkout. Refuse corrupt repositories rather than treating them as new.
		if _, refErr := read("symbolic-ref", "HEAD"); refErr != nil {
			return nil, err
		}
		return map[string]bool{}, nil
	}
	// --no-ext-diff and --no-textconv prevent repository configuration from
	// turning this read into execution of an external diff/filter command.
	changed, err := read("diff", "--no-ext-diff", "--no-textconv", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return nil, fmt.Errorf("read checkout changes: %w", err)
	}
	inputs := map[string]bool{}
	for _, name := range strings.Split(string(tracked), "\x00") {
		if name != "" {
			inputs[filepath.Join(root, filepath.FromSlash(name))] = true
		}
	}
	for _, name := range strings.Split(string(changed), "\x00") {
		delete(inputs, filepath.Join(root, filepath.FromSlash(name)))
	}
	return inputs, nil
}

// Bound both memory and time when reading an app-controlled repository.
type artifactGitOutput struct{ bytes.Buffer }

func (b *artifactGitOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, fmt.Errorf("checkout listing exceeds 8 MiB")
	}
	return b.Buffer.Write(p)
}
