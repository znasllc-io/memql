//go:build darwin || linux

package modelcall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

func TestInferenceLeaseAcrossProcesses(t *testing.T) {
	if root := os.Getenv("MEMQL_TEST_INFERENCE_LEASE_ROOT"); root != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		lease, err := acquireInferenceLease(ctx, root, time.Millisecond)
		if os.Getenv("MEMQL_TEST_INFERENCE_LEASE_BUSY") == "1" {
			if !errors.Is(err, context.DeadlineExceeded) {
				if lease != nil {
					lease.close()
				}
				t.Fatalf("second process bypassed occupied gate: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			lease.close()
		}
		return
	}
	root := t.TempDir()
	lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runChild := func(busy string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestInferenceLeaseAcrossProcesses$")
		cmd.Env = append(os.Environ(), "MEMQL_TEST_INFERENCE_LEASE_ROOT="+root, "MEMQL_TEST_INFERENCE_LEASE_BUSY="+busy)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v: %s", err, out)
		}
	}
	runChild("1")
	lease.close()
	runChild("0")
}

func TestSeparateWorkersRecoverOwnershipAndReclaimBeforeAdmission(t *testing.T) {
	var unloaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
		case "/api/generate":
			unloaded.Store(true)
			fmt.Fprint(w, `{"done":true}`)
		default:
			t.Errorf("unexpected %s", req.URL.Path)
		}
	}))
	defer srv.Close()
	root := t.TempDir()
	makeWorker := func() *hostResources {
		r := resourcesForTest(func() (memoryHeadroom, error) { return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil })
		r.leaseRoot = func() (string, error) { return root, nil }
		return r
	}
	a, b := makeWorker(), makeWorker()
	old := models.Info{ID: "large", Kind: models.KindOllama, BaseURL: srv.URL}
	release, err := a.acquire(context.Background(), old, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	image := models.Info{ID: "image", Kind: models.KindOpenAICompatible, BaseURL: srv.URL, PeakMemoryBytes: 16 * gib}
	if otherRelease, err := b.acquire(ctx, image, srv.Client(), slog.Default()); !errors.Is(err, context.DeadlineExceeded) {
		if otherRelease != nil {
			otherRelease()
		}
		t.Fatalf("second worker bypassed active model: %v", err)
	}
	if unloaded.Load() {
		t.Fatal("active model unloaded")
	}
	release()
	b.probe = func() (memoryHeadroom, error) {
		if !unloaded.Load() {
			t.Error("read headroom before reclaiming prior worker's model")
		}
		return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil
	}
	release, err = b.acquire(context.Background(), image, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release()
	lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	owned, err := lease.readOwned()
	if err != nil || len(owned) != 0 {
		t.Fatalf("stale ownership after reclaim: %v %v", owned, err)
	}
}

func TestInferenceLeaseRefusesUnsafeRecords(t *testing.T) {
	for _, data := range []string{`{`, `[] {}`, `[{"id":"x","baseURL":"https://remote.example"}]`, `[{"id":"x","baseURL":"http://secret@localhost"}]`} {
		t.Run(data, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "inference-owned.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			if _, err := lease.readOwned(); err == nil {
				t.Fatal("unsafe record accepted")
			}
		})
	}
	for _, name := range []string{"inference.lock", "inference-owned.json"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte(`[]`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
			if name == "inference.lock" {
				if err == nil {
					lease.close()
					t.Fatal("symlink lock accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			if _, err := lease.readOwned(); err == nil {
				t.Fatal("symlink record accepted")
			}
		})
	}
}
