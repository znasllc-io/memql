package modelcall

import (
	"context"
	"encoding/json"
	"fmt"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
)

const gib = uint64(1 << 30)

type memoryHeadroom struct {
	Total, Available uint64
	Pressure         bool
}

// hostResources coordinates local runtimes, not model names or modalities.
// Installing another model does not install another GPU. In particular an
// idle Ollama runner and an on-demand MLX runner share the same unified pool.
// The gate is shared by every cluster Manager and is held through cleanup.
type hostResources struct {
	gate  chan struct{}
	probe func() (memoryHeadroom, error)
	// Only models this worker actually invoked may be evicted. Unrelated
	// applications' runners must not be stopped to make room for Fleet.
	owned     map[string]models.Info
	poll      time.Duration
	leaseRoot func() (string, error)
	// headroomWait is how long a call waits for memory; zero is
	// defaultHeadroomWait.
	headroomWait time.Duration
}

// NewHostLimiter is the production limiter. NewLimiter remains the pure
// advertised concurrency limiter for embedders that supply their own budget.
func NewHostLimiter() *Limiter {
	l := NewLimiter()
	l.resources = &hostResources{gate: make(chan struct{}, 1), probe: readMemoryHeadroom, owned: make(map[string]models.Info), poll: time.Second, leaseRoot: inferenceCapacityRoot}
	return l
}

func localRuntime(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func resourceKey(info models.Info) string {
	return strings.TrimRight(info.BaseURL, "/") + "\x00" + info.ID
}

// defaultHeadroomWait bounds a call's wait for memory, well inside its own
// deadline. Everything this worker can release is gone by the time a call waits
// (acquire reclaims its idle models first), so what is left is a runner still
// freeing its memory, which takes seconds, or another application, which may
// never leave. The wait holds this machine's only gate, so nothing else can run
// here meanwhile, and another machine can usually serve the call now: the engine
// moves on when this one declines (memoryRefusal).
const defaultHeadroomWait = 30 * time.Second

// memoryRefusal is this machine declining a call for memory. Nothing has reached
// the runtime when it is returned, which is what lets the engine offer the call
// to another machine (CodeInsufficientMemory).
type memoryRefusal struct{ detail string }

func (e *memoryRefusal) Error() string { return e.detail }

// workingSet is a conservative admission estimate, never reported as usage.
// Declared peaks include temporary buffers. Ollama's actual file size gets an
// eighth of itself, and at least 2 GiB, of context/compute headroom: the figure
// that reproduces the catalog's residency at a 32K working context
// (dsl/models/seeds.memql), which is what the engine admits on and what a machine
// class is chosen by. A larger allowance refuses the catalog's own pick on the
// class it is picked for: 25% plus 2 GiB put the 24 GB class's 27B past every
// 24 GB card, permanently. Runtime resident bytes can raise this estimate.
// Unknown runtimes still get exclusive admission and pressure protection, but
// must declare a peak to protect a cold load in advance.
func workingSet(info models.Info) uint64 {
	if info.PeakMemoryBytes != 0 {
		return info.PeakMemoryBytes
	}
	if info.ModelBytes == 0 {
		return 0
	}
	headroom := max(info.ModelBytes/8, 2*gib)
	if info.ModelBytes > math.MaxUint64-headroom {
		return math.MaxUint64
	}
	return info.ModelBytes + headroom
}

// memoryReserve is what admission leaves free on a pool of this size.
func memoryReserve(total uint64) uint64 {
	return min(4*gib, max(gib, total/16))
}

func saturatedMultiply(a, b uint64) uint64 {
	if b != 0 && a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}

// Request shape changes peak memory independently of model size. Scale up
// declared image peaks by pixel area/batch, never down for smaller requests.
// Ollama gets extra context headroom above the baseline working window.
func requestMemory(info models.Info, start *memqlv1.ModelCallStart) models.Info {
	peak := workingSet(info)
	if start.GetKind() == KindImage && info.MemoryReferencePixels > 0 {
		payload, _ := payloadFor(start)
		pixels := info.MemoryReferencePixels
		if payload.Image.Width > 0 && payload.Image.Height > 0 {
			pixels = saturatedMultiply(uint64(payload.Image.Width), uint64(payload.Image.Height))
		}
		factor := pixels / info.MemoryReferencePixels
		if pixels%info.MemoryReferencePixels != 0 {
			factor++
		}
		peak = saturatedMultiply(peak, max(uint64(1), factor))
		peak = saturatedMultiply(peak, uint64(max(1, payload.Image.Count)))
	} else if info.Kind == models.KindOllama && info.PeakMemoryBytes == 0 {
		contextTokens := start.GetParams().GetContextTokens()
		if contextTokens > 32768 {
			extra := saturatedMultiply(uint64((contextTokens-1)/32768), 2*gib)
			if extra > math.MaxUint64-peak {
				peak = math.MaxUint64
			} else {
				peak += extra
			}
		}
	}
	info.PeakMemoryBytes = peak
	return info
}

func (r *hostResources) acquire(ctx context.Context, info models.Info, client *http.Client, logger *slog.Logger) (func(), error) {
	// WHAT CAN NEVER FIT IS REFUSED FIRST: before it queues behind the gate, and
	// before the reclaim below unloads an idle model for nothing. That idle model
	// can be the one the caller's fallback is about to need.
	pool, err := r.probe()
	if err != nil {
		return nil, fmt.Errorf("cannot establish inference memory headroom: %w", err)
	}
	if peak := workingSet(info); peak > pool.Total || memoryReserve(pool.Total) > pool.Total-peak {
		return nil, &memoryRefusal{detail: fmt.Sprintf("model %q needs more memory than this machine can safely offer", info.ID)}
	}
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var lease *inferenceLease
	release := func() {
		if lease != nil {
			lease.close()
		}
		<-r.gate
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	if r.leaseRoot != nil {
		root, err := r.leaseRoot()
		if err != nil {
			return nil, err
		}
		lease, err = acquireInferenceLease(ctx, root, r.poll)
		if err != nil {
			return nil, err
		}
		r.owned, err = lease.readOwned()
		if err != nil {
			return nil, err
		}
	}
	save := func() error {
		if lease != nil {
			return lease.saveOwned(r.owned)
		}
		return nil
	}
	key := resourceKey(info)
	// Reclaim idle models before examining free memory, including transitions
	// between text, embeddings, audio and image runtimes. Same-model calls keep
	// their warm runner and prompt cache.
	for oldKey, old := range r.owned {
		if oldKey == key || old.Kind != models.KindOllama {
			continue
		}
		c := &ollamaClient{baseURL: old.BaseURL, http: client}
		resp, err := c.post(ctx, "/api/generate", map[string]any{"model": old.ID, "keep_alive": 0, "stream": false})
		if err != nil {
			return nil, fmt.Errorf("reclaim idle model %q: %w", old.ID, err)
		}
		var result struct {
			Error string `json:"error"`
			Done  bool   `json:"done"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
		resp.Body.Close()
		if err != nil || result.Error != "" || !result.Done {
			return nil, fmt.Errorf("idle model %q did not confirm unloading", old.ID)
		}
		delete(r.owned, oldKey)
		if err := save(); err != nil {
			return nil, err
		}
		logger.Info("released idle inference model", "model", old.ID)
	}
	wanted := workingSet(info)
	var last string
	wait := r.headroomWait
	if wait <= 0 {
		wait = defaultHeadroomWait
	}
	ceiling := time.NewTimer(wait)
	defer ceiling.Stop()
	for {
		state, err := r.probe()
		if err != nil {
			return nil, fmt.Errorf("cannot establish inference memory headroom: %w", err)
		}
		reserve := memoryReserve(state.Total)
		resident := uint64(0)
		if info.Kind == models.KindOllama {
			resident, err = ollamaResident(ctx, client, info)
			if err != nil {
				return nil, err
			}
		}
		additional := wanted - min(wanted, resident)
		if !state.Pressure && state.Available >= reserve && state.Available-reserve >= additional {
			r.owned[key] = info
			if err := save(); err != nil {
				return nil, err
			}
			logger.Info("admitted local inference", "model", info.ID, "available_bytes", state.Available, "reserved_bytes", reserve, "estimated_peak_bytes", wanted, "resident_bytes", resident)
			ok = true
			return release, nil
		}
		headroom := fmt.Sprintf("available %d, additional %d, reserve %d, pressure %t", state.Available, additional, reserve, state.Pressure)
		if last == "" {
			last = fmt.Sprintf("model %q is waiting for memory headroom (%s)", info.ID, headroom)
			logger.Info(last)
		}
		timer := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%s: %w", last, ctx.Err())
		case <-ceiling.C:
			timer.Stop()
			return nil, &memoryRefusal{detail: fmt.Sprintf("model %q found no memory headroom in %s (%s)", info.ID, wait, headroom)}
		case <-timer.C:
		}
	}
}

func ollamaResident(ctx context.Context, client *http.Client, info models.Info) (uint64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(info.BaseURL, "/")+"/api/ps", nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read resident inference models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read resident inference models: HTTP %d", resp.StatusCode)
	}
	var running struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
			Size  uint64 `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&running); err != nil {
		return 0, fmt.Errorf("read resident inference models: %w", err)
	}
	for _, m := range running.Models {
		if m.Name == info.ID || m.Model == info.ID {
			return m.Size, nil
		}
	}
	return 0, nil
}
