//go:build darwin || linux

package modelcall

import (
	"context"
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
