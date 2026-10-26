package imageverify

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type attemptMarker struct {
	Version                                 int
	ID, RequestSHA256, Workspace, SourceSHA string
}

func markerBytes(r Record) []byte {
	b, _ := json.Marshal(attemptMarker{1, r.ID, r.AttemptSHA256, r.Workspace, r.SourceSHA})
	return b
}

// The marker is immutable for the lifetime of the original build. Cleanup has
// a new service invocation/request identity, but must never replace this source
// binding; a second interrupted cleanup still reconciles the same attempt.
func createAttempt(c ServiceConfig, r Record, q serviceRequest) (err error) {
	if len(filepath.Join(r.Root, "buildkit.sock")) >= 100 {
		return errors.New("private builder socket path exceeds its supported bound")
	}
	if err = os.Mkdir(r.Root, 0700); err != nil {
		return errors.New("image attempt state already exists or cannot be created")
	}
	// This process exclusively created the empty directory, and has not started
	// source work. Partial preparation may therefore be removed on a local error.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(r.Root)
		}
	}()
	if err = writeAttemptMarker(r); err != nil {
		return err
	}
	for _, dir := range []string{"home", "state", "output", "runtime"} {
		if err = os.Mkdir(filepath.Join(r.Root, dir), 0700); err != nil {
			return err
		}
	}
	return nil
}
func writeAttemptMarker(r Record) error {
	temp := filepath.Join(r.Root, ".attempt.json.pending")
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	data := markerBytes(r)
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = errors.New("short marker write")
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(temp, filepath.Join(r.Root, "attempt.json")); err != nil {
		return err
	}
	dir, err := os.Open(r.Root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func checkAttemptMarker(r Record) error {
	if err := validatePrivateDirectory(r.Root, r.Identity.OwnerUID); err != nil {
		return err
	}
	p := filepath.Join(r.Root, "attempt.json")
	stat, err := os.Lstat(p)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 4096 || stat.Mode().Perm() != 0600 {
		return errors.New("image attempt marker is invalid")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, markerBytes(r)) {
		return errors.New("image attempt marker differs")
	}
	return nil
}

// A crash before the marker's atomic rename cannot have started RootlessKit:
// READY is emitted only after this preparation finishes. Accept only the exact
// empty/private directory or a bounded prefix of the expected pending marker.
// Arbitrary contents, symlinks or a different marker remain fenced for review.
func prepareCleanupMarker(r Record) error {
	if err := checkAttemptMarker(r); err == nil {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(r.Root, "attempt.json")); !os.IsNotExist(err) {
		return errors.New("existing image attempt marker is not valid")
	}
	stat, err := os.Lstat(r.Root)
	if err != nil || !stat.IsDir() || stat.Mode().Perm() != 0700 {
		return errors.New("partial image preparation is not private")
	}
	uid, ok := serviceFileUID(stat)
	if !ok || uid != uint64(r.Identity.OwnerUID) {
		return errors.New("partial image preparation belongs to another account")
	}
	entries, err := os.ReadDir(r.Root)
	if err != nil {
		return err
	}
	if len(entries) > 1 {
		return errors.New("partial image preparation contains unknown state")
	}
	if len(entries) == 1 {
		if entries[0].Name() != ".attempt.json.pending" {
			return errors.New("partial image preparation contains unknown content")
		}
		p := filepath.Join(r.Root, entries[0].Name())
		s, err := os.Lstat(p)
		if err != nil || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > 4096 {
			return errors.New("partial image marker has an unexpected type")
		}
		data, err := os.ReadFile(p)
		if err != nil || !bytes.HasPrefix(markerBytes(r), data) {
			return errors.New("partial image marker differs")
		}
		if err = os.Remove(p); err != nil {
			return err
		}
	}
	return writeAttemptMarker(r)
}

// State and checkout roots are provisioned private directories, not aliases
// through a repository or another user's writable directory. Ownership and
// component checks precede any child creation/removal.
func validatePrivateDirectory(path string, owner int) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("private directory path is not canonical")
	}
	for current := path; ; current = filepath.Dir(current) {
		stat, err := os.Lstat(current)
		if err != nil || !stat.IsDir() {
			return errors.New("private directory crosses a symbolic or missing component")
		}
		uid, ok := serviceFileUID(stat)
		if !ok || (uid != 0 && uid != uint64(owner)) {
			return errors.New("private directory has a foreign ancestor")
		}
		if current == path {
			if uid != uint64(owner) || stat.Mode().Perm() != 0700 {
				return errors.New("private directory must belong to its owner with mode 0700")
			}
		} else if stat.Mode().Perm()&0022 != 0 && !(uid == 0 && stat.Mode()&os.ModeSticky != 0) {
			return errors.New("private directory has a writable ancestor")
		}
		if current == "/" {
			break
		}
	}
	return nil
}
