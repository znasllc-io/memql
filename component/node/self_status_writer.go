package node

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
)

// SelfStatusWriter refreshes this process's persisted liveness independently
// of mesh gossip and health transitions. The slow stale-node sweep needs a
// real heartbeat even when a healthy process never changes state.
type SelfStatusWriter struct {
	*component.Component
	writer                    *NodeStatusWriter
	lifecycle                 *NodeLifecycle
	nodeID, nodeType, address string
	interval                  time.Duration
	now                       func() time.Time
	writeMu                   sync.Mutex
	stateMu                   sync.Mutex
	meshReport                func() MeshReport
	activeCancel              context.CancelFunc
	stopped                   bool
	stop                      chan struct{}
	terminalDone              chan struct{}
}

// NewSelfStatusWriter is installed once by every node's app lifecycle wiring.
func NewSelfStatusWriter(self *Identity, lifecycle *NodeLifecycle, engine *memqlengine.MemQLEngine, logger *slog.Logger) *SelfStatusWriter {
	if engine == nil {
		return nil
	}
	return newSelfStatusWriter(self, lifecycle, &engineExecutorAdapter{engine: engine}, logger)
}

func newSelfStatusWriter(self *Identity, lifecycle *NodeLifecycle, engine EngineExecutor, logger *slog.Logger) *SelfStatusWriter {
	if self == nil || lifecycle == nil || engine == nil {
		return nil
	}
	comp, _ := component.New(common.ComponentName("nodeSelfStatusWriter"))
	if logger == nil {
		logger = slog.Default()
	}
	w := &SelfStatusWriter{Component: comp, writer: NewNodeStatusWriter(engine, nil, logger), lifecycle: lifecycle, nodeID: self.ID, nodeType: string(self.Type), address: self.Address, interval: time.Minute, now: time.Now, stop: make(chan struct{}), terminalDone: make(chan struct{})}
	_ = w.ConfigureLifecycle(component.WithRunHook(w.run))
	return w
}

func (*SelfStatusWriter) Order() int { return 94 }

// SetMeshReporter gives the writer this node's delivery report (memql#5338,
// D7): every refresh then carries it onto the node's own v1:cluster:node row
// as its `mesh` object, which is what MemQL OS's Cluster > Mesh reads. No new
// rows and no new writes -- the report rides the heartbeat that already
// writes the row once a minute.
func (w *SelfStatusWriter) SetMeshReporter(fn func() MeshReport) {
	if w == nil {
		return
	}
	w.stateMu.Lock()
	w.meshReport = fn
	w.stateMu.Unlock()
}

func (w *SelfStatusWriter) run(ctx context.Context, markStarted func()) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	return w.runWithTicks(ctx, markStarted, ticker.C)
}

func (w *SelfStatusWriter) runWithTicks(ctx context.Context, markStarted func(), ticks <-chan time.Time) error {
	markStarted()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-w.stop:
			return nil
		case <-ticks:
			if err := w.refresh(ctx); err != nil && ctx.Err() == nil {
				w.writer.logger.Warn("self liveness refresh failed", "node_id", w.nodeID, "error", err)
			}
		}
	}
}

func (w *SelfStatusWriter) refresh(ctx context.Context) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.persistLocked(ctx, false)
}

// SyncLifecycle is called by the existing lifecycle observer. It cancels and
// joins any periodic write before persisting the new state. app.Run invokes
// MarkNodeStopped before closing database pools, making this synchronous
// callback the terminal-write barrier rather than a late dependency stop hook.
func (w *SelfStatusWriter) SyncLifecycle() {
	if w == nil {
		return
	}
	terminal := w.lifecycle.State() == LifecycleStopped
	w.stateMu.Lock()
	if w.stopped {
		done := w.terminalDone
		w.stateMu.Unlock()
		<-done
		return
	}
	if terminal {
		w.stopped = true
		close(w.stop)
		defer close(w.terminalDone)
	}
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.stateMu.Unlock()
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := w.persistLocked(context.Background(), terminal); err != nil {
		w.writer.logger.Warn("self lifecycle persistence failed", "node_id", w.nodeID, "error", err)
	}
}

// Caller holds writeMu. Cancellation remains accessible through stateMu while
// an engine call is blocked, so a terminal callback can cancel before joining.
func (w *SelfStatusWriter) persistLocked(parent context.Context, terminal bool) error {
	if err := parent.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	w.stateMu.Lock()
	if w.stopped && !terminal {
		w.stateMu.Unlock()
		return nil
	}
	w.activeCancel = cancel
	report := w.meshReport
	w.stateMu.Unlock()
	defer func() { w.stateMu.Lock(); w.activeCancel = nil; w.stateMu.Unlock() }()
	// Observers run outside the lifecycle lock and may reach us out of order.
	// Always sample the actual current state after entering write serialization.
	health := HealthLabel(w.lifecycle.Health())
	at := w.now().UTC().Format(time.RFC3339)
	var mesh map[string]any
	if report != nil {
		mesh = report().Wire()
	}
	query, err := buildUpdateNodeHealthCall(w.nodeID, w.nodeType, w.address, health, at, mesh)
	if err != nil {
		return err
	}
	ctx = auth.ContextWithUserActor(ctx, "system:node-self-status-writer")
	return w.writer.persistHealthTransition(ctx, query, w.nodeID, w.nodeType, w.address, health, at)
}
