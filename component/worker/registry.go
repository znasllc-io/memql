package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

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
	// CapabilityDescriptor is the optional structured capability
	// self-description sent at registration (memql#1330). Nil when
	// the worker didn't send one.
	CapabilityDescriptor *CapabilityDescriptor
	Labels               map[string]string
	Concurrency          map[string]uint32
	Platform             map[string]any
	Permissions          map[string]any
	Version              string
	BuildTag             string
	ConnectedAt          time.Time
	LastSeenAt           time.Time
	SourceIP             string

	dispatchFn   DispatchFunc
	appSessionFn AppSessionFunc
	modelCallFn  ModelCallFunc
	cancelStream func()

	mu           sync.Mutex
	activePerCap map[string]uint32
	queue        []chan struct{}
	// apps is the reported local-app inventory. Guarded by mu because
	// heartbeats rewrite it while selection reads it.
	apps []AppInfo
}

// DispatchFunc is the worker-side dispatch hook owned by the
// stream session. The registry calls it when an agent-side request
// has been admission-checked.
//
// onChunk may be nil. When it is not, the session invokes it for every
// ToolStream the worker emits for this call, in arrival order and before the
// result returns.
type DispatchFunc func(ctx context.Context, dispatch *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error)

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

// WorkerById returns the live handle for one registration, or nil when this
// replica does not hold its stream. O(1) over the byId index the registry
// already maintains -- a router turning a chosen registration id back into a
// stream handle should not walk WorkersForUser to re-derive an index that
// exists.
//
// Returns nil while draining, for the same reason Add refuses during a drain:
// the streams are being cancelled and a handle taken now is one whose dispatch
// is about to fail.
func (r *Registry) WorkerById(registrationId string) *Worker {
	if r == nil || registrationId == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.draining {
		return nil
	}
	return r.byId[registrationId]
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

// SetApps replaces the worker's app inventory and re-derives its
// `app:` routing labels (memql#4359). Called on register and on every
// heartbeat that carries an inventory, so signing into -- or out of --
// an app changes what the router can select within one beat rather than
// at the next reconnect.
//
// The derived labels go into Labels, the COCKPIT's side of the label
// pair, not OperatorLabels: the engine derives them from what the
// machine reported, and the owner does not set them. MergeLabels then
// lets an operator label win, which is the right precedence -- an owner
// pinning a machine should out-rank a derived hint.
func (w *Worker) SetApps(apps []AppInfo) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.apps = apps
	w.Labels = mergeAppLabels(w.Labels, apps)
}

// Apps returns a copy of the worker's app inventory.
func (w *Worker) Apps() []AppInfo {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.apps) == 0 {
		return nil
	}
	out := make([]AppInfo, len(w.apps))
	copy(out, w.apps)
	return out
}

// LabelsSnapshot returns a copy of the worker's current labels,
// including the derived `app:` ones.
func (w *Worker) LabelsSnapshot() map[string]string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.Labels) == 0 {
		return nil
	}
	out := make(map[string]string, len(w.Labels))
	for k, v := range w.Labels {
		out[k] = v
	}
	return out
}

// App returns the reported entry for appId and whether it was found.
func (w *Worker) App(appId string) (AppInfo, bool) {
	for _, a := range w.Apps() {
		if a.Id == appId {
			return a, true
		}
	}
	return AppInfo{}, false
}

// RunsApp reports whether this worker can actually run appId: the id is
// one the engine drives, the machine allows it, and the app is signed
// in. The same test the label derivation applies, asked of one worker --
// the two must agree, or the router picks a machine that then refuses.
func (w *Worker) RunsApp(appId string) bool {
	a, ok := w.App(appId)
	return ok && a.Runnable()
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

// ActiveCount is this replica's view of how many calls are in flight on the
// worker: the sum of the per-capability slots Acquire has taken and Release
// has not yet given back.
//
// It is the SERVER's count, not the worker's. The heartbeat carries the
// worker's own (Heartbeat.active_calls_total) and that is what the persisted
// activeCount means; this is the fallback for a cockpit build that predates
// the field, and the two can legitimately differ by whatever is in flight
// between the dispatch and the beat.
func (w *Worker) ActiveCount() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for _, n := range w.activePerCap {
		total += int(n)
	}
	return total
}

// Dispatch sends a ToolDispatch envelope to the worker and waits for
// the matching ToolResult. The acquired concurrency slot is released
// when this method returns.
//
// Output chunks the worker emits along the way are dropped. Use
// DispatchWithStream to receive them.
func (w *Worker) Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.ToolResult, error) {
	return w.DispatchWithStream(ctx, dispatch, nil)
}

// DispatchWithStream is Dispatch plus a per-chunk callback invoked for every
// ToolStream the worker emits for this call, in arrival order. onChunk may be
// nil, in which case this is exactly Dispatch.
//
// The callback runs on the stream-recv goroutine, so it must not block: while
// it runs, nothing else on that worker's connection is read -- not another
// call's chunks, not any result, not the heartbeat. A forwarder should hand
// the chunk to a buffered channel and return.
//
// Every chunk delivered here precedes the returned ToolResult. A chunk the
// worker emits AFTER its own result is dropped rather than delivered late,
// because by then the caller has its answer.
func (w *Worker) DispatchWithStream(ctx context.Context, dispatch *memqlv1.ToolDispatch, onChunk func(*memqlv1.ToolStream)) (*memqlv1.ToolResult, error) {
	if w == nil || w.dispatchFn == nil {
		return nil, ErrWorkerDisconnected
	}
	return w.dispatchFn(ctx, dispatch, onChunk)
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
