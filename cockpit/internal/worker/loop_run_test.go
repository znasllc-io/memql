package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql/cockpit/internal/worker/models"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// loop_run_test.go drives Runner.Run -- the reconnect loop, the hold, the
// heartbeat, the drain -- against the fake cluster in fakecluster_test.go
// (memql-cockpit#434). Every one of these paths was untested before: the
// loop dialled the SDK directly, so nothing could stand where the cluster
// stands.

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func durations(ds ...time.Duration) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// awaitRegistered waits until the runner has a live, registered stream.
func awaitRegistered(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.RegistrationId() == "" {
		if time.Now().After(deadline) {
			t.Fatal("the runner never registered")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRunHoldsAfterRepeatedRefusalsAndSaysSoOnce (memql-cockpit#427).
//
// A revoked token used to be asked about every fifteen seconds forever,
// with a warning each time. Now: the ordinary backoff while the cluster
// has refused for less than refusalGrace (a deploy's token-lookup blip
// looks exactly like this), then a hold of a minute doubling toward
// fifteen, ONE error line, the gauge, and the record `worker config`
// prints -- and all of it undone by the first accepted handshake.
func TestRunHoldsAfterRepeatedRefusalsAndSaysSoOnce(t *testing.T) {
	const refusals = 7
	cluster := newFakeCluster(func(n int) answer {
		if n < refusals {
			return refuseToken()
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	metrics := NewMetrics()
	logs := &logBuffer{}
	waits := &waitRecorder{}
	var sawRecord, sawGauge atomic.Bool
	waits.onWait = func(d time.Duration) {
		if d != defaultRefusalHoldMin {
			return
		}
		rec, ok := loadRefusal(cfg.StateDir)
		sawRecord.Store(ok && rec.Kind == refusedCredential &&
			rec.ClusterSaid == "invalid worker token" && rec.Token == tokenFingerprint(cfg.Token))
		sawGauge.Store(strings.Contains(metrics.render(), `worker_home_refused{home="prod"} 1`))
	}
	r := runnerAgainst(t, cluster, Options{Config: cfg, Metrics: metrics}, logs, waits)
	runInBackground(t, r)

	for i := 0; i <= refusals; i++ {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	// Refusals at 0s, 1s, 3s, 7s and 15s keep the ordinary backoff; the
	// one at 30s has lasted the grace and starts the hold.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, time.Minute, 2 * time.Minute}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s: the ordinary backoff through the grace, then a hold that doubles", durations(got...), durations(want...))
	}
	if n := logs.count(`"level":"ERROR"`, refusalLogSentence(refusedCredential)); n != 1 {
		t.Fatalf("the refusal was logged at error %d times, want exactly once:\n%s", n, logs)
	}
	if n := logs.count(`"cluster_said":"invalid worker token"`, `"fix":"pair this machine again with a new code from the portal`); n != 1 {
		t.Fatalf("the error line must carry what the cluster said and the fix:\n%s", logs)
	}
	if !sawRecord.Load() {
		t.Error("during the hold, the home's state dir must record the refusal for `memql worker config`")
	}
	if !sawGauge.Load() {
		t.Error("during the hold, worker_home_refused must read 1 for the home")
	}

	// Accepted: the hold, the record and the gauge all end.
	if _, ok := loadRefusal(cfg.StateDir); ok {
		t.Error("an accepted handshake must clear the recorded refusal")
	}
	if !strings.Contains(metrics.render(), `worker_home_refused{home="prod"} 0`) {
		t.Error("an accepted handshake must reset worker_home_refused")
	}
	if logs.count("the cluster accepted this machine again") != 1 {
		t.Errorf("the recovery must be said once:\n%s", logs)
	}
}

// Refusals that have not lasted the grace are not a hold, however many:
// a token lookup that failed during an engine deploy answers exactly like
// a revoked token, and treating a few seconds of those as final would
// park a healthy fleet for a minute.
func TestRunRefusalsInsideTheGraceKeepTheOrdinaryBackoff(t *testing.T) {
	cluster := newFakeCluster(func(n int) answer {
		if n < 5 {
			return refuseToken()
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	logs := &logBuffer{}
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg}, logs, waits)
	runInBackground(t, r)
	for i := 0; i < 6; i++ {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s", durations(got...), durations(want...))
	}
	if logs.count(`"level":"ERROR"`) != 0 {
		t.Errorf("fifteen seconds of refusals is inside the grace; no error line:\n%s", logs)
	}
	if _, ok := loadRefusal(cfg.StateDir); ok {
		t.Error("no refusal record inside the grace")
	}
}

// And a streak shorter than refusalStreak never holds either.
func TestRunRefusalsBelowTheStreakKeepTheOrdinaryBackoff(t *testing.T) {
	cluster := newFakeCluster(func(n int) answer {
		if n < refusalStreak-1 {
			return refuseToken()
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	logs := &logBuffer{}
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg}, logs, waits)
	runInBackground(t, r)
	for i := 0; i < refusalStreak; i++ {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	if got, want := waits.got(), []time.Duration{time.Second, 2 * time.Second}; durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s", durations(got...), durations(want...))
	}
	if logs.count(`"level":"ERROR"`) != 0 {
		t.Errorf("no error line below the streak:\n%s", logs)
	}
	if _, ok := loadRefusal(cfg.StateDir); ok {
		t.Error("no refusal record below the streak")
	}
}

// A cluster that cannot be reached is not a cluster that refused: it
// keeps the fast backoff however long it lasts, because the network
// coming back is what it is waiting for.
func TestRunAnUnreachableClusterIsNeverAHold(t *testing.T) {
	cluster := newFakeCluster(func(n int) answer {
		if n < 6 {
			return unreachable()
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	logs := &logBuffer{}
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg}, logs, waits)
	runInBackground(t, r)
	for i := 0; i < 7; i++ {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s", durations(got...), durations(want...))
	}
	if logs.count(`"level":"ERROR"`) != 0 {
		t.Errorf("an unreachable cluster earns warnings, not an error:\n%s", logs)
	}
}

// Once held, a network error in between does not mean the cluster
// changed its mind: the hold keeps doubling until a handshake is
// accepted.
func TestRunTheHoldOutlastsANetworkErrorInBetween(t *testing.T) {
	script := []answer{
		refuseToken(), refuseToken(), refuseToken(), refuseToken(), refuseToken(), refuseToken(),
		unreachable(), refuseToken(), accept(),
	}
	cluster := newFakeCluster(func(n int) answer {
		if n < len(script) {
			return script[n]
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg}, nil, waits)
	runInBackground(t, r)
	for range script {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second,
		time.Minute, 2 * time.Minute, 4 * time.Minute}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s", durations(got...), durations(want...))
	}
}

// A streak is refusals IN A ROW: an attempt that never reached the
// cluster starts it over. Otherwise, after a long outage, the first few
// refusals would count the outage as grace and hold at once.
func TestRunAnUnreachableAttemptStartsTheStreakOver(t *testing.T) {
	script := []answer{
		refuseToken(), refuseToken(), unreachable(),
		refuseToken(), refuseToken(), refuseToken(), refuseToken(), accept(),
	}
	cluster := newFakeCluster(func(n int) answer {
		if n < len(script) {
			return script[n]
		}
		return accept()
	})
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod")}, nil, waits)
	runInBackground(t, r)
	for range script {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	// The streak restarts at the refusal after the unreachable attempt
	// (t=7s); the one at t=45s is its fourth, and the first past the
	// grace. Without the restart the refusal at t=30s would have held.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second, time.Minute}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s", durations(got...), durations(want...))
	}
}

// A machine removed from the fleet (its registration revoked) is a
// refusal too, and its sentence names BOTH ways out: pair again, or stop
// asking by unpairing here.
func TestRunARemovedMachineIsARefusalThatNamesUnpair(t *testing.T) {
	const refusals = 6 // the sixth comes at 30s, the end of the grace
	cluster := newFakeCluster(func(n int) answer {
		if n < refusals {
			return removed()
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	logs := &logBuffer{}
	waits := &waitRecorder{}
	var kind atomic.Value
	waits.onWait = func(d time.Duration) {
		if rec, ok := loadRefusal(cfg.StateDir); ok {
			kind.Store(rec.Kind)
		}
	}
	r := runnerAgainst(t, cluster, Options{Config: cfg}, logs, waits)
	runInBackground(t, r)
	for i := 0; i <= refusals; i++ {
		cluster.next(t)
	}
	awaitRegistered(t, r)

	if n := logs.count(`"level":"ERROR"`, refusalLogSentence(refusedRemoved), `memql worker unpair --cluster prod`); n != 1 {
		t.Fatalf("want one error line naming the removal and the unpair command:\n%s", logs)
	}
	if got, _ := kind.Load().(refusalKind); got != refusedRemoved {
		t.Fatalf("recorded kind = %q, want %q", got, refusedRemoved)
	}
}

// TestRunBackoffResetsOnlyAfterAHealthyHeartbeat (memql-cockpit#431).
//
// The backoff used to reset the moment a handshake succeeded, so a
// cluster that accepted Register and then dropped the stream was asked
// again every second -- each time a full Register and a hardware scan
// that shells out. Now a stream has to prove itself (one heartbeat) to
// earn the reset.
func TestRunBackoffResetsOnlyAfterAHealthyHeartbeat(t *testing.T) {
	// Streams 0-2 drop straight after the ack; stream 3 lives through a
	// heartbeat and is then dropped by the test; stream 4 drops straight
	// after the ack again; stream 5 stays.
	cluster := newFakeCluster(func(n int) answer {
		switch n {
		case 0, 1, 2, 4:
			return answer{dropAfterAck: true}
		}
		return accept()
	})
	cfg := testHomeConfig(t, "prod")
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg, Heartbeat: 150 * time.Millisecond}, nil, waits)
	runInBackground(t, r)

	for i := 0; i < 3; i++ {
		cluster.next(t)
	}
	healthy := cluster.next(t)
	healthy.await(t, "a heartbeat", isHeartbeat)
	healthy.drop(status.Error(codes.Unavailable, "transport is closing"))
	cluster.next(t)
	cluster.next(t)
	awaitRegistered(t, r)

	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second, 2 * time.Second}
	if got := waits.got(); durations(got...) != durations(want...) {
		t.Fatalf("waits = %s, want %s: an unproven stream keeps growing the backoff, a healthy one resets it", durations(got...), durations(want...))
	}
}

// blockingRuntime is an Ollama that takes a chat call and holds it until
// released, then streams one token and finishes. arrived fires once per
// call it receives; abandoned once per call whose caller hung up first.
type blockingRuntime struct {
	srv       *httptest.Server
	arrived   chan struct{}
	abandoned chan struct{}
	release   chan struct{}
}

func newBlockingRuntime(t *testing.T) *blockingRuntime {
	t.Helper()
	b := &blockingRuntime{arrived: make(chan struct{}, 64), abandoned: make(chan struct{}, 64), release: make(chan struct{})}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		b.arrived <- struct{}{}
		select {
		case <-b.release:
		case <-req.Context().Done():
			b.abandoned <- struct{}{}
			return
		}
		enc := json.NewEncoder(w)
		_ = enc.Encode(map[string]any{"model": "m", "message": map[string]string{"content": "tok"}, "done": false})
		_ = enc.Encode(map[string]any{"model": "m", "done": true, "done_reason": "stop"})
	}))
	t.Cleanup(b.srv.Close)
	t.Cleanup(b.open)
	return b
}

// open releases every call, once.
func (b *blockingRuntime) open() {
	select {
	case <-b.release:
	default:
		close(b.release)
	}
}

func (b *blockingRuntime) awaitCall(t *testing.T) {
	t.Helper()
	select {
	case <-b.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime never received the call")
	}
}

// oneModelOn is an inventory offering one chat model served at url.
func oneModelOn(url string) *fakeModelInventory {
	inv := &fakeModelInventory{}
	m := offeredModel("m", models.Attributes{ContextWindow: 8192, MaxConcurrent: 16})
	m.BaseURL = url
	inv.serve(servingInventory(m))
	return inv
}

func modelCallStart(id string) *memqlv1.WorkerServerMessage {
	return &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_ModelCallStart{ModelCallStart: &memqlv1.ModelCallStart{
		RequestId: id, Model: "m", Kind: modelcall.KindChat,
		Messages: []*memqlv1.ModelCallMessage{{Role: "user", Content: "hello"}},
		Limits:   &memqlv1.ModelCallLimits{TimeoutSeconds: 30, IdleTimeoutSeconds: 20, KeepaliveSeconds: 10},
	}}}
}

func endFor(id string) func(*memqlv1.WorkerClientMessage) bool {
	return func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelCallEnd().GetRequestId() == id }
}

// Losing the stream ends every call in flight and frees the runtime: a
// generation that outlived its stream would keep a GPU busy producing
// output nobody can receive. Whether its End reaches a stream that is
// already gone is secondary (modelcall.Manager.StopAll); the runtime
// hearing the hang-up is not.
func TestRunAStreamDropEndsTheCallsInFlight(t *testing.T) {
	runtime := newBlockingRuntime(t)
	inv := oneModelOn(runtime.srv.URL)
	calls := modelcall.NewManager(modelcall.Options{Inventory: inv, Logger: quietLogger()})
	cluster := newFakeCluster(func(int) answer { return accept() })
	cfg := testHomeConfig(t, "prod")
	waits := &waitRecorder{}
	r := runnerAgainst(t, cluster, Options{Config: cfg, Models: inv, Calls: calls}, nil, waits)
	runInBackground(t, r)

	first := cluster.next(t)
	awaitRegistered(t, r)
	first.say(modelCallStart("call-1"))
	runtime.awaitCall(t)
	first.drop(status.Error(codes.Unavailable, "transport is closing"))

	select {
	case <-runtime.abandoned:
	case <-time.After(10 * time.Second):
		t.Fatal("the runtime never heard the call hang up: a lost stream must free the GPU")
	}
	cluster.next(t)
	deadline := time.Now().Add(10 * time.Second)
	for calls.Live() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("Live() = %d after the drop, want 0", calls.Live())
		}
		time.Sleep(time.Millisecond)
	}
	for _, m := range first.messages() {
		if end := m.GetModelCallEnd(); end != nil && end.GetErrorCode() != modelcall.CodeWorkerStopped {
			t.Fatalf("an End that did get out must say %q, got %q", modelcall.CodeWorkerStopped, end.GetErrorCode())
		}
	}
}

// recordingTools answers every dispatch with a success after a moment,
// so dispatches overlap one another and everything else on the stream.
type recordingTools struct{ calls atomic.Int64 }

func (d *recordingTools) Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure) {
	d.calls.Add(1)
	time.Sleep(time.Millisecond)
	return &memqlv1.Success{ExitCode: 0}, nil
}

// TestRunEveryWriterAtOnceUnderRace (memql-cockpit#426 and #434). The
// writers that share this stream -- heartbeats, pongs from the recv
// goroutine, one goroutine per tool result, a model call's deltas -- all
// run at once here, and under -race the cockpit's own state on the way to
// the seam is proven unshared. Every frame asked for must arrive.
func TestRunEveryWriterAtOnceUnderRace(t *testing.T) {
	runtime := newBlockingRuntime(t)
	runtime.open()
	inv := oneModelOn(runtime.srv.URL)
	calls := modelcall.NewManager(modelcall.Options{Inventory: inv, Logger: quietLogger()})
	dispatcher := &recordingTools{}
	cluster := newFakeCluster(func(int) answer { return accept() })
	cfg := testHomeConfig(t, "prod")
	r := runnerAgainst(t, cluster, Options{
		Config: cfg, Models: inv, Calls: calls, Tools: dispatcher, Heartbeat: 2 * time.Millisecond,
	}, nil, &waitRecorder{})
	runInBackground(t, r)
	s := cluster.next(t)
	awaitRegistered(t, r)

	const n = 40
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_Ping{Ping: &memqlv1.Ping{
				RequestId: "ping-" + strconv.Itoa(i), SentAt: timestamppb.Now(),
			}}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_ToolDispatch{ToolDispatch: &memqlv1.ToolDispatch{
				CallId: "tool-" + strconv.Itoa(i), Tool: "workerHost", Action: "fs_stat", Timeout: durationpb.New(time.Minute),
			}}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			s.say(modelCallStart("call-" + strconv.Itoa(i)))
		}
	}()
	wg.Wait()

	for i := 0; i < n; i++ {
		id := "ping-" + strconv.Itoa(i)
		s.await(t, "pong "+id, func(m *memqlv1.WorkerClientMessage) bool { return m.GetPong().GetRequestId() == id })
		tool := "tool-" + strconv.Itoa(i)
		s.await(t, "tool result "+tool, func(m *memqlv1.WorkerClientMessage) bool { return m.GetToolResult().GetCallId() == tool })
	}
	for i := 0; i < 8; i++ {
		id := "call-" + strconv.Itoa(i)
		end := s.await(t, "the End of "+id, endFor(id)).GetModelCallEnd()
		if end.GetErrorCode() != "" {
			t.Errorf("%s ended with %q (%s), want a clean finish", id, end.GetErrorCode(), end.GetError())
		}
	}
	s.await(t, "a heartbeat", isHeartbeat)
}

// Close ends the loop itself, not just the current stream: before, it
// closed the connection and then waited for a Run that simply dialled
// again.
func TestRunCloseStopsTheLoop(t *testing.T) {
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod")}, nil, &waitRecorder{})
	_, finished, result := runInBackground(t, r)
	cluster.next(t)
	awaitRegistered(t, r)

	closed := make(chan struct{})
	go func() {
		r.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	<-finished
	if err := result(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v after Close, want context.Canceled", err)
	}
	if got := cluster.dialCount(); got != 1 {
		t.Fatalf("the runner dialled %d times, want 1: a closed runner does not reconnect", got)
	}
}

// Close on a runner that never ran returns at once.
func TestCloseOnARunnerThatNeverRan(t *testing.T) {
	r, err := NewRunner(Options{Config: testHomeConfig(t, "prod")})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close on a runner that never ran must not wait for it")
	}
}

// toolDispatch is the cluster sending one workerHost call.
func toolDispatch(callID string, timeout time.Duration) *memqlv1.WorkerServerMessage {
	return &memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_ToolDispatch{ToolDispatch: &memqlv1.ToolDispatch{
		CallId: callID, Tool: "workerHost", Action: "pipeline_step", Timeout: durationpb.New(timeout),
	}}}
}

func resultFor(callID string) func(*memqlv1.WorkerClientMessage) bool {
	return func(m *memqlv1.WorkerClientMessage) bool { return m.GetToolResult().GetCallId() == callID }
}

// streamingTools writes two chunks of output for every call, then answers.
// Dispatch -- the path with nowhere to put output -- answers with a failure
// a test can recognise, so a runner that took it instead of DispatchStream
// shows.
type streamingTools struct{}

func (streamingTools) Dispatch(context.Context, *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure) {
	return nil, &memqlv1.Failure{ErrorCode: "not_streamed"}
}

func (streamingTools) DispatchStream(_ context.Context, d *memqlv1.ToolDispatch, send func(*memqlv1.ToolStream) error) (*memqlv1.Success, *memqlv1.Failure) {
	_ = send(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StdoutChunk{StdoutChunk: []byte("one\n")}})
	_ = send(&memqlv1.ToolStream{CallId: d.GetCallId(), Payload: &memqlv1.ToolStream_StderrChunk{StderrChunk: []byte("two\n")}})
	return &memqlv1.Success{ResultJson: []byte(`{"exitCode":0}`)}, nil
}

// TestRunStreamsAToolCallsOutputBeforeItsResult (memql#5494). The engine
// relays a ToolStream only while its call is pending: a chunk that arrives
// after the ToolResult has no sink and is dropped. So a call's output is on
// the wire, in order, before its result.
func TestRunStreamsAToolCallsOutputBeforeItsResult(t *testing.T) {
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod"), Tools: streamingTools{}}, nil, &waitRecorder{})
	runInBackground(t, r)
	s := cluster.next(t)
	awaitRegistered(t, r)

	s.say(toolDispatch("call-s", time.Minute))
	if res := s.await(t, "the result", resultFor("call-s")).GetToolResult(); res.GetFailure() != nil {
		t.Fatalf("the call went through Dispatch, which has nowhere to put output: %+v", res.GetFailure())
	}
	var order []string
	for _, m := range s.messages() {
		if c := m.GetToolStream(); c != nil && c.GetCallId() == "call-s" {
			order = append(order, "chunk:"+string(c.GetStdoutChunk())+string(c.GetStderrChunk()))
		}
		if m.GetToolResult().GetCallId() == "call-s" {
			order = append(order, "result")
		}
	}
	if got, want := strings.Join(order, " | "), "chunk:one\n | chunk:two\n | result"; got != want {
		t.Fatalf("frames for the call arrived as %q, want %q", got, want)
	}
}

// heldTools holds every call until its context ends, then answers with
// how it ended.
type heldTools struct{ started chan string }

func (h *heldTools) Dispatch(ctx context.Context, d *memqlv1.ToolDispatch) (*memqlv1.Success, *memqlv1.Failure) {
	h.started <- d.GetCallId()
	<-ctx.Done()
	return nil, &memqlv1.Failure{ErrorCode: "stopped", ErrorMessage: ctx.Err().Error()}
}

func (h *heldTools) awaitStart(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.started:
		if got != want {
			t.Fatalf("call %q started, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("call %q never started", want)
	}
}

// TestRunAToolCancelEndsTheCallItNames (memql#5494). The engine sends
// ToolCancel when it stops waiting for a call -- a pipeline run cancelled, a
// deadline on its side -- and a step left running would spend up to an hour
// of this machine on output nobody will read. The cancel ends that call and
// no other.
func TestRunAToolCancelEndsTheCallItNames(t *testing.T) {
	held := &heldTools{started: make(chan string, 4)}
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod"), Tools: held}, nil, &waitRecorder{})
	runInBackground(t, r)
	s := cluster.next(t)
	awaitRegistered(t, r)

	s.say(toolDispatch("call-a", time.Hour))
	held.awaitStart(t, "call-a")
	s.say(toolDispatch("call-b", time.Hour))
	held.awaitStart(t, "call-b")

	s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_ToolCancel{ToolCancel: &memqlv1.ToolCancel{
		CallId: "call-a", Reason: "context_cancelled",
	}}})
	res := s.await(t, "the cancelled call's result", resultFor("call-a")).GetToolResult()
	if res.GetFailure().GetErrorMessage() != context.Canceled.Error() {
		t.Fatalf("call-a ended with %+v, want its context cancelled", res)
	}
	time.Sleep(50 * time.Millisecond)
	for _, m := range s.messages() {
		if m.GetToolResult().GetCallId() == "call-b" {
			t.Fatal("cancelling call-a ended call-b too")
		}
	}
}

// TestRunAStreamDropEndsTheToolCallsInFlight (memql#5494). A call whose
// stream is gone has nobody waiting for its result -- the engine has already
// answered worker_disconnected -- and the runner does not reconnect until
// its calls have returned. A pipeline step left running would therefore keep
// this machine off the cluster for as long as the step's own timeout.
func TestRunAStreamDropEndsTheToolCallsInFlight(t *testing.T) {
	held := &heldTools{started: make(chan string, 4)}
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod"), Tools: held}, nil, &waitRecorder{})
	runInBackground(t, r)
	first := cluster.next(t)
	awaitRegistered(t, r)

	first.say(toolDispatch("call-long", time.Hour))
	held.awaitStart(t, "call-long")
	first.drop(status.Error(codes.Unavailable, "transport is closing"))

	second := cluster.next(t)
	second.await(t, "a Register on the new stream", func(m *memqlv1.WorkerClientMessage) bool { return m.GetRegister() != nil })
}

// TestRunAGrantedPipelinesPolicyReRegistersWithTheLabel (memql#5494). The
// SIGHUP path: the owner sets pipelines.allow, the policy reloads, and the
// next Register carries pipelines=allowed -- the only way the cluster's
// router can learn it.
func TestRunAGrantedPipelinesPolicyReRegistersWithTheLabel(t *testing.T) {
	var allowed atomic.Bool
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "prod"), PipelinesPolicy: func() tools.PipelinesPolicy { return tools.PipelinesPolicy{Allow: allowed.Load()} }}, nil, &waitRecorder{})
	runInBackground(t, r)
	first := cluster.next(t)
	awaitRegistered(t, r)
	if v, ok := first.register().GetLabels()["pipelines"]; ok {
		t.Fatalf("a machine whose policy allows no pipelines registered pipelines=%q", v)
	}

	allowed.Store(true)
	r.RequestImmediateReadvertise()
	second := cluster.next(t)
	reg := second.await(t, "a Register on the new stream", func(m *memqlv1.WorkerClientMessage) bool { return m.GetRegister() != nil }).GetRegister()
	if got := reg.GetLabels()["pipelines"]; got != "allowed" {
		t.Fatalf("the re-Register carried pipelines=%q, want \"allowed\"", got)
	}
}
