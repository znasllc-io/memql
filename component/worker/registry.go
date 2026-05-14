package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

// ErrNoWorkerAvailable indicates dispatch found no online worker
// matching the caller's requirements.
var ErrNoWorkerAvailable = errors.New("worker: no worker available for owner")

// ErrWorkerBusy indicates the worker is at its concurrency cap and
// the FIFO queue declined further calls within the timeout window.
var ErrWorkerBusy = errors.New("worker: worker busy at concurrency cap")

// ErrWorkerDisconnected indicates the worker's gRPC stream went away
// before the dispatch completed.
var ErrWorkerDisconnected = errors.New("worker: worker disconnected mid-call")

// Registry is the in-memory map of currently-connected workers
// indexed by owner user id. The registry doesn't own the gRPC
// streams (server.go does); it holds back-pointers + concurrency
// state that lives only as long as the connection.
type Registry struct {
	logger *slog.Logger
	clock  func() time.Time

	mu       sync.RWMutex
	byOwner  map[string][]*Worker
	byId     map[string]*Worker
	draining bool
}

// Worker is a registry-side handle to a connected worker. The
// referenced gRPC stream is owned by the per-stream session in
// server.go; the registry never reads/writes the stream directly.
type Worker struct {
	registry *Registry

	RegistrationId string
	OwnerUserId    string
	IdentityId     string
	Name           string
	Capabilities   []string
	Labels         map[string]string
	Concurrency    map[string]uint32
	Platform       map[string]any
	Permissions    map[string]any
	Version        string
	BuildTag       string
	ConnectedAt    time.Time
	LastSeenAt     time.Time
	SourceIP       string

	dispatchFn   DispatchFunc
	cancelStream func()

	mu          sync.Mutex
	activePerCap map[string]uint32
	queue       []chan struct{}
}

// DispatchFunc is the worker-side dispatch hook owned by the
// stream session. The registry calls it when an agent-side request
// has been admission-checked.
type DispatchFunc func(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.ToolResult, error)

// NewRegistry constructs an empty registry.
func NewRegistry(logger *slog.Logger, clock func() time.Time) *Registry {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Registry{
		logger:  logger,
		clock:   clock,
		byOwner: make(map[string][]*Worker),
		byId:    make(map[string]*Worker),
	}
}

// Add inserts a worker into the registry. Replaces any existing
// entry with the same registration id (a reconnect).
func (r *Registry) Add(w *Worker) {
	if r == nil || w == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return
	}
	w.registry = r
	if w.activePerCap == nil {
		w.activePerCap = make(map[string]uint32, len(w.Capabilities))
	}
	if existing, ok := r.byId[w.RegistrationId]; ok {
		r.removeLocked(existing)
	}
	r.byId[w.RegistrationId] = w
	r.byOwner[w.OwnerUserId] = append(r.byOwner[w.OwnerUserId], w)
}

// Remove removes a worker from the registry. Called when its gRPC
// stream closes.
func (r *Registry) Remove(registrationId string) {
	if r == nil || registrationId == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.byId[registrationId]
	if !ok {
		return
	}
	r.removeLocked(w)
}

func (r *Registry) removeLocked(w *Worker) {
	delete(r.byId, w.RegistrationId)
	owners := r.byOwner[w.OwnerUserId]
	out := owners[:0]
	for _, candidate := range owners {
		if candidate == w {
			continue
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		delete(r.byOwner, w.OwnerUserId)
	} else {
		r.byOwner[w.OwnerUserId] = out
	}
	w.cancelQueueLocked()
}

// Drain marks the registry as draining and ends every active stream.
// New workers are refused; in-flight calls finish naturally.
func (r *Registry) Drain() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	for _, w := range r.byId {
		if w.cancelStream != nil {
			w.cancelStream()
		}
	}
}

// WorkersForUser returns every connected worker owned by ownerUserId.
// Caller must not mutate the slice; call Snapshot for a copy.
func (r *Registry) WorkersForUser(ownerUserId string) []*Worker {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	src := r.byOwner[ownerUserId]
	if len(src) == 0 {
		return nil
	}
	out := make([]*Worker, len(src))
	copy(out, src)
	return out
}

// Snapshot returns every worker in the registry sorted by registration ID.
// Used by /admin views that span all owners.
func (r *Registry) Snapshot() []*Worker {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Worker, 0, len(r.byId))
	for _, w := range r.byId {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RegistrationId < out[j].RegistrationId
	})
	return out
}

// PickWorker selects a worker for the given owner that satisfies the
// capability requirement. The simple MVP implementation returns the
// first online matching worker; future capacity-aware routing slots
// in here.
func (r *Registry) PickWorker(ownerUserId string, capability string, labels map[string]string) (*Worker, error) {
	if r == nil {
		return nil, ErrNoWorkerAvailable
	}
	for _, w := range r.WorkersForUser(ownerUserId) {
		if !w.SupportsCapability(capability) {
			continue
		}
		if !w.MatchesLabels(labels) {
			continue
		}
		return w, nil
	}
	return nil, ErrNoWorkerAvailable
}

// SupportsCapability reports whether the worker advertised the
// supplied capability.
func (w *Worker) SupportsCapability(name string) bool {
	if w == nil {
		return false
	}
	if name == "" {
		return true
	}
	for _, cap := range w.Capabilities {
		if cap == name {
			return true
		}
	}
	return false
}

// MatchesLabels reports whether every label in the requirement is
// present on the worker. Empty requirement matches anything.
func (w *Worker) MatchesLabels(req map[string]string) bool {
	if len(req) == 0 {
		return true
	}
	if w == nil || w.Labels == nil {
		return false
	}
	for k, v := range req {
		got, ok := w.Labels[k]
		if !ok || got != v {
			return false
		}
	}
	return true
}

// ConcurrencyCap returns the per-capability cap; 0 means unbounded.
func (w *Worker) ConcurrencyCap(capability string) uint32 {
	if w == nil || w.Concurrency == nil {
		return 0
	}
	return w.Concurrency[capability]
}

// Acquire takes a concurrency slot for the supplied capability. If
// the cap is reached, the call enqueues FIFO and blocks until either
// a slot frees, the context cancels, or the worker disconnects.
func (w *Worker) Acquire(ctx context.Context, capability string) error {
	if w == nil {
		return ErrWorkerDisconnected
	}
	cap := w.ConcurrencyCap(capability)
	w.mu.Lock()
	if cap == 0 || w.activePerCap[capability] < cap {
		w.activePerCap[capability]++
		w.mu.Unlock()
		return nil
	}
	wait := make(chan struct{})
	w.queue = append(w.queue, wait)
	w.mu.Unlock()

	select {
	case <-wait:
		w.mu.Lock()
		w.activePerCap[capability]++
		w.mu.Unlock()
		return nil
	case <-ctx.Done():
		w.removeFromQueue(wait)
		return fmt.Errorf("worker: queue wait cancelled: %w", ctx.Err())
	}
}

// Release frees a concurrency slot taken by Acquire.
func (w *Worker) Release(capability string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if v, ok := w.activePerCap[capability]; ok && v > 0 {
		w.activePerCap[capability] = v - 1
	}
	if len(w.queue) > 0 {
		next := w.queue[0]
		w.queue = w.queue[1:]
		close(next)
	}
	w.mu.Unlock()
}

// Dispatch sends a ToolDispatch envelope to the worker and waits for
// the matching ToolResult. The acquired concurrency slot is released
// when this method returns.
func (w *Worker) Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.ToolResult, error) {
	if w == nil || w.dispatchFn == nil {
		return nil, ErrWorkerDisconnected
	}
	return w.dispatchFn(ctx, dispatch)
}

// SetDispatchFunc wires the per-stream dispatch hook. Called once
// per stream when the session admits a successful Register message.
func (w *Worker) SetDispatchFunc(fn DispatchFunc, cancel func()) {
	if w == nil {
		return
	}
	w.dispatchFn = fn
	w.cancelStream = cancel
}

// TouchLastSeen bumps the in-memory last-seen timestamp.
// Persistence happens on the registry's batched flush cadence.
func (w *Worker) TouchLastSeen(at time.Time, sourceIP string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.LastSeenAt = at
	if sourceIP != "" {
		w.SourceIP = sourceIP
	}
	w.mu.Unlock()
}

func (w *Worker) removeFromQueue(target chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, ch := range w.queue {
		if ch == target {
			w.queue = append(w.queue[:i], w.queue[i+1:]...)
			return
		}
	}
}

func (w *Worker) cancelQueueLocked() {
	for _, ch := range w.queue {
		close(ch)
	}
	w.queue = nil
}
