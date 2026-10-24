package worker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/hardware"
)

// fakecluster_test.go is a cluster a Runner can be pointed at with no
// network: the Runner's dial seam hands it one scriptStream per attempt,
// and everything after the dial -- the handshake, the loop, the backoff,
// the hold -- is the production code.
//
// scriptStream locks around Send the way the SDK's connection does. The
// serialization itself is proven where the lock lives (memql
// sdk/go/worker); what these tests prove is everything above it, and
// under -race that nothing above it shares state unsafely.

// inbound is one thing the cluster says: a message, or the end of the
// stream.
type inbound struct {
	msg *memqlv1.WorkerServerMessage
	err error
}

// scriptStream is one stream to the fake cluster.
type scriptStream struct {
	n int // which dial produced it

	mu     sync.Mutex
	frames []*memqlv1.WorkerClientMessage
	sent   chan *memqlv1.WorkerClientMessage

	inbox     chan inbound
	closed    chan struct{}
	closeOnce sync.Once
}

func newScriptStream(n int) *scriptStream {
	return &scriptStream{
		n:      n,
		sent:   make(chan *memqlv1.WorkerClientMessage, 4096),
		inbox:  make(chan inbound, 64),
		closed: make(chan struct{}),
	}
}

func (s *scriptStream) Send(m *memqlv1.WorkerClientMessage) error {
	select {
	case <-s.closed:
		return errors.New("scriptStream: send on a closed stream")
	default:
	}
	s.mu.Lock()
	s.frames = append(s.frames, m)
	s.mu.Unlock()
	select {
	case s.sent <- m:
	default:
	}
	return nil
}

func (s *scriptStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	select {
	case in := <-s.inbox:
		return in.msg, in.err
	case <-s.closed:
		return nil, status.Error(codes.Canceled, "the worker closed the stream")
	}
}

func (s *scriptStream) Close() { s.closeOnce.Do(func() { close(s.closed) }) }

// say queues something the cluster says on this stream.
func (s *scriptStream) say(msg *memqlv1.WorkerServerMessage) { s.inbox <- inbound{msg: msg} }

// drop ends the stream from the cluster's side.
func (s *scriptStream) drop(err error) { s.inbox <- inbound{err: err} }

// messages copies every frame the worker has sent on this stream.
func (s *scriptStream) messages() []*memqlv1.WorkerClientMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*memqlv1.WorkerClientMessage(nil), s.frames...)
}

// register is the Register this stream opened with.
func (s *scriptStream) register() *memqlv1.Register {
	for _, m := range s.messages() {
		if r := m.GetRegister(); r != nil {
			return r
		}
	}
	return nil
}

// await waits for the worker to send a frame that match accepts.
func (s *scriptStream) await(t *testing.T, what string, match func(*memqlv1.WorkerClientMessage) bool) *memqlv1.WorkerClientMessage {
	t.Helper()
	for _, m := range s.messages() {
		if match(m) {
			return m
		}
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-s.sent:
			if match(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("stream %d: the worker never sent %s", s.n, what)
			return nil
		}
	}
}

// answer is how the fake cluster treats one dial.
type answer struct {
	// unreachable fails the dial itself, the way a network that is down
	// does.
	unreachable error
	// refuse ends the stream at the door with this status, the way the
	// engine's interceptor refuses a token.
	refuse error
	// registerErr answers Register with a RegisterError.
	registerErr *memqlv1.RegisterError
	// dropAfterAck acks, then ends the stream at once.
	dropAfterAck bool
}

func accept() answer { return answer{} }

func refuseToken() answer {
	return answer{refuse: status.Error(codes.Unauthenticated, "invalid worker token")}
}

func unreachable() answer {
	return answer{unreachable: status.Error(codes.Unavailable, "connection refused")}
}

func removed() answer {
	return answer{registerErr: &memqlv1.RegisterError{Code: "register_failed", Message: "worker is revoked"}}
}

// fakeCluster hands out one scriptStream per dial, answered by script.
type fakeCluster struct {
	script func(n int) answer

	mu      sync.Mutex
	dials   int
	streams []*scriptStream
	dialed  chan *scriptStream
}

func newFakeCluster(script func(n int) answer) *fakeCluster {
	return &fakeCluster{script: script, dialed: make(chan *scriptStream, 256)}
}

func (c *fakeCluster) dial(ctx context.Context, cfg Config) (stream, error) {
	c.mu.Lock()
	n := c.dials
	c.dials++
	c.mu.Unlock()
	a := c.script(n)
	if a.unreachable != nil {
		c.announce(ctx, nil)
		return nil, a.unreachable
	}
	s := newScriptStream(n)
	// The SDK opens its stream on the dial's context, so cancelling that
	// context ends the stream -- which is how Run's own cancel reaches a
	// Recv that is waiting on it. The fake does the same.
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	switch {
	case a.refuse != nil:
		s.drop(a.refuse)
	case a.registerErr != nil:
		s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_RegisterError{RegisterError: a.registerErr}})
		s.drop(status.Error(codes.PermissionDenied, a.registerErr.GetMessage()))
	default:
		s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_RegisterAck{RegisterAck: &memqlv1.RegisterAck{
			RegistrationId: "reg-" + cfg.homeID(),
			OwnerUserId:    "user-1",
			RegisteredAt:   timestamppb.Now(),
		}}})
		if a.dropAfterAck {
			s.drop(status.Error(codes.Unavailable, "transport is closing"))
		}
	}
	c.mu.Lock()
	c.streams = append(c.streams, s)
	c.mu.Unlock()
	c.announce(ctx, s)
	return s, nil
}

// announce hands a dial to whoever is waiting in next -- unless the dial's
// context ends first, so a runner looping against a cluster nobody is
// reading from still stops when it is told to.
func (c *fakeCluster) announce(ctx context.Context, s *scriptStream) {
	select {
	case c.dialed <- s:
	case <-ctx.Done():
	}
}

// next waits for the next dial and returns its stream (nil for a dial
// that was unreachable).
func (c *fakeCluster) next(t *testing.T) *scriptStream {
	t.Helper()
	select {
	case s := <-c.dialed:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never dialed again")
		return nil
	}
}

func (c *fakeCluster) dialCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dials
}

// fakeClock is the runner's clock in these tests: it moves only when a
// pause says the runner waited, so time-based rules (the refusal grace)
// are asserted exactly and without waiting.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// waitRecorder stands in for the pause between attempts: it records what
// the runner asked to wait, moves the fake clock by that much, and does
// not wait -- so the backoff and the hold are asserted exactly rather
// than timed.
type waitRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
	clock *fakeClock
	// onWait, when set, runs inside each pause -- the moment the runner
	// has decided and before it tries again.
	onWait func(d time.Duration)
}

func (w *waitRecorder) pause(ctx context.Context, d time.Duration) bool {
	w.mu.Lock()
	w.waits = append(w.waits, d)
	hook := w.onWait
	clock := w.clock
	w.mu.Unlock()
	if clock != nil {
		clock.advance(d)
	}
	if hook != nil {
		hook(d)
	}
	return ctx.Err() == nil
}

func (w *waitRecorder) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

// logBuffer captures a runner's log lines, safely across goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// count is how many captured lines contain every one of parts.
func (l *logBuffer) count(parts ...string) int {
	n := 0
	for _, line := range strings.Split(l.String(), "\n") {
		all := line != ""
		for _, p := range parts {
			if !strings.Contains(line, p) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// slogJSON is a debug-level JSON logger into w.
func slogJSON(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// testHomeConfig is a valid single-home config with its own state dir.
func testHomeConfig(t *testing.T, home string) Config {
	t.Helper()
	return Config{
		ClusterURL:   "https://api." + home + ".example",
		Token:        "mql_wkr_" + home + "_token_aaaaaaaaaaaa",
		Name:         "test-machine",
		Capabilities: []string{"HEADLESS"},
		Concurrency:  map[string]uint32{"HEADLESS": 1},
		StateDir:     t.TempDir(),
		Home:         home,
	}
}

// runnerAgainst builds a Runner pointed at cluster, logging into logs,
// pausing through waits, and scanning no hardware.
func runnerAgainst(t *testing.T, cluster *fakeCluster, opts Options, logs io.Writer, waits *waitRecorder) *Runner {
	t.Helper()
	if logs == nil {
		logs = io.Discard
	}
	opts.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if opts.Heartbeat == 0 {
		// Long enough that no beat lands unless a test waits for one.
		opts.Heartbeat = time.Hour
	}
	r, err := NewRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	r.dial = cluster.dial
	r.scanHardware = func(context.Context) hardware.Inventory { return hardware.Inventory{Chip: "test"} }
	if waits != nil {
		if waits.clock == nil {
			waits.clock = newFakeClock()
		}
		r.now = waits.clock.Now
		r.pause = waits.pause
	}
	return r
}

// runInBackground runs r until the test ends. finished closes when Run
// returns, and result then reports what it returned.
func runInBackground(t *testing.T, r *Runner) (cancel context.CancelFunc, finished <-chan struct{}, result func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after its context was cancelled")
		}
	})
	return cancel, done, func() error { <-done; return runErr }
}

func isHeartbeat(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil }
