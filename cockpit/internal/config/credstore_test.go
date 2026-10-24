package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// FileStore
// ---------------------------------------------------------------------------

func TestFileStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	fs := &FileStore{Dir: dir}

	got, err := fs.Get("local")
	if err != nil {
		t.Fatalf("Get on empty store: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for missing entry, got %+v", got)
	}

	want := &StoredToken{
		AccessToken:  "at-abc",
		RefreshToken: "rt-def",
		Expiry:       time.Now().Add(time.Hour).Round(time.Second),
	}
	if err := fs.Put("local", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err = fs.Get("local")
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if got == nil {
		t.Fatal("nil token after Put")
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round-tripped token mismatch: got %+v want %+v", got, want)
	}
	// Expiry is JSON-encoded to RFC3339; round-trip equality is via
	// Equal not == because the unmarshaled time has a different
	// monotonic clock reading.
	if !got.Expiry.Equal(want.Expiry) {
		t.Errorf("expiry mismatch: got %v want %v", got.Expiry, want.Expiry)
	}
}

func TestFileStore_FileModeEnforced(t *testing.T) {
	dir := t.TempDir()
	fs := &FileStore{Dir: dir}

	if err := fs.Put("local", &StoredToken{AccessToken: "at"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Check the written mode is 0600.
	path := filepath.Join(dir, "local.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", info.Mode().Perm())
	}

	// Externally relax the mode -- Get must reject.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err = fs.Get("local")
	if err == nil {
		t.Fatal("Get on 0644 file should reject")
	}
	if !strings.Contains(err.Error(), "must be 0600") {
		t.Errorf("error %q lacks the 0600 hint", err)
	}
}

func TestFileStore_DeleteMissingIsNoOp(t *testing.T) {
	fs := &FileStore{Dir: t.TempDir()}
	if err := fs.Delete("never-existed"); err != nil {
		t.Fatalf("Delete on missing key: %v", err)
	}
}

func TestFileStore_List(t *testing.T) {
	dir := t.TempDir()
	fs := &FileStore{Dir: dir}
	for _, name := range []string{"local", "staging", "prod"} {
		if err := fs.Put(name, &StoredToken{AccessToken: "x"}); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	// Stray non-JSON file should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	got, err := fs.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := map[string]bool{"local": true, "staging": true, "prod": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected entry %q", name)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("missing entries: %v", want)
	}
}

func TestFileStore_PutNilRejected(t *testing.T) {
	fs := &FileStore{Dir: t.TempDir()}
	if err := fs.Put("local", nil); err == nil {
		t.Error("Put(nil) should reject")
	}
}

// ---------------------------------------------------------------------------
// Resolve() chain
// ---------------------------------------------------------------------------

// fakeStore is an in-memory CredentialStore used to exercise the
// resolution chain without touching the OS keyring or the disk.
type fakeStore struct {
	name string
	mu   sync.Mutex
	data map[string]*StoredToken
}

func newFakeStore(name string) *fakeStore {
	return &fakeStore{name: name, data: map[string]*StoredToken{}}
}

func (f *fakeStore) Name() string { return f.name }

func (f *fakeStore) Get(cluster string) (*StoredToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.data[cluster]
	if !ok {
		return nil, nil
	}
	cp := *t
	return &cp, nil
}

func (f *fakeStore) Put(cluster string, token *StoredToken) error {
	if token == nil {
		return errors.New("fake store: nil token")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *token
	f.data[cluster] = &cp
	return nil
}

func (f *fakeStore) Delete(cluster string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, cluster)
	return nil
}

func TestResolve_AutoProbePrefersKeyringWhenHealthy(t *testing.T) {
	t.Setenv(EnvCredStore, "")
	kr := newFakeStore("keyring")
	file := newFakeStore("file")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	s, err := Resolve(ResolveOptions{
		KeyringProbe:     func() (CredentialStore, error) { return kr, nil },
		FileStoreFactory: func() CredentialStore { return file },
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.Name() != "keyring" {
		t.Errorf("expected keyring to win; got %q", s.Name())
	}
	if !strings.Contains(buf.String(), "backend=keyring") {
		t.Errorf("startup log should name keyring; got %q", buf.String())
	}
}

func TestResolve_FallsBackWhenKeyringUnavailable(t *testing.T) {
	t.Setenv(EnvCredStore, "")
	file := newFakeStore("file")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	s, err := Resolve(ResolveOptions{
		KeyringProbe:     func() (CredentialStore, error) { return nil, errors.New("dbus not running") },
		FileStoreFactory: func() CredentialStore { return file },
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.Name() != "file" {
		t.Errorf("expected file fallback; got %q", s.Name())
	}
	if !strings.Contains(buf.String(), "OS keyring unavailable") {
		t.Errorf("operator-facing log should announce fallback; got %q", buf.String())
	}
}

func TestResolve_OperatorOverrideFile(t *testing.T) {
	t.Setenv(EnvCredStore, "file")
	kr := newFakeStore("keyring")
	file := newFakeStore("file")

	s, err := Resolve(ResolveOptions{
		KeyringProbe:     func() (CredentialStore, error) { return kr, nil },
		FileStoreFactory: func() CredentialStore { return file },
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.Name() != "file" {
		t.Errorf("override=file should win over a healthy keyring; got %q", s.Name())
	}
}

func TestResolve_OperatorOverrideKeyringErrorsWhenUnavailable(t *testing.T) {
	t.Setenv(EnvCredStore, "keyring")
	file := newFakeStore("file")

	_, err := Resolve(ResolveOptions{
		KeyringProbe:     func() (CredentialStore, error) { return nil, errors.New("dbus not running") },
		FileStoreFactory: func() CredentialStore { return file },
	})
	if err == nil {
		t.Fatal("override=keyring with unavailable keyring should error (not silently fall back)")
	}
	if !strings.Contains(err.Error(), "MEMQL_COCKPIT_CRED_STORE=keyring") {
		t.Errorf("error %q should name the override that produced it", err)
	}
}

func TestResolve_UnknownOverrideFallsThroughWithWarning(t *testing.T) {
	t.Setenv(EnvCredStore, "vault") // not supported
	kr := newFakeStore("keyring")
	file := newFakeStore("file")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	s, err := Resolve(ResolveOptions{
		KeyringProbe:     func() (CredentialStore, error) { return kr, nil },
		FileStoreFactory: func() CredentialStore { return file },
		Logger:           logger,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if s.Name() != "keyring" {
		t.Errorf("unknown override should auto-probe; got %q", s.Name())
	}
	if !strings.Contains(buf.String(), "ignoring unrecognized MEMQL_COCKPIT_CRED_STORE value") {
		t.Errorf("warning expected for unknown override; got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// Package-level wrapper routes through ActiveStore
// ---------------------------------------------------------------------------

func TestPackageWrappers_RouteThroughActiveStore(t *testing.T) {
	previous := ActiveStore()
	t.Cleanup(func() { SetActiveStore(previous) })

	fake := newFakeStore("test")
	SetActiveStore(fake)

	if err := SaveToken("alpha", &StoredToken{AccessToken: "at-alpha"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	got, err := LoadToken("alpha")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got == nil || got.AccessToken != "at-alpha" {
		t.Errorf("LoadToken did not route through ActiveStore; got %+v", got)
	}
	if err := DeleteToken("alpha"); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	got, err = LoadToken("alpha")
	if err != nil {
		t.Fatalf("LoadToken after delete: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after DeleteToken; got %+v", got)
	}
}
