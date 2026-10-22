package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/capacity"
	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// Re-advertising a changed model set (memql-cockpit#361).
//
// The engine accepts Register EXACTLY ONCE, at the handshake -- its
// stream dispatcher has no case for a second one -- and Heartbeat carries
// apps but no labels. So a model pulled, removed, or newly allowed while
// this worker is connected is invisible to the cluster until the worker
// registers again, and registering again means reconnecting.
//
// That makes re-advertisement expensive in a way the app inventory is
// not, and the two constants below are what keep it from being reckless:
// the set is re-checked often enough that `ollama pull` is noticed within
// a minute, and a reconnect is spent only when the ADVERTISED labels
// actually changed, only when no work is in flight, and never twice in
// quick succession -- a runtime flapping up and down must not turn into a
// worker that reconnects forever.
const (
	// modelRefreshInterval is how often the offered set is re-checked.
	modelRefreshInterval = 60 * time.Second
	// modelReadvertiseMinInterval floors the gap between two
	// model-triggered reconnects. RequestImmediateReadvertise is its one
	// exception for a changed model set: see that method for why a pull
	// somebody is watching does not wait it out.
	modelReadvertiseMinInterval = 2 * time.Minute
)

// Runner owns the worker's main loop: reconnect-with-backoff, the
// inbound-message dispatcher, the heartbeat ticker, and the
// in-process tool execution.
type Runner struct {
	logger    *slog.Logger
	cfg       Config
	home      string
	tools     ToolDispatcher
	apps      AppInventory
	modelsInv ModelInventory
	capacity  CapacityProbe
	setup     SetupSource
	calls     *modelcall.Manager
	sessions  *appsession.Manager
	pulls     *modelPuller
	heartbeat time.Duration
	pipelines func() tools.PipelinesPolicy
	metrics   *Metrics

	// toolCalls is every tool call in flight, so a ToolCancel -- or the
	// loss of the stream the call arrived on -- can end it (memql#5494).
	toolCalls toolCallSet

	conn            atomic.Pointer[Connection]
	active          sync.WaitGroup
	activeCalls     atomic.Int64
	lastReadvertise atomic.Int64

	// pendingReadvertise is the one-shot floor bypass armed by
	// RequestImmediateReadvertise, and readvertiseNow is how that
	// request wakes the heartbeat loop instead of waiting up to
	// modelRefreshInterval for the next tick. Buffered with room for
	// ONE: a burst of pulls is one evaluation, not one reconnect each.
	pendingReadvertise atomic.Bool
	readvertiseNow     chan struct{}
	// beatNow asks the heartbeat loop for a beat ahead of its tick: the
	// installer's report changed a step (RequestBeat). Room for one.
	beatNow chan struct{}

	// refused is the runner's standing with a cluster that has refused it
	// (refusal.go). Touched only by the goroutine in Run.
	refused refusalHold

	// now is the clock the re-advertise guards read. Injectable because
	// the floor is two minutes of WALL clock, and a test that waited it
	// out would spend two minutes of every CI run.
	now func() time.Time

	// The seams below are what let Run be driven by a test with no
	// cluster: every one defaults to the production behaviour in
	// NewRunner, and a test sets the ones it needs on the Runner it
	// built. They are fields rather than Options because nothing outside
	// this package has a reason to set them.
	//
	// dial opens the stream (dialSDK); the handshake and everything after
	// it run for real against whatever it returns.
	dial func(ctx context.Context, cfg Config) (stream, error)
	// scanHardware reads this machine's inventory for Register and every
	// tenth beat (hardware.Local, which shells out).
	scanHardware func(ctx context.Context) hardware.Inventory
	// backoffMin / backoffMax bound the reconnect backoff against a
	// cluster that is unreachable; holdMin / holdMax bound the hold
	// against one that refused this machine.
	backoffMin, backoffMax time.Duration
	holdMin, holdMax       time.Duration
	// pause waits between attempts (sleepWithJitter). A test records the
	// waits it is asked for instead, which is how the backoff and the hold
	// are asserted exactly rather than timed.
	pause func(ctx context.Context, d time.Duration) bool

	stopOnce sync.Once
	stop     chan struct{}
	started  atomic.Bool
	closed   chan struct{}
}

// ToolDispatcher resolves a ToolDispatch to either a Success or a
// Failure. The cockpit's tool implementations satisfy this
// interface.
type ToolDispatcher interface {
	Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure)
}

// StreamingToolDispatcher is a ToolDispatcher that can carry a call's
// output to the cluster while the call runs (memql#5494: a pipeline step's
// stdout and stderr). send writes one ToolStream frame; the dispatcher
// returns only once every frame of the call is sent, because the engine
// drops a chunk that arrives after the call's result. The runner uses this
// in preference to Dispatch when the dispatcher has it.
type StreamingToolDispatcher interface {
	DispatchStream(ctx context.Context, dispatch *memqlv1.ToolDispatch, send func(*memqlv1.ToolStream) error) (*memqlv1.Success, *memqlv1.Failure)
}

// toolCallSet is the set of tool calls in flight on one runner, each with the
// cancel of its context.
type toolCallSet struct {
	mu    sync.Mutex
	calls map[*toolCall]struct{}
}

type toolCall struct {
	id     string
	cancel context.CancelFunc
}

// add records a call and returns the function that forgets it.
func (s *toolCallSet) add(id string, cancel context.CancelFunc) (forget func()) {
	call := &toolCall{id: id, cancel: cancel}
	s.mu.Lock()
	if s.calls == nil {
		s.calls = make(map[*toolCall]struct{})
	}
	s.calls[call] = struct{}{}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.calls, call)
		s.mu.Unlock()
	}
}

// cancel ends the calls carrying id and reports how many there were.
func (s *toolCallSet) cancel(id string) int {
	if id == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for call := range s.calls {
		if call.id == id {
			call.cancel()
			n++
		}
	}
	return n
}

// cancelAll ends every call in flight and reports how many there were.
func (s *toolCallSet) cancelAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for call := range s.calls {
		call.cancel()
	}
	return len(s.calls)
}

// Options configures NewRunner.
type Options struct {
	Logger    *slog.Logger
	Config    Config
	Tools     ToolDispatcher
	Apps      AppInventory
	Models    ModelInventory
	Calls     *modelcall.Manager
	Sessions  *appsession.Manager
	Heartbeat time.Duration
	Metrics   *Metrics
	// PipelinesPolicy reads the live allow flag and repository list together.
	// A policy reload changes both. Nil grants no pipeline execution.
	PipelinesPolicy func() tools.PipelinesPolicy
	// ModelPull wires the cluster-driven pull (engine epic memql#5103;
	// the install wizard's D13). Nil, or a Models of nil, means this
	// build pulls nothing and answers every ModelPullStart with ok=false
	// and a sentence -- a pull onto a machine that advertises nothing
	// would fetch gigabytes the cluster is never told about.
	ModelPull *ModelPullOptions
	// Capacity measures what the machine can take on now, for every
	// heartbeat. Nil sends no report, and the engine admits the machine on
	// its concurrency slots alone.
	Capacity CapacityProbe
	// Setup is the installer's report for every heartbeat
	// (setupprogress.go). Nil, or a nil report, sends none, and the engine
	// keeps whatever it last stored.
	Setup SetupSource
}

// SetupSource is the installer's latest report, or nil when there is none.
type SetupSource interface {
	Proto() *memqlv1.SetupProgress
}

// CapacityProbe takes one capacity measurement, or reports false when there
// is nothing it can vouch for.
type CapacityProbe interface {
	Probe(ctx context.Context) (capacity.Report, bool)
}

// NewRunner constructs a Runner. The runner is not yet running; call
// Run to start the connect / dispatch loop.
func NewRunner(opts Options) (*Runner, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	hb := opts.Heartbeat
	if hb <= 0 {
		hb = DefaultHeartbeat
	}
	r := &Runner{
		logger:    opts.Logger,
		cfg:       opts.Config,
		home:      opts.Config.homeID(),
		tools:     opts.Tools,
		apps:      opts.Apps,
		modelsInv: opts.Models,
		capacity:  opts.Capacity,
		setup:     opts.Setup,
		calls:     opts.Calls,
		sessions:  opts.Sessions,
		heartbeat: hb,
		metrics:   opts.Metrics,
		pipelines: opts.PipelinesPolicy,
		stop:      make(chan struct{}),
		closed:    make(chan struct{}),
		// Room for one. A nil channel would be safe (both the send and
		// the receive sit in a select), but it would make every request
		// wait for the refresh ticker, which is the wait this exists to
		// remove.
		readvertiseNow: make(chan struct{}, 1),
		// Room for one, for the same reason: a beat already asked for
		// covers every request made before it goes out.
		beatNow: make(chan struct{}, 1),
	}
	pull := opts.ModelPull
	if opts.Models == nil {
		pull = nil
	}
	r.pulls = newModelPuller(opts.Logger, pull, r.RequestImmediateReadvertise)
	return r, nil
}

// RegistrationId returns this machine's v1:worker:registration id, or "" when
// no stream is currently up.
//
// A GETTER rather than a field handed out once, because the id is only known
// after a RegisterAck and the runner may still be backing off through a
// reconnect. Callers that need it (the backup sweeper) re-ask each time and
// skip the pass when it is empty, rather than capturing an empty string at
// startup and never noticing it filled in.
func (r *Runner) RegistrationId() string {
	if r == nil {
		return ""
	}
	if conn := r.conn.Load(); conn != nil {
		return conn.RegistrationId
	}
	return ""
}

// Run blocks until ctx is cancelled or the runner is closed. It reconnects
// on every disconnect, and HOW SOON depends on what the last attempt
// learned:
//
//   - The cluster could not be reached: exponential backoff, 1s -> 15s
//     with jitter. The ceiling is deliberately short: a multi-home fleet
//     must regain availability aggressively after a real network blip; a
//     60s wait left prod machines looking "gone" long after the laptop
//     was awake.
//   - The cluster answered and REFUSED this machine -- its token, or its
//     registration: a hold of a minute, doubling to fifteen, and one
//     error line rather than one warning every fifteen seconds forever
//     (refusal.go, memql-cockpit#427).
//   - A stream ended: the backoff resets to 1s only if the stream had
//     proven itself healthy -- one heartbeat sent (memql-cockpit#431).
//     A cluster that accepts Register and then drops the stream would
//     otherwise be re-asked every second, and every ask is a full Register
//     plus a hardware scan that shells out to nvidia-smi and docker.
func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("worker.runner: not initialized")
	}
	if lifecycle, ok := r.tools.(interface{ RunWorkbenchLifecycle(context.Context) func() }); ok {
		defer lifecycle.RunWorkbenchLifecycle(ctx)()
	}
	r.started.Store(true)
	defer close(r.closed)
	// The home's series exist from the start, reading "not connected",
	// rather than appearing at the first event -- an alert on a missing
	// series would otherwise fire for every home that has not flapped yet.
	r.metrics.SetConnected(r.home, false)
	r.metrics.SetRefused(r.home, false)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	backoff := r.minBackoff()
	for {
		if err := r.stopErr(ctx); err != nil {
			return err
		}

		// Each stream is opened on a context of its own, so closing ONE
		// stream can cancel it (Connection.Close) without touching the run.
		streamCtx, cancelStream := context.WithCancel(ctx)
		conn, err := r.connect(streamCtx)
		if err != nil {
			cancelStream()
			if err := r.stopErr(ctx); err != nil {
				return err
			}
			r.metrics.RecordReconnect(r.home)
			if !r.wait(ctx, r.afterConnectFailure(err, &backoff)) {
				return ctx.Err()
			}
			continue
		}
		conn.cancel = cancelStream
		r.afterConnect()
		r.conn.Store(conn)
		r.metrics.SetConnected(r.home, true)

		healthy, streamErr := r.runStream(ctx, conn)
		conn.Close()
		r.conn.Store(nil)
		r.metrics.SetConnected(r.home, false)

		if streamErr == nil {
			return nil
		}
		if err := r.stopErr(ctx); err != nil {
			return err
		}
		if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
			return streamErr
		}
		if healthy {
			backoff = r.minBackoff()
		}
		code, reason := streamEndCodeReason(streamErr)
		r.logger.Warn("worker stream ended; will reconnect",
			"error", streamErr,
			"code", code,
			"reason", reason,
			"healthy", healthy,
			"backoff_seconds", backoff.Seconds(),
		)
		if !r.wait(ctx, backoff) {
			return ctx.Err()
		}
		backoff = nextBackoff(backoff, r.maxBackoff())
	}
}

// stopErr is why the loop must end now, or nil: its context ended, or
// Close was called. Close is checked HERE, synchronously, and not only
// through the context it cancels: Close ends the stream the loop is
// reading, and a loop that looked only at a context not yet cancelled
// would read that as an ordinary disconnect and dial again.
func (r *Runner) stopErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-r.stop:
		return context.Canceled
	default:
		return nil
	}
}

// wait pauses between attempts through the seam, or with jitter.
func (r *Runner) wait(ctx context.Context, d time.Duration) bool {
	if r.pause != nil {
		return r.pause(ctx, d)
	}
	return sleepWithJitter(ctx, d)
}

// connect opens the stream and runs the handshake with what this machine
// offers right now.
//
// The app and model inventories are taken BEFORE the dial: the app probes
// are subprocesses with timeouts of their own, and an open stream that
// sends nothing while they run is a stream the front door may idle out.
// The hardware scan stays after it, so a cluster that cannot be reached is
// not also a reason to shell out to nvidia-smi on every attempt.
func (r *Runner) connect(ctx context.Context) (*Connection, error) {
	dial := r.dial
	if dial == nil {
		dial = func(ctx context.Context, cfg Config) (stream, error) { return dialSDK(ctx, cfg, r.logger) }
	}
	inventory, modelInv, pipelines := r.inventory(ctx), r.modelInventory(ctx), r.pipelinesPolicy()
	s, err := dial(ctx, r.cfg)
	if err != nil {
		return nil, err
	}
	cfg := r.cfg
	if ready, ok := r.tools.(interface{ WorkbenchReady(context.Context) bool }); ok {
		cfg.workbenchReady = ready.WorkbenchReady(ctx)
	}
	return handshake(ctx, s, cfg, inventory, modelInv, r.hardwareInventory(ctx), pipelines, r.logger)
}

// Close stops the runner: the loop ends at once, and the stream with it.
// It returns once Run has returned, and at once when Run was never
// started.
func (r *Runner) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		if conn := r.conn.Load(); conn != nil {
			conn.Close()
		}
	})
	if r.started.Load() {
		<-r.closed
	}
}

// runStream handles inbound traffic on an active connection. healthy
// reports whether the stream proved itself before it ended: at least one
// heartbeat went out on it.
//
// The heartbeat goroutine is JOINED before this returns. It is where the
// advertisement is re-checked, so one outliving its stream could act on
// the next stream's runner state with the last stream's connection.
func (r *Runner) runStream(ctx context.Context, conn *Connection) (healthy bool, err error) {
	hbCtx, hbCancel := context.WithCancel(ctx)
	var beat atomic.Bool
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		// A failed outbound heartbeat does not guarantee that Recv ends.
		// Close this connection when its heartbeat loop exits so the ordinary
		// disconnect path stops owned calls and reconnects. Leaving Recv alive
		// would keep a silent worker and its sessions stranded indefinitely.
		// Connection.Close already bounds a half-close blocked by flow control.
		defer conn.Close()
		r.heartbeatLoop(hbCtx, conn, &beat)
	}()
	defer func() {
		hbCancel()
		<-hbDone
	}()

	for {
		msg, err := conn.Recv()
		if err != nil {
			// A disconnect ends every live app session with a named
			// error. The stream is the only channel back to the caller,
			// so a session that outlived it has nowhere to report and
			// nobody waiting -- and the process it is supervising would
			// keep working on somebody's machine with nothing watching
			// it, which is precisely what cancel exists to prevent.
			if r.sessions != nil {
				r.sessions.StopAll("the worker's stream to the cluster was lost")
			}
			// The same argument for model calls, and it is the stronger
			// one: an abandoned generation keeps a GPU busy producing
			// output that has nowhere to go.
			if r.calls != nil {
				r.calls.StopAll("the worker's stream to the cluster was lost")
			}
			// And for pulls, with one difference: the engine has already
			// given up every pull on this stream (worker_disconnected),
			// so the download is stopped rather than finished into a
			// void. The blobs already fetched stay on disk for the next
			// pull of the same model to resume.
			r.pulls.StopAll("the worker's stream to the cluster was lost")
			// And for tool calls (memql#5494). The engine has already
			// answered every one of them worker_disconnected, and this
			// loop does not reconnect until they return -- so a pipeline
			// step left running would keep the machine off the cluster
			// for as long as its own timeout, an hour, working for
			// nobody.
			if n := r.toolCalls.cancelAll(); n > 0 {
				r.logger.Info("the worker's stream to the cluster was lost; stopping the tool calls in flight", "tool_calls", n)
			}
			r.active.Wait()
			return beat.Load(), err
		}
		if err := r.handleMessage(ctx, conn, msg); err != nil {
			r.logger.Warn("worker message handling failed",
				"error", err,
			)
		}
	}
}

// DefaultHeartbeat is the beat interval when the caller states none.
//
// A named constant rather than a literal because the hardware refresh
// cadence is stated in BEATS and its wall-clock interval follows from
// this number -- so a test that pinned the interval against its own
// copy of 15s would stay green while this moved, and the refresh would
// silently become five minutes.
const DefaultHeartbeat = 15 * time.Second

// DefaultReconnectMaxBackoff caps stream reconnect delay. Kept at one
// heartbeat interval so a home that was healthy recovers inside the
// cluster OnlineWindow rather than sitting dark for a minute.
const DefaultReconnectMaxBackoff = 15 * time.Second

// defaultReconnectMinBackoff is where the backoff starts, and where a
// healthy stream's end puts it back.
const defaultReconnectMinBackoff = time.Second

// hardwareRefreshBeats is how often the hardware inventory is re-scanned
// onto the heartbeat (design record D1: "refreshed on every tenth
// heartbeat"). At the 15-second default that is a refresh every two and
// a half minutes, so a pulled model or an installed runtime shows within
// minutes -- the record's own words -- without shelling out to
// nvidia-smi and `docker version` four times a minute.
const hardwareRefreshBeats = 10

// hardwareOnBeat reports whether this beat carries the inventory.
//
// Register carries the first one, so beat 10 is the first REFRESH
// rather than the first report -- a machine that reported on beat 1 as
// well would send the same payload twice within fifteen seconds of
// connecting.
func hardwareOnBeat(beat int) bool { return beat > 0 && beat%hardwareRefreshBeats == 0 }

// heartbeatLoop beats until ctx ends or a beat fails, and is where the
// advertisement is re-checked. beat is set once a heartbeat has gone out:
// the stream has proven itself (see Run).
func (r *Runner) heartbeatLoop(ctx context.Context, conn *Connection, beat *atomic.Bool) {
	t := time.NewTicker(r.heartbeat)
	defer t.Stop()
	refresh := time.NewTicker(modelRefreshInterval)
	defer refresh.Stop()
	beats := 0
	var lastBeat time.Time
	// held is a step's beat waiting out setupBeatSpacing; nil when none is.
	var held <-chan time.Time
	send := func(hw *hardware.Inventory) bool {
		if err := conn.SendHeartbeat(0, nil, r.inventory(ctx), hw, r.capacityReport(ctx), r.setupReport()); err != nil {
			r.logger.Warn("worker heartbeat failed; reconnecting", "error", err)
			return false
		}
		lastBeat = time.Now()
		return true
	}
	// Register carries no setup report, so a stream that opens while an
	// install is under way beats at once: until it does, the cluster reads
	// a machine whose installer is downloading as one with nothing on it.
	if r.setupReport() != nil {
		r.RequestBeat()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.beatNow:
			// The installer's report changed a step (setupprogress.go).
			// Not a tick: it neither counts toward the hardware refresh nor
			// proves the stream -- only the cadence does that (see Run).
			if wait := setupBeatSpacing - time.Since(lastBeat); !lastBeat.IsZero() && wait > 0 {
				if held == nil {
					held = time.After(wait)
				}
				continue
			}
			if !send(nil) {
				return
			}
		case <-held:
			held = nil
			if !send(nil) {
				return
			}
		case <-refresh.C:
			r.maybeReadvertiseModels(ctx, conn)
		case <-r.readvertiseNow:
			// A pull just finished with somebody watching, or the policy
			// was reloaded. Evaluating here rather than at the next
			// refresh tick is the difference between a change that
			// lands now and one that lands in up to a minute -- and a
			// minute of nothing happening reads as a pull that failed.
			r.maybeReadvertiseModels(ctx, conn)
		case <-t.C:
			// The inventory is re-taken on every beat rather than
			// captured at connect. The engine applies an inventory
			// change to its live registry immediately and persists it
			// outside the 60s lastSeenAt throttle precisely because it
			// is a routing change -- so signing into Claude Code makes
			// this machine selectable on the NEXT BEAT, not the next
			// reconnect. Sending a snapshot would give that back.
			beats++
			// The hardware inventory rides every tenth beat and nothing
			// in between. It is scanned HERE rather than cached on the
			// Runner because a scan whose result is held across beats
			// would report a runtime that has since been uninstalled --
			// and the whole reason for the refresh is that a machine
			// changes under the worker.
			var hw *hardware.Inventory
			if hardwareOnBeat(beats) {
				inv := r.hardwareInventory(ctx)
				hw = &inv
			}
			if !send(hw) {
				return
			}
			beat.Store(true)
		}
	}
}

// setupBeatSpacing is the least time between two beats sent for a step of
// the install: a burst of steps -- the plan, then the first stage starting --
// goes out as two beats a second apart rather than as many as were reported.
// Above the engine's floor for writing a step ahead of its batch (half a
// second), so jitter on the wire never holds a step back for a whole
// interval.
const setupBeatSpacing = time.Second

// RequestBeat asks for a heartbeat now. It never blocks: a beat already
// asked for covers this one.
func (r *Runner) RequestBeat() {
	if r == nil || r.beatNow == nil {
		return
	}
	select {
	case r.beatNow <- struct{}{}:
	default:
	}
}

// capacityReport measures what the machine can take on now, or nil when it
// cannot vouch for a figure. Measured on every beat: free memory and loaded
// models move with every call, and the engine stops trusting a report after
// two minutes.
func (r *Runner) capacityReport(ctx context.Context) *capacity.Report {
	if r == nil || r.capacity == nil {
		return nil
	}
	report, ok := r.capacity.Probe(ctx)
	if !ok {
		return nil
	}
	return &report
}

// setupReport is the installer's report for this beat, or nil when this
// worker keeps none.
func (r *Runner) setupReport() *memqlv1.SetupProgress {
	if r == nil || r.setup == nil {
		return nil
	}
	return r.setup.Proto()
}

// pipelinesPolicy reads the live pipelines consent, defaulting closed.
func (r *Runner) pipelinesPolicy() tools.PipelinesPolicy {
	if r.pipelines == nil {
		return tools.PipelinesPolicy{}
	}
	return r.pipelines()
}

// modelInventory takes the current local model inventory, or the zero
// value when this build reports none. The zero value advertises nothing,
// which is what a cockpit that cannot serve models should say.
func (r *Runner) modelInventory(ctx context.Context) models.Inventory {
	if r == nil || r.modelsInv == nil {
		return models.Inventory{}
	}
	return r.modelsInv.Models(ctx)
}

// hardwareInventory scans this machine for Register and the tenth beats.
func (r *Runner) hardwareInventory(ctx context.Context) hardware.Inventory {
	if r.scanHardware != nil {
		return r.scanHardware(ctx)
	}
	return hardware.Local(ctx)
}

// maybeReadvertiseModels reconnects only when the model inventory or repository
// scope changed and no work is in flight. A deliberate pull or policy reload
// can bypass the reconnect floor once. Model sharing is stored in Fleet and
// takes effect on the next admission without a worker reconnect.
func (r *Runner) maybeReadvertiseModels(ctx context.Context, conn *Connection) bool {
	if r == nil || conn == nil || conn.closing.Load() {
		return false
	}
	labelsChanged := false
	if r.modelsInv != nil {
		labelsChanged = advertisedFingerprint(r.modelInventory(ctx).Labels()) != conn.ModelFingerprint
	}
	// The pipelines label is another part of the advertisement (memql#5494),
	// and an ordinary one: no floor bypass of its own, because a step routed
	// on a stale grant is refused by the live policy before it runs.
	pipelines := r.pipelinesPolicy()
	pipelinesChanged := pipelines.AdvertisementFingerprint() != conn.AdvertisedPipelines
	workbenchChanged := false
	if ready, ok := r.tools.(interface{ WorkbenchReady(context.Context) bool }); ok {
		workbenchChanged = ready.WorkbenchReady(ctx) != conn.AdvertisedWorkbench
	}
	if !labelsChanged && !pipelinesChanged && !workbenchChanged {
		return false
	}
	if r.busy() {
		r.logger.Debug("this machine's advertisement changed; deferring re-registration until this worker is idle",
			"models_changed", labelsChanged,
			"pipelines_changed", pipelinesChanged,
		)
		return false
	}
	now := r.clock()
	last := r.lastReadvertise.Load()
	switch {
	case last != 0 && now.Sub(time.Unix(0, last)) < modelReadvertiseMinInterval:
		// Swap rather than Load: the bypass must be consumed by the one
		// reconnect it paid for. Left armed, it would sit there and let
		// a flapping runtime through the floor at some unrelated later
		// moment.
		if !r.pendingReadvertise.Swap(false) {
			return false
		}
		r.logger.Info("re-advertising without waiting out the floor: a model pull or a policy reload asked for it")
	default:
		r.pendingReadvertise.Store(false)
	}
	r.lastReadvertise.Store(now.UnixNano())
	switch {
	case pipelinesChanged:
		r.logger.Info("pipeline repository policy changed; reconnecting to update routing")
	default:
		r.logger.Info("local model inventory changed; reconnecting to re-advertise",
			"models_offered", len(r.modelInventory(ctx).Advertised()),
		)
	}
	// Closing the connection surfaces as a Recv error in runStream,
	// which returns and lets Run reconnect. There is no lighter way to
	// re-register: the engine binds the advertisement at the handshake.
	conn.closing.Store(true)
	conn.Close()
	return true
}

// RequestImmediateReadvertise says the local model set just changed and
// SOMEBODY IS WATCHING -- the caller pulled a model, on purpose, in
// front of a person who pressed a button.
//
// Two things happen, and they are one call because a caller that did
// only the first would ship the bug this method exists to prevent:
//
//  1. The cached inventory is dropped. Discovery is cached for
//     DefaultModelInventoryTTL, and a model pulled a moment ago is not in
//     a probe taken a minute before it. Re-advertising without this
//     spends a reconnect to re-register the labels from BEFORE the pull:
//     the reconnect happens, nothing changes, and the pull reads to the
//     person watching as one that did nothing.
//  2. The two-minute floor is waived for the next evaluation, once. Two
//     minutes of a model that does not appear is indistinguishable from
//     a failed pull, and the person is looking at the screen.
//
// The BUSY guard is NOT waived, deliberately. A reconnect that kills a
// running model call or an hour-old app session is worse than a label
// that is a minute stale, and somebody watching a pull is not a reason
// to throw away somebody else's work. The request survives that wait.
//
// WHO CALLS IT, and from where. Only something inside THIS process can,
// and the cockpit's own pull path runs in another one: `memql worker
// models --pull` and `memql worker setup --inference` reach the running
// worker through the SIGHUP they already send after writing
// models.allow, and the handler in cli.go asks for this after reloading
// the policy -- a reloaded allow list that nobody re-advertised is a
// model the cluster still cannot see. The cluster's own ModelPullStart
// arm (modelpull.go) calls it directly, after reloading the policy
// itself and AFTER sending its End: the reconnect this asks for closes
// the stream that End rides.
func (r *Runner) RequestImmediateReadvertise() {
	if r == nil {
		return
	}
	r.pendingReadvertise.Store(true)
	if r.modelsInv != nil {
		r.modelsInv.Invalidate()
	}
	select {
	case r.readvertiseNow <- struct{}{}:
	default:
		// Already woken and not yet evaluated, or no loop running (the
		// worker is reconnecting). Either way the arm above is what
		// carries the request; a second token would only buy a second
		// evaluation of the same answer.
	}
}

// clock reads the injectable time source, defaulting to time.Now for
// every runner but the ones tests build.
func (r *Runner) clock() time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return time.Now()
}

// busy reports whether this worker has work a reconnect would interrupt.
func (r *Runner) busy() bool {
	if r.activeCalls.Load() > 0 {
		return true
	}
	if r.sessions != nil && r.sessions.Live() > 0 {
		return true
	}
	if r.calls != nil && r.calls.Live() > 0 {
		return true
	}
	// A pull holds this guard until AFTER its End is sent, so the
	// re-advertise one model earns cannot close the stream under its
	// sibling's download -- the recommended set is a pair, asked for at
	// once -- and cannot lose the End that reports its own success.
	if r.pulls.Live() > 0 {
		return true
	}
	return false
}

// inventory takes the current local app inventory, or nil when this
// build reports none.
func (r *Runner) inventory(ctx context.Context) []apps.Info {
	if r == nil || r.apps == nil {
		return nil
	}
	return r.apps.Apps(ctx)
}

func (r *Runner) handleMessage(ctx context.Context, conn *Connection, msg *memqlv1.WorkerServerMessage) error {
	if msg == nil {
		return nil
	}
	switch payload := msg.GetPayload().(type) {
	case *memqlv1.WorkerServerMessage_ToolDispatch:
		// The call is recorded HERE, on the receive goroutine, before its
		// own goroutine starts: a ToolCancel for it arrives on this same
		// goroutine later, and finds it however soon it comes.
		callCtx, cancelCall := context.WithCancel(ctx)
		forget := r.toolCalls.add(payload.ToolDispatch.GetCallId(), cancelCall)
		r.active.Add(1)
		r.activeCalls.Add(1)
		go func() {
			defer r.active.Done()
			defer r.activeCalls.Add(-1)
			defer forget()
			defer cancelCall()
			r.runToolDispatch(callCtx, conn, payload.ToolDispatch)
		}()
	case *memqlv1.WorkerServerMessage_ToolCancel:
		// The engine sends this when it stops waiting for a call -- a
		// pipeline run cancelled, a deadline on its side -- and ending the
		// call is the only thing that stops a step it no longer wants
		// (memql#5494). Its result is still sent; the engine drops it.
		running := r.toolCalls.cancel(payload.ToolCancel.GetCallId())
		r.logger.Info("worker received tool cancel",
			"call_id", payload.ToolCancel.GetCallId(),
			"reason", payload.ToolCancel.GetReason(),
			"running", running > 0,
		)
	case *memqlv1.WorkerServerMessage_AppSessionStart:
		if r.sessions == nil {
			r.logger.Warn("app session start received but this build runs no sessions",
				"session_id", payload.AppSessionStart.GetSessionId())
			break
		}
		r.sessions.Start(ctx, conn, payload.AppSessionStart)
	case *memqlv1.WorkerServerMessage_ModelCallStart:
		if r.calls == nil {
			r.logger.Warn("model call start received but this build serves no models",
				"request_id", payload.ModelCallStart.GetRequestId())
			break
		}
		r.calls.Start(ctx, conn, payload.ModelCallStart)
	case *memqlv1.WorkerServerMessage_ModelCallCancel:
		if r.calls != nil {
			r.calls.Cancel(payload.ModelCallCancel)
		}
	case *memqlv1.WorkerServerMessage_AppSessionControl:
		if r.sessions != nil {
			r.sessions.Control(payload.AppSessionControl)
		}
	case *memqlv1.WorkerServerMessage_Ping:
		r.answerPing(conn, payload.Ping)
	case *memqlv1.WorkerServerMessage_ModelPullStart:
		// Nil-safe: a runner built without the seam refuses in a
		// sentence rather than dropping the request.
		r.pulls.Start(ctx, conn, payload.ModelPullStart)
	case *memqlv1.WorkerServerMessage_ModelPullCancel:
		r.pulls.Cancel(payload.ModelPullCancel)
	case *memqlv1.WorkerServerMessage_Drain:
		r.logger.Info("worker received drain; will exit after in-flight calls finish")
		// App sessions are not tool calls and are not in r.active: a
		// session can run for an hour, and draining is not a reason to
		// abandon one silently. Cancel them by name so each reports its
		// own end before the stream goes.
		if r.sessions != nil {
			r.sessions.StopAll("the cluster asked this worker to drain")
		}
		if r.calls != nil {
			r.calls.StopAll("the cluster asked this worker to drain")
		}
		r.pulls.StopAll("the cluster asked this worker to drain")
		r.active.Wait()
		return errServerDrain
	case *memqlv1.WorkerServerMessage_RotationResponse:
		r.logger.Info("worker received rotation response (ignored in MVP)")
	}
	return nil
}

// answerPing is the Pong (epic memql#5218, D11): the cluster's evidence
// that the return path to this machine works, and how fast. It is
// answered at once, on the recv goroutine, because anything queued behind
// a tool dispatch would measure the queue rather than the path.
//
// DEBUG, NEVER INFO. The cluster pings every machine once a minute for as
// long as it is connected; an info line for each is a log that says
// nothing forever.
func (r *Runner) answerPing(conn *Connection, ping *memqlv1.Ping) {
	if ping == nil {
		return
	}
	if err := conn.SendPong(ping.GetRequestId(), ping.GetSentAt()); err != nil {
		r.logger.Debug("pong not sent", "request_id", ping.GetRequestId(), "error", err)
		return
	}
	r.logger.Debug("answered the cluster's ping", "request_id", ping.GetRequestId())
}

func (r *Runner) runToolDispatch(ctx context.Context, conn *Connection, dispatch *memqlv1.ToolDispatch) {
	if dispatch == nil {
		return
	}
	if err := validateWorkbenchEnrollment(r.cfg, conn, dispatch, time.Now()); err != nil {
		_ = conn.SendToolResult(dispatch.GetCallId(), nil, &memqlv1.Failure{ErrorCode: "workbench_constraint_refused", ErrorMessage: err.Error()})
		return
	}
	if c := dispatch.GetComputerPermission(); c != nil {
		machineID, err := machineIDFor(r.cfg)
		if err != nil || c.GetOwnerUserId() != conn.OwnerUserId || c.GetRegistrationId() != conn.RegistrationId || c.GetMachineId() != machineID {
			_ = conn.SendToolResult(dispatch.GetCallId(), nil, &memqlv1.Failure{ErrorCode: "computer_permission_refused", ErrorMessage: "This permission belongs to a different machine or enrollment."})
			return
		}
	}
	if r.tools == nil {
		_ = conn.SendToolResult(dispatch.GetCallId(), nil, &memqlv1.Failure{
			ErrorCode:    "no_tools_configured",
			ErrorMessage: "worker has no tool dispatcher configured",
		})
		return
	}
	timeout := time.Duration(dispatch.GetTimeout().GetSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	dispatchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startedAt := time.Now()
	var success *memqlv1.Success
	var failure *memqlv1.Failure
	if streaming, ok := r.tools.(StreamingToolDispatcher); ok {
		// The call's output goes out as it is produced, on this stream,
		// and all of it before the result below.
		success, failure = streaming.DispatchStream(dispatchCtx, dispatch, conn.SendToolStream)
	} else {
		success, failure = r.tools.Dispatch(dispatchCtx, dispatch)
	}
	durationMs := time.Since(startedAt).Milliseconds()

	if r.metrics != nil {
		outcome := "success"
		if failure != nil {
			outcome = failure.GetErrorCode()
			if outcome == "" {
				outcome = "failure"
			}
		}
		r.metrics.RecordCall(r.home, outcome, durationMs)
	}

	if err := conn.SendToolResult(dispatch.GetCallId(), success, failure); err != nil {
		r.logger.Warn("worker failed to send tool result",
			"call_id", dispatch.GetCallId(),
			"error", err,
		)
	}
}

func (r *Runner) minBackoff() time.Duration {
	if r.backoffMin > 0 {
		return r.backoffMin
	}
	return defaultReconnectMinBackoff
}

func (r *Runner) maxBackoff() time.Duration {
	if r.backoffMax > 0 {
		return r.backoffMax
	}
	return DefaultReconnectMaxBackoff
}

func nextBackoff(current, max time.Duration) time.Duration {
	doubled := current * 2
	if doubled > max {
		return max
	}
	return doubled
}

func sleepWithJitter(ctx context.Context, base time.Duration) bool {
	var jitter time.Duration
	if quarter := int64(base / 4); quarter > 0 {
		jitter = time.Duration(rand.Int63n(quarter))
	}
	t := time.NewTimer(base + jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
