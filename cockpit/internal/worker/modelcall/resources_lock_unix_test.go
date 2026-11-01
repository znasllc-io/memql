//go:build darwin || linux

package modelcall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

func TestInferenceWaitsForBuildButSharesWorkbenchAcrossProcesses(t *testing.T) {
	for name, mode := range map[string]int{"build": syscall.LOCK_EX, "workbench": syscall.LOCK_SH} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			file, err := os.OpenFile(filepath.Join(root, "pipeline.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			if mode == syscall.LOCK_SH {
				slot, err := os.OpenFile(filepath.Join(root, "workbench-0.lock"), os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer slot.Close()
				if err := syscall.Flock(int(slot.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				if err := json.NewEncoder(slot).Encode(map[string]string{"Name": "memql-workbench-" + strings.Repeat("a", 32), "Key": strings.Repeat("b", 64), "DockerID": "recorded-daemon"}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInferenceLeaseAcrossProcesses$")
			busy := ""
			if mode == syscall.LOCK_EX {
				busy = "1"
			}
			cmd.Env = append(os.Environ(), "MEMQL_TEST_INFERENCE_LEASE_ROOT="+root, "MEMQL_TEST_INFERENCE_LEASE_BUSY="+busy)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("inference did not respect another process's %s reservation: %v %s", name, err, out)
			}
		})
	}
}

func TestInferenceLeaseExcludesBuildButSharesWorkbenchReservations(t *testing.T) {
	root := t.TempDir()
	lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	for _, mode := range []int{syscall.LOCK_EX, syscall.LOCK_SH} {
		file, err := os.OpenFile(filepath.Join(root, "pipeline.lock"), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		file.Close()
		if mode == syscall.LOCK_EX && !errors.Is(err, syscall.EWOULDBLOCK) || mode == syscall.LOCK_SH && err != nil {
			t.Fatalf("host reservation mode %d did not respect inference: %v", mode, err)
		}
	}
}

func TestInferencePreservesRetainedHostRecords(t *testing.T) {
	for _, name := range []string{"pipeline.lock", "local-build.json", "workbench-0.lock", "workbench-unknown.lock", "workbench-admission.lock", capacity.InferenceActivityFile} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, name)
			const retained = "retained-unconfirmed-effect"
			if err := os.WriteFile(path, []byte(retained), 0600); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
			if err == nil {
				lease.close()
				t.Fatal("retained operation admitted inference")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != retained {
				t.Fatal("another workload's record changed", err)
			}
		})
	}
}

func TestInferenceActivitySurvivesLockReleaseAndRequiresExactCompletion(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			root := t.TempDir()
			lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			until := time.Now().Add(time.Hour)
			if err := lease.beginActive(until); err != nil {
				lease.close()
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, capacity.InferenceActivityFile))
			var witness map[string]any
			decodeErr := json.Unmarshal(data, &witness)
			attempt, _ := witness["attempt"].(string)
			ends, _ := witness["until"].(string)
			endsAt, endsErr := time.Parse(time.RFC3339, ends)
			if err != nil || decodeErr != nil || len(witness) != 3 || witness["version"] != float64(2) || len(attempt) != 32 ||
				endsErr != nil || !endsAt.Equal(until) {
				lease.close()
				t.Fatalf("activity witness must contain only version, random identity and when it ends: %s %v", data, err)
			}
			if completed {
				if err := lease.completeActive(); err != nil {
					lease.close()
					t.Fatal(err)
				}
			}
			lease.close() // same kernel release as worker death, not completion
			replacement, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
			if completed {
				if err != nil {
					t.Fatal("confirmed completion kept capacity fenced", err)
				}
				replacement.close()
			} else {
				if !errors.Is(err, capacity.ErrInferenceUnreconciled) {
					if replacement != nil {
						replacement.close()
					}
					t.Fatal("process death was treated as runtime completion", err)
				}
				if got, err := os.ReadFile(filepath.Join(root, capacity.InferenceActivityFile)); err != nil || string(got) != string(data) {
					t.Fatal("uncertain activity witness changed", err)
				}
			}
		})
	}
}

func TestUnconfirmedOwnedModelUnloadRetainsInferenceActivity(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/generate" {
			t.Errorf("unexpected runtime request %s", req.URL.Path)
			return
		}
		if err := capacity.CheckInferenceIdle(root); !errors.Is(err, capacity.ErrInferenceUnreconciled) {
			t.Error("unload started without a durable witness", err)
		}
		fmt.Fprint(w, `{"done":false}`)
	}))
	defer srv.Close()
	lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	old := models.Info{ID: "owned", Kind: models.KindOllama, BaseURL: srv.URL}
	if err := lease.saveOwned(map[string]models.Info{resourceKey(old): old}); err != nil {
		lease.close()
		t.Fatal(err)
	}
	lease.close()
	resources := resourcesForTest(func() (memoryHeadroom, error) {
		t.Error("unconfirmed unload reached memory admission")
		return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil
	})
	resources.leaseRoot = func() (string, error) { return root, nil }
	reservation, err := resources.acquire(context.Background(), models.Info{ID: "next", Kind: models.KindOpenAICompatible, BaseURL: srv.URL}, srv.Client(), slog.Default())
	if err == nil {
		reservation.close()
		t.Fatal("unconfirmed unload admitted another runtime")
	}
	if !errors.Is(capacity.CheckInferenceIdle(root), capacity.ErrInferenceUnreconciled) {
		t.Fatal("unconfirmed unload released shared capacity")
	}
}

func TestInferenceCompletionDoesNotClearChangedWitness(t *testing.T) {
	root := t.TempDir()
	lease, err := acquireInferenceLease(context.Background(), root, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	if err := lease.beginActive(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, capacity.InferenceActivityFile)
	other := strings.Repeat("x", len(lease.active))
	if err := os.WriteFile(path, []byte(other), 0600); err != nil {
		t.Fatal(err)
	}
	if err := lease.completeActive(); !errors.Is(err, capacity.ErrInferenceUnreconciled) {
		t.Fatal("changed witness cleared", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != other {
		t.Fatal("changed witness not preserved", err)
	}
}

func TestRuntimeOutcomeAloneClearsInferenceActivity(t *testing.T) {
	for _, outcome := range []string{"complete", "truncated", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/ps" {
					fmt.Fprint(w, `{"models":[]}`)
					return
				}
				if req.URL.Path != "/api/chat" {
					t.Errorf("unexpected runtime request %s", req.URL.Path)
					return
				}
				if err := capacity.CheckInferenceIdle(root); !errors.Is(err, capacity.ErrInferenceUnreconciled) {
					t.Error("runtime request preceded durable activity", err)
				}
				drain(req)
				switch outcome {
				case "complete":
					fmt.Fprintln(w, `{"done":true,"done_reason":"stop"}`)
				case "truncated":
					fmt.Fprintln(w, `{"done":false,"message":{"content":"partial"}}`)
				case "cancelled":
					cancel()
					<-req.Context().Done()
				}
			}))
			defer srv.Close()
			limiter := NewLimiter()
			limiter.resources = resourcesForTest(func() (memoryHeadroom, error) { return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil })
			limiter.resources.leaseRoot = func() (string, error) { return root, nil }
			info := ollamaModel(srv.URL, "model", models.Attributes{MaxConcurrent: 1})
			manager := NewManager(Options{Inventory: inventoryWith(info), Limiter: limiter})
			rec := newRecorder()
			request := start("request", info.ID, KindChat)
			manager.run(ctx, rec, &call{requestID: "request", cancel: cancel}, info, limitsFrom(request.Limits), request, &deltaStream{sender: rec, requestID: "request"})
			end := rec.wait(t)
			if outcome == "complete" {
				if end.Error != "" || capacity.CheckInferenceIdle(root) != nil {
					t.Fatal("successful terminal response did not release capacity", end)
				}
			} else if end.Error == "" || !errors.Is(capacity.CheckInferenceIdle(root), capacity.ErrInferenceUnreconciled) {
				t.Fatal("uncertain runtime outcome released capacity", end)
			}
		})
	}
}

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
			otherRelease.close()
		}
		t.Fatalf("second worker bypassed active model: %v", err)
	}
	if unloaded.Load() {
		t.Fatal("active model unloaded")
	}
	release.close()
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
	release.close()
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
