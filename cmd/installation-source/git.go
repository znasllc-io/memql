package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"
)

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkoutObjects reads raw objects, never working-tree files. Git replacement,
// global config, filesystem monitors, hooks, transport and lazy fetch are
// disabled. The trusted Job's clone supplies the checkout; no repository code
// is executed by this collector, and its environment carries no clone token.
type checkoutObjects struct{ repository, git string }

func (g checkoutObjects) OpenGitObject(ctx context.Context, kind, oid string) (io.ReadCloser, error) {
	if (kind != "commit" && kind != "tree" && kind != "blob") || !objectID.MatchString(oid) {
		return nil, errors.New("invalid Git object request")
	}
	cmd := exec.CommandContext(ctx, g.git, "--no-replace-objects", "-C", g.repository,
		"-c", "safe.directory="+g.repository, "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null",
		"-c", "protocol.allow=never", "cat-file", kind, oid)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0",
	}
	cmd.WaitDelay = 2 * time.Second
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("Git object reader could not be opened")
	}
	if err := cmd.Start(); err != nil {
		_ = pipe.Close()
		return nil, errors.New("Git object reader could not be started")
	}
	return &objectProcess{cmd: cmd, pipe: pipe}, nil
}

type objectProcess struct {
	cmd  *exec.Cmd
	pipe io.ReadCloser
	eof  bool
}

func (p *objectProcess) Read(b []byte) (int, error) {
	n, err := p.pipe.Read(b)
	if errors.Is(err, io.EOF) {
		p.eof = true
	}
	return n, err
}

func (p *objectProcess) Close() error {
	// A bounded consumer can stop before EOF. Kill before Wait so a producer
	// blocked on a full pipe cannot outlive cancellation or an object limit.
	if !p.eof {
		_ = p.cmd.Process.Kill()
	}
	_ = p.pipe.Close()
	if err := p.cmd.Wait(); err != nil {
		return errors.New("Git object read failed")
	}
	return nil
}
