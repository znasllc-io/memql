package modelcall

import (
	"context"
	"errors"
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

const gib = uint64(1 << 30)

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
	// Availability is judged after the reclaim: only then is there room.
	r := resourcesForTest(func() (memoryHeadroom, error) {
		if !unloaded.Load() {
			return memoryHeadroom{Total: 64 * gib, Available: 4 * gib}, nil
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

// noResidentModels answers the one runtime read admission makes for a model
// that is not loaded.
func noResidentModels(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			t.Errorf("unexpected runtime call %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"models":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The catalog's pick for the 24 GB class is the 27B Q4, 20.1 GB resident at its
// 32K working context. A gate that estimated 24.3 GB for it refused it on every
// 24 GB card, permanently, while the engine's own admission sent the card the
// call. The figures are what such machines report: the first is an RTX 5090
// Laptop with a desktop session on it, read from a registration on 2026-11-01,
// and 17741872154 is the size Ollama reports for the tag.
func TestResourcesAdmitTheTwentyFourGigabytePickOnATwentyFourGigabyteCard(t *testing.T) {
	const file27B = uint64(17741872154)
	for _, tc := range []struct {
		name  string
		state memoryHeadroom
	}{
		{"RTX 5090 Laptop, idle under a desktop", memoryHeadroom{Total: 25651314688, Available: 23528996864}},
		{"RTX 4090, idle with no desktop", memoryHeadroom{Total: 25757220864, Available: 25300000000}},
		{"a card of exactly 24 GiB under a desktop", memoryHeadroom{Total: 24 * gib, Available: 23000000000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := noResidentModels(t)
			r := resourcesForTest(func() (memoryHeadroom, error) { return tc.state, nil })
			r.headroomWait = 20 * time.Millisecond
			info := models.Info{ID: "qwen3.8:27b", Kind: models.KindOllama, BaseURL: srv.URL, ModelBytes: file27B}
			release, err := r.acquire(context.Background(), info, srv.Client(), slog.Default())
			if err != nil {
				t.Fatalf("the class's own model was refused on the class's own card: %v", err)
			}
			release.close()
		})
	}
}

// The estimate has to cover what the catalog says a model occupies, or the gate
// stops protecting anything. Weights and residency are literals from
// dsl/models/seeds.memql, for the models it states at a 32K working context. The
// 2 GiB floor leaves the two smaller ones a little under, by 0.16 GB at most,
// which is close enough for a figure the catalog itself calls an estimate.
func TestWorkingSetCoversTheCatalogsResidencyAtThirtyTwoK(t *testing.T) {
	const slack = uint64(160_000_000)
	for _, tc := range []struct {
		model             string
		weights, resident uint64
	}{
		{"qwen3.5:9b", 6_600_000_000, 8_800_000_000},
		{"gemma4:12b", 7_600_000_000, 9_900_000_000},
		{"qwen3.8:27b", 18_000_000_000, 20_100_000_000},
		{"gemma4:26b", 18_100_000_000, 20_300_000_000},
	} {
		if got := workingSet(models.Info{Kind: models.KindOllama, ModelBytes: tc.weights}); got+slack < tc.resident {
			t.Errorf("%s: estimated %d, below the catalog's %d", tc.model, got, tc.resident)
		}
	}
}

// What can never fit is refused before anything happens to the machine. Each
// such call used to unload the idle model first, which can be the model the
// caller's fallback was about to need, and cost it a second cold load.
func TestResourcesRefuseAnImpossibleModelWithoutEvictingAnything(t *testing.T) {
	var unloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/generate" {
			unloads.Add(1)
		}
		fmt.Fprint(w, `{"done":true,"models":[]}`)
	}))
	defer srv.Close()
	r := resourcesForTest(func() (memoryHeadroom, error) { return memoryHeadroom{Total: 12 * gib, Available: 11 * gib}, nil })
	warm := models.Info{ID: "warm-fallback", Kind: models.KindOllama, BaseURL: srv.URL}
	r.owned[resourceKey(warm)] = warm

	release, err := r.acquire(context.Background(), models.Info{ID: "too-large", Kind: models.KindOllama, BaseURL: srv.URL, PeakMemoryBytes: 16 * gib}, srv.Client(), slog.Default())
	if err == nil {
		release.close()
		t.Fatal("a model larger than the machine was admitted")
	}
	var refused *memoryRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("a model that cannot fit should be a memory refusal, got %T: %v", err, err)
	}
	if n := unloads.Load(); n != 0 {
		t.Fatalf("%d idle model(s) were unloaded for a call that was going to be refused", n)
	}
	if _, kept := r.owned[resourceKey(warm)]; !kept {
		t.Fatal("the warm model stopped being owned")
	}
}

// A call waiting for memory holds this machine's only inference gate, so it
// must give up long before its own deadline and say why. Waiting out the call's
// deadline stalled a scheduled brief for ten minutes on a machine other machines
// could have served, with nothing else able to run on it meanwhile.
func TestResourcesStopWaitingForHeadroomAndReleaseTheGate(t *testing.T) {
	var available atomic.Uint64
	available.Store(10 * gib)
	r := resourcesForTest(func() (memoryHeadroom, error) {
		return memoryHeadroom{Total: 64 * gib, Available: available.Load()}, nil
	})
	r.headroomWait = 20 * time.Millisecond
	info := models.Info{ID: "any-modality", PeakMemoryBytes: 16 * gib}

	// The deadline is far away: only the wait's own ceiling can end this.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	began := time.Now()
	release, err := r.acquire(ctx, info, http.DefaultClient, slog.Default())
	if err == nil {
		release.close()
		t.Fatal("overcommitted memory")
	}
	if waited := time.Since(began); waited > 10*time.Second {
		t.Fatalf("waited %s for headroom with a 20ms ceiling", waited)
	}
	var refused *memoryRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("giving up on headroom should be a memory refusal, got %T: %v", err, err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("the call's deadline was not what ended this: %v", err)
	}

	// The gate came back, so a call that fits runs at once.
	available.Store(24 * gib)
	release, err = r.acquire(context.Background(), info, http.DefaultClient, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release.close()
}

// Memory that arrives during the wait is still taken: the ceiling bounds the
// wait, it does not replace it.
func TestResourcesAdmitWhenHeadroomArrivesDuringTheWait(t *testing.T) {
	var probes atomic.Int32
	r := resourcesForTest(func() (memoryHeadroom, error) {
		if probes.Add(1) < 5 {
			return memoryHeadroom{Total: 64 * gib, Available: 10 * gib}, nil
		}
		return memoryHeadroom{Total: 64 * gib, Available: 24 * gib}, nil
	})
	r.headroomWait = time.Minute
	release, err := r.acquire(context.Background(), models.Info{ID: "any-modality", PeakMemoryBytes: 16 * gib}, http.DefaultClient, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	release.close()
}
