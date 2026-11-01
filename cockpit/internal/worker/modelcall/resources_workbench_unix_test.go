//go:build darwin || linux

package modelcall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/workbench"
)

type inferenceWorkbenchEngine struct {
	files            map[string]map[string]string
	created, removed int
}

func (*inferenceWorkbenchEngine) Ready(context.Context) (workbench.Runtime, error) {
	return workbench.Runtime{ID: "recorded-daemon", CPUs: 8, MemoryBytes: 16 << 30}, nil
}

func (e *inferenceWorkbenchEngine) Create(_ context.Context, _ workbench.Runtime, name, _ string) error {
	e.created++
	e.files[name] = map[string]string{}
	return nil
}

func (e *inferenceWorkbenchEngine) Remove(_ context.Context, _ workbench.Runtime, name string) error {
	e.removed++
	delete(e.files, name)
	return nil
}

func (e *inferenceWorkbenchEngine) Execute(_ context.Context, _ workbench.Runtime, name string, req workbench.Request) (workbench.Result, error) {
	files, exists := e.files[name]
	if !exists {
		return workbench.Result{}, fmt.Errorf("workspace disappeared")
	}
	path, _ := req.Args["path"].(string)
	if req.Action == "fs_write" {
		files[path], _ = req.Args["data"].(string)
	}
	return workbench.Result{OK: true, Action: req.Action, Payload: files[path]}, nil
}

// Exercise both real Managers: a goal's persistent tool workspace must retain
// its files across the next local model turn, without waiting for the idle
// sweeper to destroy it. No Docker process or installed model is involved.
func TestModelToolModelKeepsSameWorkbench(t *testing.T) {
	root := t.TempDir()
	engine := &inferenceWorkbenchEngine{files: map[string]map[string]string{}}
	bench := workbench.New(engine, root)
	t.Cleanup(func() { _ = bench.CloseHome("home") })
	id := workbench.Identity{Home: "home", Owner: "owner", Caller: "caller", Registration: "registration", Machine: "machine", Run: "goal", ExpiresAt: time.Now().Add(workbench.Lifetime)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		if req.URL.Path != "/api/chat" {
			t.Errorf("unexpected runtime path %s", req.URL.Path)
		}
		fmt.Fprintln(w, `{"done":true,"done_reason":"stop","message":{"content":"ready"}}`)
	}))
	defer srv.Close()
	limiter := NewLimiter()
	limiter.resources = resourcesForTest(func() (memoryHeadroom, error) {
		return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil
	})
	limiter.resources.leaseRoot = func() (string, error) { return root, nil }
	info := ollamaModel(srv.URL, "model", models.Attributes{MaxConcurrent: 1})
	info.PeakMemoryBytes = 16 * gib
	manager := NewManager(Options{Inventory: inventoryWith(info), Limiter: limiter})
	modelTurn := func(requestID string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		recorder := newRecorder()
		request := start(requestID, info.ID, KindChat)
		manager.run(ctx, recorder, &call{requestID: requestID, cancel: cancel}, info, limitsFrom(request.Limits), request, &deltaStream{sender: recorder, requestID: requestID})
		if end := recorder.wait(t); end.Error != "" {
			t.Fatalf("model turn with persistent workspace: %s", end.Error)
		}
	}
	modelTurn("first")
	if _, err := bench.Execute(context.Background(), id, workbench.Request{Action: "fs_write", Args: map[string]any{"path": "state.txt", "data": "retained between turns"}}); err != nil {
		t.Fatal(err)
	}
	modelTurn("second")
	result, err := bench.Execute(context.Background(), id, workbench.Request{Action: "fs_read", Args: map[string]any{"path": "state.txt"}})
	if err != nil || result.Payload != "retained between turns" || engine.created != 1 || engine.removed != 0 {
		t.Fatalf("model turn replaced or destroyed its tool workspace: result=%+v created=%d removed=%d err=%v", result, engine.created, engine.removed, err)
	}
}

func TestInferenceReservesEveryWorkbenchSlotWithoutChargingVRAM(t *testing.T) {
	for _, tc := range []struct {
		name string
		pool memoryHeadroom
		host capacity.SystemHeadroom
		peak uint64
		fits bool
	}{
		{"unified pool fits model and all slots", memoryHeadroom{Total: 16 * gib, Available: 12 * gib}, capacity.SystemHeadroom{}, 8 * gib, true},
		{"unified pool refuses unreserved slot growth", memoryHeadroom{Total: 16 * gib, Available: 12*gib - 1}, capacity.SystemHeadroom{}, 8 * gib, false},
		{"dedicated GPU keeps workspace RAM separate", memoryHeadroom{Total: 24 * gib, Available: 22 * gib, DedicatedGPU: true}, capacity.SystemHeadroom{Total: 16 * gib, Available: 4 * gib}, 20 * gib, true},
		{"dedicated GPU still protects workspace RAM", memoryHeadroom{Total: 24 * gib, Available: 22 * gib, DedicatedGPU: true}, capacity.SystemHeadroom{Total: 16 * gib, Available: 4*gib - 1}, 20 * gib, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resources := resourcesForTest(func() (memoryHeadroom, error) { return tc.pool, nil })
			resources.systemProbe = func() (capacity.SystemHeadroom, error) { return tc.host, nil }
			root := t.TempDir()
			resources.leaseRoot = func() (string, error) { return root, nil }
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			reservation, err := resources.acquire(ctx, models.Info{ID: "model", PeakMemoryBytes: tc.peak}, http.DefaultClient, slog.Default())
			if err == nil {
				reservation.close()
			}
			if (err == nil) != tc.fits {
				t.Fatalf("fits=%v: %v", tc.fits, err)
			}
		})
	}
}

// The estimate and the workspace budget together, in the shape production runs:
// a lease, and host RAM probed beside a dedicated card. The figures are from
// machines' registrations. A dedicated card's workspaces are charged to host RAM,
// so the 24 GB class keeps its model, where a unified pool has to hold them too:
// 19.96 GB of model, 4 GiB of reserve and 3 GiB of slots is 27.48 GB.
func TestTheTwentyFourGigabytePickBesideEveryWorkbenchSlot(t *testing.T) {
	const file27B = uint64(17741872154)
	for _, tc := range []struct {
		name string
		pool memoryHeadroom
		host capacity.SystemHeadroom
		fits bool
	}{
		{"RTX 5090 Laptop", memoryHeadroom{Total: 25651314688, Available: 23528996864, DedicatedGPU: true}, capacity.SystemHeadroom{Total: 66793422848, Available: 40_000_000_000}, true},
		{"RTX 4090", memoryHeadroom{Total: 25757220864, Available: 25300000000, DedicatedGPU: true}, capacity.SystemHeadroom{Total: 33441214464, Available: 20_000_000_000}, true},
		{"64 GiB M4 Max with room", memoryHeadroom{Total: 64 * gib, Available: 27_500_000_000}, capacity.SystemHeadroom{}, true},
		{"64 GiB M4 Max without it", memoryHeadroom{Total: 64 * gib, Available: 27_400_000_000}, capacity.SystemHeadroom{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := noResidentModels(t)
			resources := resourcesForTest(func() (memoryHeadroom, error) { return tc.pool, nil })
			resources.systemProbe = func() (capacity.SystemHeadroom, error) { return tc.host, nil }
			root := t.TempDir()
			resources.leaseRoot = func() (string, error) { return root, nil }
			resources.headroomWait = 20 * time.Millisecond
			info := models.Info{ID: "qwen3.8:27b", Kind: models.KindOllama, BaseURL: srv.URL, ModelBytes: file27B}
			reservation, err := resources.acquire(context.Background(), info, srv.Client(), slog.Default())
			if err == nil {
				reservation.close()
			}
			if (err == nil) != tc.fits {
				t.Fatalf("fits=%v: %v", tc.fits, err)
			}
		})
	}
}

// What the workspace budget alone puts out of reach is still something that can
// never fit, so it is refused at once. It used to reclaim first and then refuse,
// and only the budget's reserve could tell: 12.5 GiB leaves 3.5 GiB on this pool,
// past the 1 GiB the operating system needs and short of the 4 GiB with the slots.
func TestAModelTheWorkbenchBudgetRulesOutIsRefusedWithoutWaiting(t *testing.T) {
	resources := resourcesForTest(func() (memoryHeadroom, error) {
		return memoryHeadroom{Total: 16 * gib, Available: 15 * gib}, nil
	})
	root := t.TempDir()
	resources.leaseRoot = func() (string, error) { return root, nil }
	resources.headroomWait = time.Minute
	began := time.Now()
	reservation, err := resources.acquire(context.Background(), models.Info{ID: "model", PeakMemoryBytes: 12*gib + gib/2}, http.DefaultClient, slog.Default())
	if err == nil {
		reservation.close()
		t.Fatal("a model with no room beside the workspaces was admitted")
	}
	var refused *memoryRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("it should be a memory refusal, got %T: %v", err, err)
	}
	if waited := time.Since(began); waited > 10*time.Second {
		t.Fatalf("waited %s for room that can never exist", waited)
	}
}
