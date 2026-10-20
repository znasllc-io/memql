//go:build darwin || linux

package modelcall

import (
	"context"
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

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Independent of cluster configuration and MEMQL_HOME: separate workers still
// share the same GPU. The kernel releases this lease if a worker exits.
func inferenceCapacityRoot() (string, error) {
	owner, err := user.Current()
	if err != nil || owner.HomeDir == "" {
		return "", errors.New("cannot resolve worker account for inference admission")
	}
	return filepath.Join(owner.HomeDir, ".memql", "worker-capacity"), nil
}

type inferenceLease struct {
	file *os.File
	root string
}
type ownedModel struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseURL"`
}

func acquireInferenceLease(ctx context.Context, root string, poll time.Duration) (*inferenceLease, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// Never unlink the lock: replacing its inode would admit a second holder.
	fd, err := syscall.Open(filepath.Join(root, "inference.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
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
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			ok = true
			return &inferenceLease{file: file, root: root}, nil
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
func (l *inferenceLease) close() { l.file.Close() }

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
