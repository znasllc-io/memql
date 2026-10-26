//go:build darwin || linux

package modelcall

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/workbench"
)

// Independent of cluster configuration and MEMQL_HOME: separate workers still
// share the same GPU and host memory. The kernel releases the locks if a worker
// exits; the separate activity witness still fences uncertain runtime effects.
func inferenceCapacityRoot() (string, error) {
	owner, err := user.Current()
	if err != nil || owner.HomeDir == "" {
		return "", errors.New("cannot resolve worker account for inference admission")
	}
	return filepath.Join(owner.HomeDir, ".memql", "worker-capacity"), nil
}

type inferenceLease struct {
	file, pipeline *os.File
	root           string
	active         []byte
}
type ownedModel struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
}

func acquireInferenceLease(ctx context.Context, root string, poll time.Duration) (*inferenceLease, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Builds need exclusive ownership, while an agent's persistent workbench
	// must survive its next model turn. Sharing this lock permits that cycle;
	// resource admission reserves every workbench slot's full RAM bound.
	pipeline, err := lockInferenceFile(ctx, root, "pipeline.lock", syscall.LOCK_SH, poll)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			pipeline.Close()
		}
	}()
	file, err := lockInferenceFile(ctx, root, "inference.lock", syscall.LOCK_EX, poll)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	// Check after acquiring inference ownership: a previous live call may have
	// completed while we waited, but an interrupted call retains its witness.
	if err := checkRetainedHostWork(root, pipeline); err != nil {
		return nil, err
	}
	if err := capacity.CheckInferenceIdle(root); err != nil {
		return nil, err
	}
	ok = true
	return &inferenceLease{file: file, pipeline: pipeline, root: root}, nil
}

func lockInferenceFile(ctx context.Context, root, name string, mode int, poll time.Duration) (*os.File, error) {
	// Never unlink the lock: replacing its inode would admit a second holder.
	fd, err := syscall.Open(filepath.Join(root, name), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "inference capacity")
	ok := false
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, errors.New("inference capacity is not a regular file")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err = syscall.Flock(fd, mode|syscall.LOCK_NB)
		if err == nil {
			ok = true
			return file, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// A missing process does not prove that Docker work ended. Check only retained
// ownership, without interpreting or clearing another workload's records.
func checkRetainedHostWork(root string, pipeline *os.File) error {
	stat, err := pipeline.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != 0 {
		return errors.New("an interrupted pipeline needs reconciliation before inference can start")
	}
	if _, err := os.Lstat(filepath.Join(root, "local-build.json")); err == nil {
		return errors.New("a retained local build needs reconciliation before inference can start")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect retained local build: %w", err)
	}
	return workbench.CheckInferenceReservations(root)
}

func (l *inferenceLease) close() {
	l.file.Close()
	l.pipeline.Close()
}

// beginActive precedes every runtime effect, including owned-model eviction.
// Persist no prompt, model identifier, endpoint, credentials, PID or timestamp.
// A partial write remains a fence; only a positively completed operation may
// remove its exact witness. Creating and syncing the directory entry makes a
// worker or machine restart preserve the admission decision.
func (l *inferenceLease) beginActive() error {
	if len(l.active) != 0 {
		return capacity.ErrInferenceUnreconciled
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Version int    `json:"version"`
		Attempt string `json:"attempt"`
	}{Version: 1, Attempt: hex.EncodeToString(nonce[:])})
	if err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(l.root, capacity.InferenceActivityFile), syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return fmt.Errorf("record inference activity: %w", err)
	}
	file := os.NewFile(uintptr(fd), "inference activity")
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := syncInferenceDirectory(l.root); err != nil {
		return err
	}
	l.active = data
	return nil
}

// completeActive requires the runtime's terminal response. Cancellation, EOF,
// a transport failure and local process death provide no such evidence.
func (l *inferenceLease) completeActive() error {
	if len(l.active) == 0 {
		return capacity.ErrInferenceUnreconciled
	}
	path := filepath.Join(l.root, capacity.InferenceActivityFile)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "inference activity")
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != int64(len(l.active)) {
		return capacity.ErrInferenceUnreconciled
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil || !bytes.Equal(data, l.active) {
		return capacity.ErrInferenceUnreconciled
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(stat, current) {
		return capacity.ErrInferenceUnreconciled
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := syncInferenceDirectory(l.root); err != nil {
		return err
	}
	l.active = nil
	return nil
}

func syncInferenceDirectory(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (l *inferenceLease) readOwned() (map[string]models.Info, error) {
	owned := make(map[string]models.Info)
	fd, err := syscall.Open(filepath.Join(l.root, "inference-owned.json"), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return owned, nil
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "inference ownership")
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64<<10 {
		return nil, errors.New("invalid inference ownership record")
	}
	var records []ownedModel
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	if err := decoder.Decode(&records); err != nil {
		return nil, fmt.Errorf("read inference ownership: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid trailing inference ownership data")
	}
	for _, record := range records {
		if err := validateOwnedModel(record); err != nil {
			return nil, err
		}
		info := models.Info{ID: record.ID, BaseURL: record.BaseURL, Kind: models.KindOllama}
		owned[resourceKey(info)] = info
	}
	return owned, nil
}

func validateOwnedModel(record ownedModel) error {
	u, err := url.Parse(record.BaseURL)
	if err != nil || !localRuntime(record.BaseURL) || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || record.ID == "" {
		return errors.New("invalid owned inference endpoint")
	}
	return nil
}

// Only runtime identity is persisted, never credentials or other model policy.
// Atomic replacement leaves the prior valid record if the process dies midway.
func (l *inferenceLease) saveOwned(owned map[string]models.Info) error {
	records := []ownedModel{}
	for _, info := range owned {
		if info.Kind == models.KindOllama {
			record := ownedModel{ID: info.ID, BaseURL: info.BaseURL}
			if err := validateOwnedModel(record); err != nil {
				return err
			}
			records = append(records, record)
		}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return errors.New("inference ownership record exceeds limit")
	}
	file, err := os.CreateTemp(l.root, ".inference-owned-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(l.root, "inference-owned.json"))
}
