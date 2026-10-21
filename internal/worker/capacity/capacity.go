// Package capacity reports what this machine can take on right now: the
// memory a new model could load into, and the models the runtime already
// holds. The engine admits a model call to a machine only when the model is
// loaded or there is room to load it (engine design record 2026-10-18-routes,
// "Fleet capacity"), so a call is never placed where it would push the
// runtime into swapping.
//
// A report is sent on every heartbeat. When this machine cannot measure --
// an operating system this package does not probe, a runtime that does not
// answer -- it sends none, and the engine admits it on its concurrency slots
// alone, which is how every machine was admitted before. A report that is
// wrong is worse than no report: a machine that said it held no model would
// be refused calls it could serve. So nothing here guesses.
package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The pool kinds the engine understands.
const (
	KindUnified = "unified"
	KindVRAM    = "vram"
	KindSystem  = "system"
)

// probeTimeout bounds one probe. The heartbeat waits for it, and a beat that
// waits too long is a machine that reads offline.
const probeTimeout = 2 * time.Second

// Report is one measurement.
type Report struct {
	MemoryTotalBytes uint64
	MemoryFreeBytes  uint64
	MemoryKind       string
	Loaded           []Loaded
	MeasuredAt       time.Time
}

// Loaded is one model resident in the runtime.
type Loaded struct {
	Model       string
	Runtime     string
	SizeBytes   uint64
	ExpiresAt   time.Time
	ActiveCalls int
}

// Memory is what the platform probe found.
type Memory struct {
	Kind  string
	Total uint64
	// Available is what the operating system (or the card) can hand out now.
	Available uint64
	// ModelLimit is the most the GPU may hold on unified memory -- macOS
	// wires at most a share of the pool for the GPU -- zero when it does
	// not apply.
	ModelLimit uint64
}

// Prober takes a measurement.
type Prober struct {
	// OllamaBase is where the runtime listens.
	OllamaBase func() string
	// InFlight is the calls running per model id, across every cluster this
	// machine serves: they share one GPU.
	InFlight func() map[string]int
	// Memory and HTTPClient default to the platform probe and a plain client.
	Memory     func(ctx context.Context) (Memory, bool)
	HTTPClient *http.Client
	Now        func() time.Time
}

// Probe measures, or reports false when there is nothing it can vouch for.
func (p *Prober) Probe(ctx context.Context) (Report, bool) {
	if p == nil || p.OllamaBase == nil {
		return Report{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	memory := p.Memory
	if memory == nil {
		memory = probeMemory
	}
	mem, ok := memory(ctx)
	if !ok || mem.Total == 0 || !validKind(mem.Kind) {
		return Report{}, false
	}
	// Without the runtime's own list there is no telling which models are
	// loaded, and a report claiming none would refuse calls this machine
	// holds the model for. So no list, no report.
	loaded, err := p.ollamaLoaded(ctx, mem.Kind)
	if err != nil {
		return Report{}, false
	}
	inFlight := map[string]int{}
	if p.InFlight != nil {
		for id, n := range p.InFlight() {
			inFlight[normalize(id)] += n
		}
	}
	var held uint64
	for i := range loaded {
		loaded[i].ActiveCalls = inFlight[normalize(loaded[i].Model)]
		held += loaded[i].SizeBytes
	}
	free := mem.Available
	if mem.ModelLimit > 0 {
		room := uint64(0)
		if mem.ModelLimit > held {
			room = mem.ModelLimit - held
		}
		free = min(free, room)
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	return Report{
		MemoryTotalBytes: mem.Total,
		MemoryFreeBytes:  min(free, mem.Total),
		MemoryKind:       mem.Kind,
		Loaded:           loaded,
		MeasuredAt:       now().UTC(),
	}, true
}

// psResponse is the shape of Ollama's GET /api/ps.
type psResponse struct {
	Models []struct {
		Name      string    `json:"name"`
		Model     string    `json:"model"`
		Size      uint64    `json:"size"`
		SizeVRAM  uint64    `json:"size_vram"`
		ExpiresAt time.Time `json:"expires_at"`
	} `json:"models"`
}

func (p *Prober) ollamaLoaded(ctx context.Context, kind string) ([]Loaded, error) {
	base := strings.TrimRight(strings.TrimSpace(p.OllamaBase()), "/")
	if base == "" {
		return nil, fmt.Errorf("no runtime address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtime answered %s", resp.Status)
	}
	var ps psResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ps); err != nil {
		return nil, err
	}
	out := make([]Loaded, 0, len(ps.Models))
	for _, m := range ps.Models {
		// The same id rule the model inventory advertises by, so the
		// engine can match a loaded model to `model:<id>`.
		id := strings.TrimSpace(m.Model)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		size := m.Size
		if kind == KindVRAM && m.SizeVRAM > 0 {
			size = m.SizeVRAM
		}
		out = append(out, Loaded{Model: id, Runtime: "ollama", SizeBytes: size, ExpiresAt: m.ExpiresAt})
	}
	return out, nil
}

func validKind(kind string) bool {
	switch kind {
	case KindUnified, KindVRAM, KindSystem:
		return true
	}
	return false
}

func normalize(id string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(id)), ":latest")
}

// ToProto renders a report for the wire; nil when there is none.
func ToProto(r *Report) *memqlv1.Capacity {
	if r == nil || r.MemoryTotalBytes == 0 {
		return nil
	}
	out := &memqlv1.Capacity{
		MemoryTotalBytes: r.MemoryTotalBytes,
		MemoryFreeBytes:  r.MemoryFreeBytes,
		MemoryKind:       r.MemoryKind,
	}
	if !r.MeasuredAt.IsZero() {
		out.MeasuredAt = timestamp(r.MeasuredAt)
	}
	for _, m := range r.Loaded {
		entry := &memqlv1.LoadedModel{Model: m.Model, Runtime: m.Runtime, SizeBytes: m.SizeBytes, ActiveCalls: uint32(max(m.ActiveCalls, 0))}
		if !m.ExpiresAt.IsZero() {
			entry.ExpiresAt = timestamp(m.ExpiresAt)
		}
		out.LoadedModels = append(out.LoadedModels, entry)
	}
	return out
}
