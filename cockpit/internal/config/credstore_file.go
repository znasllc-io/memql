package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FileStore implements CredentialStore over the historical
// `~/.memql/credentials/<cluster>.json` layout. Mode 0600 enforced
// at write; the load path runs VerifyCredentialFileMode to catch
// out-of-band drift.
//
// Always available -- used as the fallback when the OS keyring
// can't be reached (CI runners, headless servers, broken Secret
// Service installs, etc.) and as the back-compat path for users
// who haven't yet run `creds migrate-to-keyring`.
type FileStore struct {
	// Dir is the directory holding `<cluster>.json` files. Defaults
	// to filepath.Join(ConfigDir(), "credentials") when zero; tests
	// pass an explicit t.TempDir().
	Dir string
}

// NewFileStore builds a FileStore rooted at the default
// credentials directory.
func NewFileStore() *FileStore { return &FileStore{} }

// Name reports the backend identifier for logs / the `creds
// status` command.
func (f *FileStore) Name() string { return "file" }

func (f *FileStore) dir() string {
	if f.Dir != "" {
		return f.Dir
	}
	return filepath.Join(ConfigDir(), "credentials")
}

func (f *FileStore) path(cluster string) string {
	return filepath.Join(f.dir(), cluster+".json")
}

// Get reads the cached token for cluster. (nil, nil) on miss; non-
// nil error when the file exists but the mode check fails or the
// payload is malformed.
func (f *FileStore) Get(cluster string) (*StoredToken, error) {
	path := f.path(cluster)
	if _, statErr := os.Stat(path); statErr == nil {
		if err := VerifyCredentialFileMode(path); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read token: %w", err)
	}
	var token StoredToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	return &token, nil
}

// Put writes the token at mode 0600 under the cluster-named file.
// The parent directory is created at mode 0700 if missing.
func (f *FileStore) Put(cluster string, token *StoredToken) error {
	if token == nil {
		return fmt.Errorf("file store: refuse to persist nil token for %q", cluster)
	}
	dir := f.dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials dir: %w", err)
	}
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}
	return os.WriteFile(f.path(cluster), data, 0o600)
}

// Delete removes the credentials file for cluster. Missing is a
// no-op (matches the legacy DeleteToken behavior callers depend
// on for logout).
func (f *FileStore) Delete(cluster string) error {
	err := os.Remove(f.path(cluster))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// List returns the cluster names with a stored token file. Used by
// the migration command to know what to copy across.
func (f *FileStore) List() ([]string, error) {
	entries, err := os.ReadDir(f.dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 5 && name[len(name)-5:] == ".json" {
			names = append(names, name[:len(name)-5])
		}
	}
	return names, nil
}
