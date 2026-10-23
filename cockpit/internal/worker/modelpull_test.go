package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
)

// recordingStream stands where the SDK's stream would be and keeps every
// frame the worker sends, so a reply's wire shape is assertable without a
// cluster. Recv answers nothing: these tests drive handleMessage by hand
// rather than through runStream.
type recordingStream struct {
	seq *sequence

	mu   sync.Mutex
	sent []*memqlv1.WorkerClientMessage
}

func (s *recordingStream) Send(m *memqlv1.WorkerClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	if s.seq != nil && m.GetModelPullEnd() != nil {
		s.seq.add("end")
	}
	return nil
}

func (s *recordingStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	return nil, errors.New("recordingStream: nothing to receive")
}

func (s *recordingStream) Close() {}

func (s *recordingStream) messages() []*memqlv1.WorkerClientMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*memqlv1.WorkerClientMessage(nil), s.sent...)
}

func (s *recordingStream) progress() []*memqlv1.ModelPullProgress {
	var out []*memqlv1.ModelPullProgress
	for _, m := range s.messages() {
		if p := m.GetModelPullProgress(); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// end is the first ModelPullEnd sent, or nil while none has been.
func (s *recordingStream) end() *memqlv1.ModelPullEnd {
	for _, m := range s.messages() {
		if e := m.GetModelPullEnd(); e != nil {
			return e
		}
	}
	return nil
}

func (s *recordingStream) ends() int {
	n := 0
	for _, m := range s.messages() {
		if m.GetModelPullEnd() != nil {
			n++
		}
	}
	return n
}

// sequence records the order the seams ran in. The order IS the
// contract: allow before reload before end before re-advertise, and a
// test that only counted calls would pass with the reconnect racing the
// End it is meant to follow.
type sequence struct {
	mu    sync.Mutex
	steps []string
}

func (q *sequence) add(s string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.steps = append(q.steps, s)
}

func (q *sequence) get() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.steps...)
}

// fakePull scripts inference.Pull: it plays its events, then returns
// err -- or, when block is set, holds the pull open until its context
// ends, which is what a multi-gigabyte download looks like to a cancel.
type fakePull struct {
	seq    *sequence
	events []inference.Progress
	err    error
	block  bool

	mu    sync.Mutex
	calls []string // base + "|" + model
}

func (f *fakePull) pull(ctx context.Context, base, model string, on func(inference.Progress)) error {
	f.mu.Lock()
	f.calls = append(f.calls, base+"|"+model)
	f.mu.Unlock()
	if f.seq != nil {
		f.seq.add("pull")
	}
	for _, ev := range f.events {
		on(ev)
	}
	if f.block {
		<-ctx.Done()
		return fmt.Errorf("pulling %s was cancelled: %w", model, ctx.Err())
	}
	return f.err
}

func (f *fakePull) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fakeAllow struct {
	seq *sequence
	err error

	mu    sync.Mutex
	calls []string // path + "|" + ids joined
}

func (f *fakeAllow) allow(policyPath string, ids ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, policyPath+"|"+strings.Join(ids, ","))
	f.mu.Unlock()
	if f.seq != nil {
		f.seq.add("allow")
	}
	return f.err
}

func (f *fakeAllow) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// pullHarness is one runner with the pull arm wired to fakes and a
// recorder where the stream would be.
type pullHarness struct {
	r     *Runner
	conn  *Connection
	rec   *recordingStream
	inv   *fakeModelInventory
	pull  *fakePull
	allow *fakeAllow
	seq   *sequence

	mu      sync.Mutex
	reloads int
}

const (
	testPolicyPath = "/home/somebody/.memql/policy.yaml"
	testOllamaBase = "http://127.0.0.1:11434"
)

func newPullHarness(pull *fakePull, allow *fakeAllow, pullAllowed bool) *pullHarness {
	seq := &sequence{}
	pull.seq = seq
	allow.seq = seq
	clock := time.Unix(1_700_000_000, 0)
	inv := &fakeModelInventory{}
	inv.serve(oneModel())
	h := &pullHarness{
		inv:   inv,
		pull:  pull,
		allow: allow,
		seq:   seq,
		rec:   &recordingStream{seq: seq},
	}
	h.r = testRunner(inv, &clock)
	h.r.pulls = newModelPuller(h.r.logger, &ModelPullOptions{
		PolicyPath:  testPolicyPath,
		OllamaBase:  func() string { return testOllamaBase },
		PullAllowed: func() bool { return pullAllowed },
		ReloadPolicy: func() error {
			h.mu.Lock()
			h.reloads++
			h.mu.Unlock()
			seq.add("reload")
			return nil
		},
		Pull:  pull.pull,
		Allow: allow.allow,
	}, func() {
		seq.add("readvertise")
		h.r.RequestImmediateReadvertise()
	})
	// The settle exists for a real transport; a recorder has nothing to
	// flush, and five hundred milliseconds per case is what makes a
	// suite slow enough that nobody runs it.
	h.r.pulls.settle = 0
	h.conn = &Connection{conn: h.rec}
	return h
}

func (h *pullHarness) reloaded() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reloads
}

func (h *pullHarness) start(t *testing.T, requestID, model string) {
	t.Helper()
	if err := h.r.handleMessage(context.Background(), h.conn, &memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelPullStart{ModelPullStart: &memqlv1.ModelPullStart{
			RequestId:      requestID,
			RegistrationId: "v1:worker:registration:abc",
			Model:          model,
		}},
	}); err != nil {
		t.Fatalf("handleMessage(ModelPullStart): %v", err)
	}
}

func (h *pullHarness) cancel(t *testing.T, requestID, reason string) {
	t.Helper()
	if err := h.r.handleMessage(context.Background(), h.conn, &memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_ModelPullCancel{ModelPullCancel: &memqlv1.ModelPullCancel{
			RequestId: requestID,
			Reason:    reason,
		}},
	}); err != nil {
		t.Fatalf("handleMessage(ModelPullCancel): %v", err)
	}
}

// waitFor polls a condition to a deadline. The pull runs on its own
// goroutine and reports over the recorder, so every assertion about its
// end is an assertion about something that has not happened yet.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readvertiseRequested(r *Runner) bool {
	select {
	case <-r.readvertiseNow:
		return true
	default:
		return false
	}
}

// -----------------------------------------------------------------------------
// The Pong (D11)
// -----------------------------------------------------------------------------

// A Ping produces exactly one Pong, carrying the same request id and the
// Ping's own sent_at echoed verbatim. The agent measures the round trip
// against its own clock; the echo is so a reader of the wire can pair the
// two, and a cockpit that rewrote it would be inventing a figure.
func TestHandleMessage_PingIsAnsweredWithOnePong(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	r := testRunner(&fakeModelInventory{}, &clock)
	rec := &recordingStream{}
	conn := &Connection{conn: rec}

	sentAt := timestamppb.New(time.Unix(1_700_000_123, 456_789))
	err := r.handleMessage(context.Background(), conn, &memqlv1.WorkerServerMessage{
		MessageId: "ping-7",
		Payload:   &memqlv1.WorkerServerMessage_Ping{Ping: &memqlv1.Ping{RequestId: "ping-7", SentAt: sentAt}},
	})
	if err != nil {
		t.Fatalf("handleMessage(Ping): %v", err)
	}

	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("a Ping must produce exactly one frame, got %d", len(msgs))
	}
	pong := msgs[0].GetPong()
	if pong == nil {
		t.Fatalf("the one frame must be a Pong, got %T", msgs[0].GetPayload())
	}
	if pong.GetRequestId() != "ping-7" {
		t.Errorf("pong request_id = %q, want %q", pong.GetRequestId(), "ping-7")
	}
	if !proto.Equal(pong.GetSentAt(), sentAt) {
		t.Errorf("pong sent_at = %v, want the Ping's own %v echoed verbatim", pong.GetSentAt(), sentAt)
	}
	if pong.GetReceivedAt() == nil {
		t.Error("pong received_at must carry this machine's clock")
	}
}

// -----------------------------------------------------------------------------
// The pull (D13)
// -----------------------------------------------------------------------------

func llamaEvents() []inference.Progress {
	const m = "llama3.1:8b"
	return []inference.Progress{
		{Model: m, Status: "pulling manifest"},
		{Model: m, Status: "pulling 8934d96d3f08", Total: 4_000_000},
		{Model: m, Status: "pulling 8934d96d3f08", Completed: 1_500_000, Total: 4_000_000},
		{Model: m, Status: "pulling 8934d96d3f08", Completed: 4_000_000, Total: 4_000_000},
		{Model: m, Status: "verifying sha256 digest"},
		{Model: m, Status: "success"},
	}
}

// The whole path, in the order the CLI runs it: pull, allow, reload,
// End(ok), re-advertise -- with every progress frame forwarded in order
// and the End sent BEFORE the re-advertise is requested.
func TestModelPull_ForwardsProgressAllowsReloadsAndReadvertises(t *testing.T) {
	h := newPullHarness(&fakePull{events: llamaEvents()}, &fakeAllow{}, true)
	h.start(t, "req-1", "llama3.1:8b")

	waitFor(t, "the re-advertise request", func() bool {
		steps := h.seq.get()
		return len(steps) > 0 && steps[len(steps)-1] == "readvertise"
	})

	wantSeq := []string{"pull", "allow", "reload", "end", "readvertise"}
	if got := h.seq.get(); strings.Join(got, " ") != strings.Join(wantSeq, " ") {
		t.Fatalf("sequence = %v, want %v", got, wantSeq)
	}

	// The pull ran against the discoverer's base, for the model verbatim.
	if got := h.pull.called(); len(got) != 1 || got[0] != testOllamaBase+"|llama3.1:8b" {
		t.Errorf("pull calls = %v, want one against %s for llama3.1:8b", got, testOllamaBase)
	}

	// Every observation, in order, with the layer derived from the status.
	got := h.rec.progress()
	events := llamaEvents()
	if len(got) != len(events) {
		t.Fatalf("progress frames = %d, want %d (every observation is forwarded)", len(got), len(events))
	}
	wantLayer := []string{"", "8934d96d3f08", "8934d96d3f08", "8934d96d3f08", "", ""}
	for i, p := range got {
		ev := events[i]
		if p.GetRequestId() != "req-1" || p.GetModel() != "llama3.1:8b" {
			t.Errorf("frame %d: request_id/model = %q/%q", i, p.GetRequestId(), p.GetModel())
		}
		if p.GetStatus() != ev.Status || p.GetCompletedBytes() != ev.Completed || p.GetTotalBytes() != ev.Total {
			t.Errorf("frame %d = %q %d/%d, want %q %d/%d", i,
				p.GetStatus(), p.GetCompletedBytes(), p.GetTotalBytes(), ev.Status, ev.Completed, ev.Total)
		}
		if p.GetLayer() != wantLayer[i] {
			t.Errorf("frame %d layer = %q, want %q", i, p.GetLayer(), wantLayer[i])
		}
	}

	end := h.rec.end()
	if end == nil || !end.GetOk() || end.GetError() != "" {
		t.Fatalf("end = %v, want ok with no error", end)
	}
	if end.GetRequestId() != "req-1" || end.GetModel() != "llama3.1:8b" {
		t.Errorf("end request_id/model = %q/%q", end.GetRequestId(), end.GetModel())
	}
	if !end.GetReadvertised() {
		t.Error("end.readvertised must be true: the request is armed and survives the busy guard")
	}
	if n := h.rec.ends(); n != 1 {
		t.Errorf("ends = %d, want exactly one", n)
	}

	// Allow got the policy path and the model, once.
	if got := h.allow.called(); len(got) != 1 || got[0] != testPolicyPath+"|llama3.1:8b" {
		t.Errorf("allow calls = %v, want one for llama3.1:8b in %s", got, testPolicyPath)
	}
	if h.reloaded() != 1 {
		t.Errorf("policy reloads = %d, want 1", h.reloaded())
	}

	// The re-advertise is the real one: cache dropped, loop woken.
	if h.inv.invalidated() != 1 {
		t.Errorf("inventory invalidations = %d, want 1", h.inv.invalidated())
	}
	if !readvertiseRequested(h.r) {
		t.Error("the heartbeat loop must be woken rather than left to the refresh ticker")
	}
	waitFor(t, "the pull to release", func() bool { return h.r.pulls.Live() == 0 })
}

// A pull the runtime refuses ends ok=false with the runtime's own words,
// allows nothing and re-advertises nothing: a model that is not on the
// disk must not be put in front of the router.
func TestModelPull_AFailedPullEndsWithTheRuntimesOwnWords(t *testing.T) {
	runtimeErr := fmt.Errorf("pulling nosuch:1b: %w: pull model manifest: file does not exist (its last status was \"pulling manifest\")",
		inference.ErrPullFailed)
	h := newPullHarness(&fakePull{
		events: []inference.Progress{{Model: "nosuch:1b", Status: "pulling manifest"}},
		err:    runtimeErr,
	}, &fakeAllow{}, true)
	h.start(t, "req-2", "nosuch:1b")

	waitFor(t, "the end", func() bool { return h.rec.end() != nil })
	end := h.rec.end()
	if end.GetOk() {
		t.Fatal("a failed pull must not end ok")
	}
	if end.GetError() != runtimeErr.Error() {
		t.Errorf("end.error = %q, want the runtime's own text %q", end.GetError(), runtimeErr.Error())
	}
	if end.GetReadvertised() {
		t.Error("nothing was re-advertised")
	}
	if len(h.allow.called()) != 0 {
		t.Errorf("allow must not run after a failed pull, got %v", h.allow.called())
	}
	if h.reloaded() != 0 || h.inv.invalidated() != 0 || readvertiseRequested(h.r) {
		t.Error("a failed pull must reload nothing and re-advertise nothing")
	}
	if got := h.seq.get(); strings.Join(got, " ") != "pull end" {
		t.Errorf("sequence = %v, want [pull end]", got)
	}
	waitFor(t, "the pull to release", func() bool { return h.r.pulls.Live() == 0 })
}

// A cancel mid-pull ends ok=false carrying the cancel reason -- the reason
// wins over the runtime's "context canceled", because the End is what the
// person who cancelled reads.
func TestModelPull_CancelMidPullEndsWithTheReason(t *testing.T) {
	pull := &fakePull{events: llamaEvents()[:2], block: true}
	h := newPullHarness(pull, &fakeAllow{}, true)
	h.start(t, "req-3", "llama3.1:8b")

	waitFor(t, "the pull to start", func() bool { return len(pull.called()) == 1 })
	if h.r.pulls.Live() != 1 {
		t.Fatalf("live pulls = %d, want 1", h.r.pulls.Live())
	}
	h.cancel(t, "req-3", "the person closed the page")

	waitFor(t, "the end", func() bool { return h.rec.end() != nil })
	end := h.rec.end()
	if end.GetOk() {
		t.Fatal("a cancelled pull must not end ok")
	}
	if want := "the cluster cancelled this pull: the person closed the page"; end.GetError() != want {
		t.Errorf("end.error = %q, want %q", end.GetError(), want)
	}
	if len(h.allow.called()) != 0 {
		t.Errorf("allow must not run after a cancel, got %v", h.allow.called())
	}
	if readvertiseRequested(h.r) {
		t.Error("a cancelled pull re-advertises nothing")
	}
	waitFor(t, "the pull to release", func() bool { return h.r.pulls.Live() == 0 })

	// A cancel for a pull that already ended is ignored, not answered: a
	// second End for one request would be noise the engine drops anyway.
	h.cancel(t, "req-3", "again")
	if n := h.rec.ends(); n != 1 {
		t.Errorf("ends after a late cancel = %d, want 1", n)
	}
}

// A build with no local-model support answers at once, synchronously,
// in a sentence, and never calls Pull. Two shapes reach that: a runner
// built with no pull seam at all, and one whose seam is present but whose
// model inventory is nil -- a pull onto a machine that advertises nothing
// would fetch gigabytes the cluster is never told about.
func TestModelPull_ABuildWithNoRuntimeRefusesAtOnce(t *testing.T) {
	t.Run("no seam", func(t *testing.T) {
		clock := time.Unix(1_700_000_000, 0)
		r := testRunner(&fakeModelInventory{}, &clock) // pulls is nil
		rec := &recordingStream{}
		conn := &Connection{conn: rec}
		err := r.handleMessage(context.Background(), conn, &memqlv1.WorkerServerMessage{
			Payload: &memqlv1.WorkerServerMessage_ModelPullStart{ModelPullStart: &memqlv1.ModelPullStart{
				RequestId: "req-4", Model: "llama3.1:8b",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		end := rec.end()
		if end == nil {
			t.Fatal("the refusal must be sent before handleMessage returns")
		}
		if end.GetOk() || end.GetError() != modelPullUnsupported || end.GetRequestId() != "req-4" {
			t.Errorf("end = %v, want ok=false with %q for req-4", end, modelPullUnsupported)
		}
	})

	t.Run("seam without an inventory", func(t *testing.T) {
		r, err := NewRunner(Options{
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Config: Config{
				ClusterURL: "https://api.example.com", Token: "mql_wkr_x", Name: "w",
				Capabilities: []string{"HEADLESS"},
			},
			ModelPull: &ModelPullOptions{
				Pull: func(context.Context, string, string, func(inference.Progress)) error {
					t.Error("Pull must not be called by a build that reports no models")
					return nil
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		rec := &recordingStream{}
		conn := &Connection{conn: rec}
		if err := r.handleMessage(context.Background(), conn, &memqlv1.WorkerServerMessage{
			Payload: &memqlv1.WorkerServerMessage_ModelPullStart{ModelPullStart: &memqlv1.ModelPullStart{
				RequestId: "req-5", Model: "llama3.1:8b",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		end := rec.end()
		if end == nil || end.GetOk() || end.GetError() != modelPullUnsupported {
			t.Errorf("end = %v, want ok=false with %q", end, modelPullUnsupported)
		}
	})
}

// models.pull: false is the owner's switch, and the cluster's ask does
// not override it. The sentence is the CLI's, naming the file.
func TestModelPull_ThePullSwitchIsHonoured(t *testing.T) {
	pull := &fakePull{}
	h := newPullHarness(pull, &fakeAllow{}, false)
	h.start(t, "req-6", "llama3.1:8b")

	end := h.rec.end()
	if end == nil {
		t.Fatal("the refusal must be sent before handleMessage returns")
	}
	if end.GetOk() {
		t.Fatal("a refused pull must not end ok")
	}
	if !strings.Contains(end.GetError(), "models.pull is false in "+testPolicyPath) {
		t.Errorf("end.error = %q, want it to name models.pull and %s", end.GetError(), testPolicyPath)
	}
	if len(pull.called()) != 0 {
		t.Error("Pull must not run when the switch is off")
	}
}

// A pull that landed and could not be allowed is NOT a success: the model
// is on disk and invisible to the fleet, which is exactly the outcome
// ok=true would misreport. Nothing is re-advertised.
func TestModelPull_AllowFailureIsNotSuccess(t *testing.T) {
	allowErr := fmt.Errorf("%w (around line 3): add llama3.1:8b to models.allow by hand", inference.ErrPolicyNotEditable)
	h := newPullHarness(&fakePull{events: llamaEvents()}, &fakeAllow{err: allowErr}, true)
	h.start(t, "req-7", "llama3.1:8b")

	waitFor(t, "the end", func() bool { return h.rec.end() != nil })
	end := h.rec.end()
	if end.GetOk() || end.GetReadvertised() {
		t.Fatalf("end = %v, want ok=false and readvertised=false", end)
	}
	if want := "the model was pulled and " + allowErr.Error(); end.GetError() != want {
		t.Errorf("end.error = %q, want %q", end.GetError(), want)
	}
	if h.reloaded() != 0 || readvertiseRequested(h.r) || h.inv.invalidated() != 0 {
		t.Error("a pull that could not be allowed must reload nothing and re-advertise nothing")
	}
}

// A live pull counts as busy -- a sibling's re-advertise must not close
// the stream under it -- and losing the stream stops it by name.
func TestModelPull_ALivePullIsBusyAndStreamLossStopsIt(t *testing.T) {
	pull := &fakePull{block: true}
	h := newPullHarness(pull, &fakeAllow{}, true)
	if h.r.busy() {
		t.Fatal("an idle runner is not busy")
	}
	h.start(t, "req-8", "llama3.1:8b")
	waitFor(t, "the pull to start", func() bool { return len(pull.called()) == 1 })
	if !h.r.busy() {
		t.Fatal("a running pull must make the worker busy")
	}
	if h.r.maybeReadvertiseModels(context.Background(), h.conn) {
		t.Fatal("a re-advertise must not close the stream under a running pull")
	}

	h.r.pulls.StopAll("the worker's stream to the cluster was lost")
	waitFor(t, "the end", func() bool { return h.rec.end() != nil })
	end := h.rec.end()
	if end.GetOk() {
		t.Fatal("a stopped pull must not end ok")
	}
	if want := "this pull was stopped: the worker's stream to the cluster was lost"; end.GetError() != want {
		t.Errorf("end.error = %q, want %q", end.GetError(), want)
	}
	waitFor(t, "the pull to release", func() bool { return !h.r.busy() })
}

// Drain stops a pull the same way, and the End goes out on the stream
// that is still up, so the cluster learns why rather than seeing a
// disconnect.
func TestModelPull_DrainStopsAPullByName(t *testing.T) {
	pull := &fakePull{block: true}
	h := newPullHarness(pull, &fakeAllow{}, true)
	h.start(t, "req-9", "llama3.1:8b")
	waitFor(t, "the pull to start", func() bool { return len(pull.called()) == 1 })

	err := h.r.handleMessage(context.Background(), h.conn, &memqlv1.WorkerServerMessage{
		Payload: &memqlv1.WorkerServerMessage_Drain{Drain: &memqlv1.Drain{}},
	})
	if err == nil {
		t.Fatal("drain must end the stream")
	}
	waitFor(t, "the end", func() bool { return h.rec.end() != nil })
	if end := h.rec.end(); end.GetOk() || !strings.Contains(end.GetError(), "drain") {
		t.Errorf("end = %v, want ok=false naming the drain", end)
	}
}

// A second Start for a request id already running is refused in a
// sentence and does not touch the first.
func TestModelPull_ADuplicateRequestIdIsRefused(t *testing.T) {
	pull := &fakePull{block: true}
	h := newPullHarness(pull, &fakeAllow{}, true)
	h.start(t, "req-10", "llama3.1:8b")
	waitFor(t, "the pull to start", func() bool { return len(pull.called()) == 1 })

	h.start(t, "req-10", "llama3.1:8b")
	end := h.rec.end()
	if end == nil || end.GetOk() || !strings.Contains(end.GetError(), "already running") {
		t.Fatalf("end = %v, want an immediate ok=false naming the duplicate", end)
	}
	if len(pull.called()) != 1 || h.r.pulls.Live() != 1 {
		t.Error("the duplicate must not start a second pull or disturb the first")
	}

	h.cancel(t, "req-10", "done")
	waitFor(t, "the first pull to end", func() bool { return h.rec.ends() == 2 })
}

// A blank model is refused before anything runs.
func TestModelPull_ABlankModelIsRefused(t *testing.T) {
	pull := &fakePull{}
	h := newPullHarness(pull, &fakeAllow{}, true)
	h.start(t, "req-11", "   ")
	end := h.rec.end()
	if end == nil || end.GetOk() || !strings.Contains(end.GetError(), "no model was named") {
		t.Fatalf("end = %v, want an immediate refusal naming the blank model", end)
	}
	if len(pull.called()) != 0 {
		t.Error("Pull must not run for a blank model")
	}
}

// The layer is the digest the status names, and nothing else.
func TestPullLayer(t *testing.T) {
	for status, want := range map[string]string{
		"pulling manifest":           "",
		"pulling 8934d96d3f08":       "8934d96d3f08",
		"verifying sha256 digest":    "",
		"writing manifest":           "",
		"success":                    "",
		"removing any unused layers": "",
	} {
		if got := pullLayer(status); got != want {
			t.Errorf("pullLayer(%q) = %q, want %q", status, got, want)
		}
	}
}
