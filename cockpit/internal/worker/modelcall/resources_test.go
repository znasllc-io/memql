package modelcall

import (
	"context"
	"fmt"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

func resourcesForTest(probe func() (memoryHeadroom, error)) *hostResources {
	return &hostResources{gate: make(chan struct{}, 1), owned: make(map[string]models.Info), probe: probe, poll: time.Millisecond}
}

func TestResourcesSerializeDifferentModelsAcrossManagers(t *testing.T) {
	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			fmt.Fprint(w, `{"models":[]}`)
			return
		}
		if r.URL.Path == "/api/generate" {
			fmt.Fprint(w, `{"done":true}`)
			return
		}
		n := active.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		active.Add(-1)
		fmt.Fprintln(w, `{"done":true,"done_reason":"stop"}`)
	}))
	defer srv.Close()
	limiter := NewLimiter()
	limiter.resources = resourcesForTest(func() (memoryHeadroom, error) { return memoryHeadroom{Total: 64 * gib, Available: 40 * gib}, nil })
	inv := inventoryWith(ollamaModel(srv.URL, "a", models.Attributes{MaxConcurrent: 2}), ollamaModel(srv.URL, "b", models.Attributes{MaxConcurrent: 2}))
	a := NewManager(Options{Inventory: inv, Limiter: limiter})
	b := NewManager(Options{Inventory: inv, Limiter: limiter})
	x, y := newRecorder(), newRecorder()
	a.Start(context.Background(), x, start("a", "a", KindChat))
	b.Start(context.Background(), y, start("b", "b", KindChat))
	for _, rec := range []*recorder{x, y} {
		select {
		case <-rec.done:
			if rec.end.Error != "" {
				t.Fatal(rec.end.Error)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("call hung")
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("different models overlapped: %d", peak.Load())
	}
}

func TestResourcesReclaimOnlyOwnedIdleModelsBeforeDifferentRuntime(t *testing.T) {
	var unloaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		unloaded.Store(true)
		fmt.Fprint(w, `{"done":true}`)
	}))
	defer srv.Close()
	r := resourcesForTest(func() (memoryHeadroom, error) {
		if !unloaded.Load() {
			t.Error("memory checked before reclaim")
		}
		return memoryHeadroom{Total: 64 * gib, Available: 30 * gib}, nil
	})
	old := models.Info{ID: "large-chat", Kind: models.KindOllama, BaseURL: srv.URL}
	r.owned[resourceKey(old)] = old
	image := models.Info{ID: "image", Kind: models.KindOpenAICompatible, BaseURL: srv.URL + "/v1", PeakMemoryBytes: 16 * gib}
	release, err := r.acquire(context.Background(), image, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release.close()
	if _, exists := r.owned[resourceKey(old)]; exists {
		t.Fatal("unloaded runner retained")
	}
}

func TestResourcesWaitForHeadroomAndCancelWithoutLeakingGate(t *testing.T) {
	var available atomic.Uint64
	available.Store(10 * gib)
	r := resourcesForTest(func() (memoryHeadroom, error) {
		return memoryHeadroom{Total: 64 * gib, Available: available.Load()}, nil
	})
	info := models.Info{ID: "any-modality", PeakMemoryBytes: 16 * gib}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if release, err := r.acquire(ctx, info, http.DefaultClient, slog.Default()); err == nil {
		release.close()
		t.Fatal("overcommitted memory")
	}
	available.Store(24 * gib)
	release, err := r.acquire(context.Background(), info, http.DefaultClient, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release.close()
}

func TestResourcesDoNotUnloadWarmModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("warm runner evicted: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":[{"name":"warm","size":17179869184}]}`)
	}))
	defer srv.Close()
	r := resourcesForTest(func() (memoryHeadroom, error) { return memoryHeadroom{Total: 64 * gib, Available: 8 * gib}, nil })
	info := models.Info{ID: "warm", Kind: models.KindOllama, BaseURL: srv.URL, PeakMemoryBytes: 16 * gib}
	r.owned[resourceKey(info)] = info
	release, err := r.acquire(context.Background(), info, srv.Client(), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release.close()
}

func TestResourcesRejectPressureUnknownProbeAndImpossiblePeak(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state memoryHeadroom
		err   error
	}{
		{"pressure", memoryHeadroom{Total: 64 * gib, Available: 40 * gib, Pressure: true}, nil},
		{"small host", memoryHeadroom{Total: 8 * gib, Available: 7 * gib}, nil},
		{"unknown", memoryHeadroom{}, fmt.Errorf("probe unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := resourcesForTest(func() (memoryHeadroom, error) { return tc.state, tc.err })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if release, err := r.acquire(ctx, models.Info{ID: "model", PeakMemoryBytes: 16 * gib}, http.DefaultClient, slog.Default()); err == nil {
				release.close()
				t.Fatal("unsafe admission")
			}
		})
	}
}

func TestLocalRuntimeIsExplicitLoopback(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:11434", "http://[::1]:1234/v1", "http://localhost:1234"} {
		if !localRuntime(base) {
			t.Error(base)
		}
	}
	for _, base := range []string{"https://provider.example/v1", "http://localhost.example/v1", "broken"} {
		if localRuntime(base) {
			t.Error(base)
		}
	}
}

func TestMemoryReservationScalesWithWorkloadWithoutOverflow(t *testing.T) {
	image := models.Info{PeakMemoryBytes: 16 * gib, MemoryReferencePixels: 1024 * 1024}
	for _, tc := range []struct {
		width, height, count int32
		want                 uint64
	}{
		{0, 0, 0, 16 * gib}, {512, 512, 1, 16 * gib}, {2048, 1024, 1, 32 * gib}, {1024, 1024, 2, 32 * gib}, {math.MaxInt32, math.MaxInt32, math.MaxInt32, math.MaxUint64},
	} {
		s := &memqlv1.ModelCallStart{Kind: KindImage, Image: &memqlv1.ModelCallImageRequest{Width: tc.width, Height: tc.height, Count: tc.count}}
		if got := requestMemory(image, s).PeakMemoryBytes; got != tc.want {
			t.Errorf("%+v: got %d want %d", tc, got, tc.want)
		}
	}
	chat := models.Info{Kind: models.KindOllama, ModelBytes: 16 * gib}
	base := requestMemory(chat, &memqlv1.ModelCallStart{}).PeakMemoryBytes
	large := requestMemory(chat, &memqlv1.ModelCallStart{Params: &memqlv1.ModelCallParams{ContextTokens: 131072}}).PeakMemoryBytes
	if large != base+6*gib {
		t.Fatalf("large context reservation %d, baseline %d", large, base)
	}
}
