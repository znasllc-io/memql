package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// newTestModelCall builds a handle with no stream behind it. The
// delta discipline and the two ceilings are properties of the handle,
// so they are testable without a gRPC server -- which is the point:
// the rule that protects the transcript should not need a cluster to
// demonstrate.
func newTestModelCall(t *testing.T, limits ModelCallLimits) (*ModelCallHandle, *[]*memqlv1.ModelCallCancel) {
	t.Helper()
	var sent []*memqlv1.ModelCallCancel
	h := &ModelCallHandle{
		requestId:    "req-1",
		limits:       limits.withDefaults(),
		deltas:       make(chan ModelCallDelta, modelDeltaBuffer),
		done:         make(chan struct{}),
		clock:        time.Now,
		lastActivity: time.Now(),
		cancelFn: func(c *memqlv1.ModelCallCancel) error {
			sent = append(sent, c)
			return nil
		},
	}
	return h, &sent
}

func drain(h *ModelCallHandle) []ModelCallDelta {
	var out []ModelCallDelta
	for {
		select {
		case d, ok := <-h.Deltas():
			if !ok {
				return out
			}
			out = append(out, d)
		default:
			return out
		}
	}
}

// A generation is a record. An out-of-order delta must be DROPPED
// rather than appended: splicing it back into the middle produces text
// no later reader can tell is wrong.
func TestModelCallDropsOutOfOrderDeltas(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{})

	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "a"})
	h.deliverDelta(ModelCallDelta{Seq: 2, Content: "b"})
	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "STALE"})
	h.deliverDelta(ModelCallDelta{Seq: 3, Content: "c"})

	got := drain(h)
	if len(got) != 3 {
		t.Fatalf("expected 3 accepted deltas, got %d: %+v", len(got), got)
	}
	for i, want := range []string{"a", "b", "c"} {
		if got[i].Content != want {
			t.Fatalf("delta %d = %q, want %q", i, got[i].Content, want)
		}
	}

	h.finish(ModelCallOutcome{FinishReason: ModelFinishStop}, nil)
	out, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// The assembled text must be the ACCEPTED set. A stale delta that
	// was kept out of the channel but folded into the assembly would
	// corrupt the answer through a second path.
	if out.Content != "abc" {
		t.Fatalf("assembled content = %q, want %q", out.Content, "abc")
	}
}

func TestModelCallDropsDuplicateDeltas(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{})

	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "x"})
	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "x"})
	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "x"})
	h.deliverDelta(ModelCallDelta{Seq: 2, Content: "y"})

	got := drain(h)
	if len(got) != 2 {
		t.Fatalf("expected 2 accepted deltas, got %d: %+v", len(got), got)
	}
	h.finish(ModelCallOutcome{FinishReason: ModelFinishStop}, nil)
	out, _ := h.Wait(context.Background())
	if out.Content != "xy" {
		t.Fatalf("assembled content = %q, want %q", out.Content, "xy")
	}
}

// A keepalive proves liveness without being output. It takes a seq (so
// it can never be confused with a replayed content delta) but is not
// forwarded to the consumer and contributes nothing to the text.
func TestModelCallKeepaliveIsNotOutput(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{})

	h.deliverDelta(ModelCallDelta{Seq: 1, Content: "hello"})
	h.deliverDelta(ModelCallDelta{Seq: 2, Keepalive: true})
	h.deliverDelta(ModelCallDelta{Seq: 3, Content: " world"})

	got := drain(h)
	if len(got) != 2 {
		t.Fatalf("expected keepalive to be withheld from the consumer, got %+v", got)
	}
	h.finish(ModelCallOutcome{FinishReason: ModelFinishStop}, nil)
	out, _ := h.Wait(context.Background())
	if out.Content != "hello world" {
		t.Fatalf("assembled content = %q", out.Content)
	}
}

// A worker that answers in one piece leaves the deltas empty and puts
// the whole text on End. Both shapes are supported, and the full-text
// answer must win over an empty assembly rather than being overwritten
// by it.
func TestModelCallNonStreamedContentSurvives(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{})
	h.finish(ModelCallOutcome{FinishReason: ModelFinishStop, Content: "one-shot answer"}, nil)
	out, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if out.Content != "one-shot answer" {
		t.Fatalf("content = %q", out.Content)
	}
}

// The idle ceiling is what distinguishes a slow model from a dead one.
// A machine that says nothing past it must fail with the NAMED idle
// error and be told to stop, not merely be abandoned.
func TestModelCallIdleCeilingCancelsTheMachine(t *testing.T) {
	h, sent := newTestModelCall(t, ModelCallLimits{
		Timeout:     10 * time.Second,
		IdleTimeout: 40 * time.Millisecond,
		Keepalive:   10 * time.Millisecond,
	})

	_, err := h.Wait(context.Background())
	if !errors.Is(err, ErrModelCallIdle) {
		t.Fatalf("expected the idle ceiling error, got %v", err)
	}
	if len(*sent) == 0 || (*sent)[0].GetReason() != "idle_timeout" {
		t.Fatalf("expected an idle_timeout cancel on the wire, got %+v", *sent)
	}
}

// A delta resets the idle timer. Without this the ceiling would fire on
// a perfectly healthy long generation.
func TestModelCallDeltasResetTheIdleTimer(t *testing.T) {
	h, _ := newTestModelCall(t, ModelCallLimits{
		Timeout:     10 * time.Second,
		IdleTimeout: 120 * time.Millisecond,
		Keepalive:   30 * time.Millisecond,
	})

	go func() {
		for i := uint64(1); i <= 6; i++ {
			time.Sleep(40 * time.Millisecond)
			h.deliverDelta(ModelCallDelta{Seq: i, Keepalive: true})
		}
		h.finish(ModelCallOutcome{FinishReason: ModelFinishStop, Content: "done"}, nil)
	}()

	out, err := h.Wait(context.Background())
	if err != nil {
		t.Fatalf("keepalives should have held the idle ceiling off: %v", err)
	}
	if out.Content != "done" {
		t.Fatalf("content = %q", out.Content)
	}
}

// The whole-call ceiling is separate from the idle ceiling: a machine
// streaming happily forever is still a call that has to end.
func TestModelCallWholeCallCeiling(t *testing.T) {
	h, sent := newTestModelCall(t, ModelCallLimits{
		Timeout:     60 * time.Millisecond,
		IdleTimeout: 10 * time.Second,
		Keepalive:   10 * time.Millisecond,
	})

	stop := make(chan struct{})
	go func() {
		var seq uint64
		for {
			select {
			case <-stop:
				return
			default:
				seq++
				h.deliverDelta(ModelCallDelta{Seq: seq, Keepalive: true})
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()
	defer close(stop)

	_, err := h.Wait(context.Background())
	if err == nil {
		t.Fatal("expected the whole-call ceiling to expire")
	}
	if errors.Is(err, ErrModelCallIdle) {
		t.Fatalf("a busy call must not be reported as idle: %v", err)
	}
	if len(*sent) == 0 || (*sent)[0].GetReason() != "call_timeout" {
		t.Fatalf("expected a call_timeout cancel on the wire, got %+v", *sent)
	}
}

// A caller walking away must stop the generation on the machine, or a
// laptop keeps burning tokens for work nobody is waiting for.
func TestModelCallCallerCancelStopsTheMachine(t *testing.T) {
	h, sent := newTestModelCall(t, ModelCallLimits{Timeout: 10 * time.Second, IdleTimeout: 10 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(*sent) == 0 || (*sent)[0].GetReason() != "caller_cancelled" {
		t.Fatalf("expected a caller_cancelled cancel on the wire, got %+v", *sent)
	}
}

// A keepalive at or past the idle ceiling would guarantee a false idle
// expiry on every quiet call, so it is clamped rather than honoured.
func TestModelCallLimitsClampAnImpossibleKeepalive(t *testing.T) {
	got := ModelCallLimits{Timeout: time.Minute, IdleTimeout: 20 * time.Second, Keepalive: 30 * time.Second}.withDefaults()
	if got.Keepalive >= got.IdleTimeout {
		t.Fatalf("keepalive %s must be under the idle ceiling %s", got.Keepalive, got.IdleTimeout)
	}
}

func TestModelLabelRoundTrip(t *testing.T) {
	if got := ModelLabel("llama3.1:8b"); got != "model:llama3.1:8b" {
		t.Fatalf("ModelLabel = %q", got)
	}
	// The model id itself contains a colon, which is why the label is
	// parsed by prefix rather than split on the separator.
	id, ok := ModelIdFromLabel("model:llama3.1:8b")
	if !ok || id != "llama3.1:8b" {
		t.Fatalf("ModelIdFromLabel = %q, %v", id, ok)
	}
	if _, ok := ModelIdFromLabel("runtime:ollama"); ok {
		t.Fatal("a runtime label must not parse as a model label")
	}
	if _, ok := ModelIdFromLabel("model:"); ok {
		t.Fatal("an empty model id must not parse")
	}
}
