package filetxn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	if os.Getenv("MEMQL_FILETXN_CHILD") != "1" {
		return
	}
	path := os.Getenv("MEMQL_FILETXN_PATH")
	for range 20 {
		if err := Update(context.Background(), path, func(b []byte) ([]byte, error) {
			n, _ := strconv.Atoi(string(b))
			return []byte(strconv.Itoa(n + 1)), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentProcessesPreserveEveryChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$")
			cmd.Env = append(os.Environ(), "MEMQL_FILETXN_CHILD=1", "MEMQL_FILETXN_PATH="+path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child: %v %s", err, out)
			}
		})
	}
	wg.Wait()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "100" {
		t.Fatalf("lost changes: %q %v", b, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private file mode: %o", info.Mode().Perm())
	}
}

func TestCASRefusesStaleBytesAndPreservesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	if err := CompareAndSwap(t.Context(), path, nil, []byte("first")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("first"))
	digest := hex.EncodeToString(sum[:])
	if err := Update(t.Context(), path, func([]byte) ([]byte, error) { return []byte("Cockpit"), nil }); err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwap(t.Context(), path, &digest, []byte("Editor")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "Cockpit" {
		t.Fatalf("overwritten: %q", b)
	}
	if err := Update(t.Context(), path, func([]byte) ([]byte, error) { return nil, fmt.Errorf("refused") }); err == nil {
		t.Fatal("expected refusal")
	}
	b, _ = os.ReadFile(path)
	if string(b) != "Cockpit" {
		t.Fatalf("failed edit changed bytes: %q", b)
	}
}

func TestLockCancellationAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.yaml")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if err := CompareAndSwap(ctx, path, nil, []byte("no")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	target := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := CompareAndSwap(t.Context(), path, nil, []byte("no")); err == nil {
		t.Fatal("followed a symlink")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "secret" {
		t.Fatal("modified symlink target")
	}
}
