//go:build darwin || linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/znasllc-io/memql/cockpit/internal/worker/workbench"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
)

// One build reservation per operating-system user, shared by every worker
// process and every cluster enrollment. It deliberately does not derive from
// MEMQL_HOME, worker configuration or workspace_root: those vary per cluster.
// A closed laptop retains the kernel lock. Process death releases the lock,
// but an unfinished record still refuses reuse until its effects reconcile.
var pipelineCapacityRoot = func() (string, error) {
	owner, err := user.Current()
	if err != nil || owner.HomeDir == "" {
		return "", errors.New("cannot resolve the worker account's home for capacity admission")
	}
	return filepath.Join(owner.HomeDir, ".memql", "worker-capacity"), nil
}

type pipelineReservation struct {
	file  *os.File
	dirty bool
}
type pipelineAttemptRecord struct {
	Workspace string   `json:"workspace"`
	Execution string   `json:"execution"`
	Container string   `json:"container,omitempty"`
	Services  []string `json:"services,omitempty"`
	Network   string   `json:"network,omitempty"`
	// Identity of the Docker endpoint is needed before another worker may
	// reconcile it; it must not remove a namesake on a different daemon.
	DockerID string `json:"dockerID,omitempty"`
}

var errPipelineBusy = errors.New("this machine's build capacity is occupied")
var errPipelineUnreconciled = errors.New("an interrupted build needs reconciliation before this machine can accept another build")

func hasPipelineAttempt() (bool, error) {
	root, err := pipelineCapacityRoot()
	if err != nil {
		return false, err
	}
	stat, err := os.Lstat(filepath.Join(root, "pipeline.lock"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !stat.Mode().IsRegular() {
		return false, errPipelineUnreconciled
	}
	return stat.Size() != 0, nil
}

func acquirePipelineReservation() (*pipelineReservation, error) {
	root, err := pipelineCapacityRoot()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Never unlink this file. Removing a locked inode would let a newcomer
	// acquire a different inode and run concurrently with the existing owner.
	fd, err := syscall.Open(filepath.Join(root, "pipeline.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "pipeline capacity")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errPipelineBusy
		}
		return nil, err
	}
	if interrupted, err := workbench.HasInterrupted(root); err != nil || interrupted {
		file.Close()
		return nil, errPipelineUnreconciled
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("capacity record is not a regular file")
	}
	return &pipelineReservation{file: file, dirty: stat.Size() != 0}, nil
}
func (r *pipelineReservation) read() (pipelineAttemptRecord, error) {
	var record pipelineAttemptRecord
	_, err := r.file.Seek(0, io.SeekStart)
	if err != nil {
		return record, err
	}
	err = json.NewDecoder(io.LimitReader(r.file, 4096)).Decode(&record)
	if err != nil {
		return record, fmt.Errorf("read interrupted build record: %w", err)
	}
	return record, nil
}
func (r *pipelineReservation) begin(record pipelineAttemptRecord) error {
	if r.dirty {
		previous, err := r.read()
		if err != nil || previous.Execution != "cleanup" || previous.Workspace != record.Workspace || previous.Container != "" || previous.DockerID != "" || previous.Network != "" || len(previous.Services) != 0 {
			return errPipelineUnreconciled
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	r.dirty = true
	if _, err = r.file.WriteAt(data, 0); err != nil {
		return err
	}
	if err = r.file.Truncate(int64(len(data))); err != nil {
		return err
	}
	return r.file.Sync()
}

// cleanupReady is written only after runtime teardown is confirmed. A
// replacement worker may then remove scratch data without Docker or replaying
// the command. Keep the same locked inode throughout this transition.
func (r *pipelineReservation) cleanupReady(workspace string) error {
	if !filepath.IsAbs(workspace) || !strings.HasPrefix(filepath.Base(workspace), "step-") {
		return errPipelineUnreconciled
	}
	if r.dirty {
		record, err := r.read()
		if err != nil {
			return err
		}
		if record.Workspace != workspace {
			return errPipelineUnreconciled
		}
	}
	data, err := json.Marshal(pipelineAttemptRecord{Execution: "cleanup", Workspace: workspace})
	if err != nil {
		return err
	}
	r.dirty = true
	if _, err = r.file.WriteAt(data, 0); err != nil {
		return err
	}
	if err = r.file.Truncate(int64(len(data))); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *pipelineReservation) clear() error {
	if err := r.file.Truncate(0); err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	r.dirty = false
	return nil
}
func (r *pipelineReservation) close() { r.file.Close() }
