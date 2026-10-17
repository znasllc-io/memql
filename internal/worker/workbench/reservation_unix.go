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
)

var ErrBusy = errors.New("fleet workbench capacity is occupied")
var ErrInterrupted = errors.New("an interrupted workbench must be reconciled before starting more work")

type reservation struct{ slot, pipeline *os.File }
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
	slot, err := lockFile(filepath.Join(root, fmt.Sprintf("workbench-%d.lock", slotIndex)), false)
	if err != nil {
		pipeline.Close()
		return nil, err
	}
	return &reservation{slot: slot, pipeline: pipeline}, nil
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
	var v record
	stat, err := r.slot.Stat()
	if err != nil {
		return v, err
	}
	if stat.Size() == 0 {
		return v, nil
	}
	if _, err = r.slot.Seek(0, io.SeekStart); err != nil {
		return v, err
	}
	if err = json.NewDecoder(io.LimitReader(r.slot, 4096)).Decode(&v); err != nil {
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
