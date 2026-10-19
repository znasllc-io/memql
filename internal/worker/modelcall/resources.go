package modelcall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
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
	owned map[string]models.Info
	poll  time.Duration
}

// NewHostLimiter is the production limiter. NewLimiter remains the pure
// advertised concurrency limiter for embedders that supply their own budget.
func NewHostLimiter() *Limiter {
	l := NewLimiter()
	l.resources = &hostResources{gate: make(chan struct{}, 1), probe: readMemoryHeadroom, owned: make(map[string]models.Info), poll: time.Second}
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

// workingSet is a conservative admission estimate, never reported as usage.
// Declared peaks include temporary buffers. Ollama's actual file size gets
// 25% plus 2 GiB of context/compute headroom; runtime resident bytes can raise
// this estimate. Unknown runtimes still get exclusive admission and pressure
// protection, but must declare a peak to protect a cold load in advance.
func workingSet(info models.Info) uint64 {
	if info.PeakMemoryBytes != 0 {
		return info.PeakMemoryBytes
	}
	if info.ModelBytes == 0 {
		return 0
	}
	if info.ModelBytes > (math.MaxUint64-2*gib)/5*4 {
		return math.MaxUint64
	}
	return info.ModelBytes + info.ModelBytes/4 + 2*gib
}

func (r *hostResources) acquire(ctx context.Context, info models.Info, client *http.Client, logger *slog.Logger) (func(), error) {
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-r.gate }
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
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
		logger.Info("released idle inference model", "model", old.ID)
	}
	wanted := workingSet(info)
	var last string
	for {
		state, err := r.probe()
		if err != nil {
			return nil, fmt.Errorf("cannot establish inference memory headroom: %w", err)
		}
		reserve := min(4*gib, max(gib, state.Total/16))
		if wanted > state.Total || reserve > state.Total-wanted {
			return nil, fmt.Errorf("model %q needs more memory than this machine can safely offer", info.ID)
		}
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
			logger.Info("admitted local inference", "model", info.ID, "available_bytes", state.Available, "reserved_bytes", reserve, "estimated_peak_bytes", wanted, "resident_bytes", resident)
			ok = true
			return release, nil
		}
		if last == "" {
			last = fmt.Sprintf("model %q is waiting for memory headroom (available %d, additional %d, reserve %d, pressure %t)", info.ID, state.Available, additional, reserve, state.Pressure)
			logger.Info(last)
		}
		timer := time.NewTimer(r.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("%s: %w", last, ctx.Err())
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
