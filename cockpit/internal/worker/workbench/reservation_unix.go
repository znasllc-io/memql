//go:build darwin || linux

package workbench

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
)

var ErrBusy = errors.New("fleet workbench capacity is occupied")
var ErrInterrupted = errors.New("an interrupted workbench must be reconciled before starting more work")

type reservation struct {
	slot, pipeline *os.File
	root           string
}
type record struct{ Name, Key, DockerID string }

func capacityRoot() (string, error) {
	owner, err := user.Current()
	if err != nil || owner.HomeDir == "" {
		return "", fmt.Errorf("cannot resolve machine capacity directory")
	}
	return filepath.Join(owner.HomeDir, ".memql", "worker-capacity"), nil
}
func lockFile(path string, shared bool) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "workbench reservation")
	mode := syscall.LOCK_EX
	if shared {
		mode = syscall.LOCK_SH
	}
	if err = syscall.Flock(fd, mode|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		file.Close()
		return nil, ErrInterrupted
	}
	return file, nil
}
func reserve(root string) (*reservation, error) {
	for i := 0; i < Slots; i++ {
		r, err := reserveAt(root, i)
		if errors.Is(err, ErrBusy) {
			continue
		}
		return r, err
	}
	return nil, ErrBusy
}
func reserveAt(root string, slotIndex int) (*reservation, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	pipeline, err := lockFile(filepath.Join(root, "pipeline.lock"), true)
	if err != nil {
		return nil, err
	}
	stat, err := pipeline.Stat()
	if err != nil || stat.Size() != 0 {
		pipeline.Close()
		return nil, ErrInterrupted
	}
	// A worker may have exited while its model runtime is still working. The
	// shared lock alone is insufficient after that exit; the separate activity
	// witness fences until the runtime's confirmed completion or the time it
	// names. This lock is shared, so a model call may be writing its own record
	// now: a witness past its end is ignored here, never removed.
	if err := capacity.CheckInferenceIdle(root); err != nil {
		pipeline.Close()
		return nil, fmt.Errorf("%w: %v", ErrInterrupted, err)
	}
	slot, err := lockFile(filepath.Join(root, fmt.Sprintf("workbench-%d.lock", slotIndex)), false)
	if err != nil {
		pipeline.Close()
		return nil, err
	}
	return &reservation{slot: slot, pipeline: pipeline, root: root}, nil
}

// Serialize the memory snapshot through container creation across worker
// processes. Slot locks still bound lifetimes; this short lock prevents two
// pending creations from both spending the same measured headroom.
func (r *reservation) admission() ([]record, func(), error) {
	lock, err := lockFile(filepath.Join(r.root, "workbench-admission.lock"), false)
	if err != nil {
		return nil, nil, err
	}
	release := func() { lock.Close() }
	var active []record
	for i := 0; i < Slots; i++ {
		fd, err := syscall.Open(filepath.Join(r.root, fmt.Sprintf("workbench-%d.lock", i)), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			release()
			return nil, nil, err
		}
		file := os.NewFile(uintptr(fd), "workbench record")
		v, err := readRecord(file)
		file.Close()
		if err != nil {
			release()
			return nil, nil, err
		}
		if v.Name != "" {
			active = append(active, v)
		}
	}
	return active, release, nil
}

// HasInterrupted is checked under the pipeline's EXCLUSIVE capacity lock.
// A dead worker may have left a container running after its flock disappeared.
// CI must not consume those resources until exact, daemon-bound cleanup wins.
func HasInterrupted(root string) (bool, error) {
	for i := 0; i < Slots; i++ {
		stat, err := os.Lstat(filepath.Join(root, fmt.Sprintf("workbench-%d.lock", i)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !stat.Mode().IsRegular() || stat.Size() != 0 {
			return true, nil
		}
	}
	return false, nil
}
func (r *reservation) previous() (record, error) {
	return readRecord(r.slot)
}
func readRecord(file *os.File) (record, error) {
	var v record
	stat, err := file.Stat()
	if err != nil {
		return v, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 4096 {
		return v, ErrInterrupted
	}
	if stat.Size() == 0 {
		return v, nil
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return v, err
	}
	if err = json.NewDecoder(io.LimitReader(file, 4096)).Decode(&v); err != nil {
		return v, ErrInterrupted
	}
	nonce, nonceErr := hex.DecodeString(strings.TrimPrefix(v.Name, "memql-workbench-"))
	key, keyErr := hex.DecodeString(v.Key)
	if !strings.HasPrefix(v.Name, "memql-workbench-") || nonceErr != nil || len(nonce) != 16 || keyErr != nil || len(key) != 32 || v.DockerID == "" {
		return v, ErrInterrupted
	}
	return v, nil
}
func (r *reservation) save(v record) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err = r.slot.WriteAt(b, 0); err != nil {
		return err
	}
	if err = r.slot.Truncate(int64(len(b))); err != nil {
		return err
	}
	return r.slot.Sync()
}
func (r *reservation) clear() error {
	if err := r.slot.Truncate(0); err != nil {
		return err
	}
	return r.slot.Sync()
}
func (r *reservation) close() { r.slot.Close(); r.pipeline.Close() }
