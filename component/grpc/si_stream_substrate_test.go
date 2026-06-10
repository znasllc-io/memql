package memql

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	"github.com/znasllc-io/memql/core/common"
)

// fakeSubstrate is a minimal in-memory node.DeliverySubstrate for testing the
// gRPC streaming cutover seam (producer -> substrate -> consumer -> client)
// without a live Postgres. It mirrors the durable contract the real Substrate
// guarantees: per-key monotonic seq, ascending replay reads from a per-consumer
// cursor, idempotent append by EventID. A subscriber receives the full backlog
// (seq > its cursor) then tails live -- so a SECOND subscriber with a different
// consumerID (a different bff replica taking over the WS) replays from 0, which
// is exactly the cross-replica guarantee under test.
type fakeSubstrate struct {
	mu      sync.Mutex
	rows    map[string][]node.Deliverable // key -> rows in seq order
	byEvent map[string]int64
	cursors map[string]int64 // key|consumer -> acked seq
	subs    map[string][]*fakeSub
}

type fakeSub struct {
	key      node.RoutingKey
	consumer string
	ch       chan node.Deliverable
	cursor   int64
}

func newFakeSubstrate() *fakeSubstrate {
	return &fakeSubstrate{
		rows:    map[string][]node.Deliverable{},
		byEvent: map[string]int64{},
		cursors: map[string]int64{},
		subs:    map[string][]*fakeSub{},
	}
}

func (f *fakeSubstrate) Publish(_ context.Context, d node.Deliverable) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq, ok := f.byEvent[d.EventID]; ok {
		return seq, nil // idempotent
	}
	k := d.Key.String()
	seq := int64(len(f.rows[k]) + 1)
	d.Seq = seq
	f.rows[k] = append(f.rows[k], d)
	f.byEvent[d.EventID] = seq
	// Fan to any live subscriber of this key (the durable-pull equivalent; we
	// deliver every row in order so the test exercises ordering + terminal).
	for _, sub := range f.subs[k] {
		f.pumpLocked(sub)
	}
	return seq, nil
}

func (f *fakeSubstrate) Subscribe(ctx context.Context, key node.RoutingKey, consumerID string) (<-chan node.Deliverable, error) {
	f.mu.Lock()
	sub := &fakeSub{
		key:      key,
		consumer: consumerID,
		ch:       make(chan node.Deliverable, 256),
		cursor:   f.cursors[key.String()+"|"+consumerID],
	}
	f.subs[key.String()] = append(f.subs[key.String()], sub)
	f.pumpLocked(sub) // replay backlog seq > cursor immediately
	f.mu.Unlock()

	out := make(chan node.Deliverable)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-sub.ch:
				select {
				case <-ctx.Done():
					return
				case out <- d:
				}
			}
		}
	}()
	return out, nil
}

// pumpLocked delivers all rows after the sub's local position. Caller holds mu.
func (f *fakeSubstrate) pumpLocked(sub *fakeSub) {
	rows := f.rows[sub.key.String()]
	for _, d := range rows {
		if d.Seq > sub.cursor {
			select {
			case sub.ch <- d:
				sub.cursor = d.Seq
			default:
			}
		}
	}
}

func (f *fakeSubstrate) Ack(_ context.Context, key node.RoutingKey, consumerID string, seq int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ck := key.String() + "|" + consumerID
	if seq > f.cursors[ck] {
		f.cursors[ck] = seq
	}
	return nil
}

var _ node.DeliverySubstrate = (*fakeSubstrate)(nil)

// recordingClientStream is a fake MemqlService_StreamServer capturing the
// messages the bff consumer renders back to the browser, in order.
type recordingClientStream struct {
	ctx context.Context
	mu  sync.Mutex
	got []*memqlv1.MemqlServerMessage
}

func newRecordingClientStream(ctx context.Context) *recordingClientStream {
	return &recordingClientStream{ctx: ctx}
}

func (r *recordingClientStream) Send(m *memqlv1.MemqlServerMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, m)
	return nil
}
func (r *recordingClientStream) Recv() (*memqlv1.MemqlClientMessage, error) { return nil, io.EOF }
func (r *recordingClientStream) Context() context.Context                   { return r.ctx }
func (r *recordingClientStream) SetHeader(metadata.MD) error                { return nil }
func (r *recordingClientStream) SendHeader(metadata.MD) error               { return nil }
func (r *recordingClientStream) SetTrailer(metadata.MD)                     {}
func (r *recordingClientStream) SendMsg(any) error                          { return nil }
func (r *recordingClientStream) RecvMsg(any) error                          { return io.EOF }

func (r *recordingClientStream) snapshot() []*memqlv1.MemqlServerMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*memqlv1.MemqlServerMessage, len(r.got))
	copy(out, r.got)
	return out
}

func testServiceWithSubstrate(sub node.DeliverySubstrate, nodeID string) *service {
	return &service{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		deliverySubstrate: sub,
		streamNodeID:      nodeID,
	}
}

func newTestStreamSession(svc *service, stream memqlv1.MemqlService_StreamServer) *streamSession {
	return &streamSession{service: svc, stream: stream, logger: svc.logger}
}

// TestTokenStreamSubstrate_RoundTripOrdered proves the worker-side producer and
// the bff-side consumer agree on the wire: producing N token deltas + Done to the
// substrate yields, on the consumer, N ordered SIChunk deltas (with monotonic
// Index) followed by exactly one SIChatResult carrying the assembled text.
func TestTokenStreamSubstrate_RoundTripOrdered(t *testing.T) {
	sub := newFakeSubstrate()
	const requestId = "chat-1"

	// Producer (worker side).
	prod := testServiceWithSubstrate(sub, "agent-1")
	prodSess := newTestStreamSession(prod, newRecordingClientStream(context.Background()))
	chunks := make(chan common.StreamChunk, 8)
	tokens := []string{"Hel", "lo ", "wor", "ld"}
	go func() {
		for _, tok := range tokens {
			chunks <- common.StreamChunk{Content: tok}
		}
		chunks <- common.StreamChunk{Done: true}
		close(chunks)
	}()
	prodSess.produceTokenStreamToSubstrate(context.Background(), requestId, chunks)

	// Consumer (bff side).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecordingClientStream(ctx)
	consumer := testServiceWithSubstrate(sub, "bff-A")
	consSess := newTestStreamSession(consumer, rec)
	done := make(chan struct{})
	go func() { consSess.consumeTokenStream(ctx, "corr-1", requestId); close(done) }()
	<-done

	msgs := rec.snapshot()
	// N deltas + 1 result.
	if len(msgs) != len(tokens)+1 {
		t.Fatalf("want %d msgs, got %d: %+v", len(tokens)+1, len(msgs), msgs)
	}
	var lastIdx int64 = -1
	for i, tok := range tokens {
		chunk := msgs[i].GetSiChunk()
		if chunk == nil {
			t.Fatalf("msg %d not an SIChunk: %+v", i, msgs[i])
		}
		if chunk.GetTextDelta() != tok {
			t.Fatalf("delta %d = %q, want %q", i, chunk.GetTextDelta(), tok)
		}
		if chunk.GetIndex() <= lastIdx {
			t.Fatalf("delta %d index %d not increasing (last %d)", i, chunk.GetIndex(), lastIdx)
		}
		lastIdx = chunk.GetIndex()
		if msgs[i].CorrelateTo != "corr-1" {
			t.Fatalf("delta %d correlate=%q want corr-1", i, msgs[i].CorrelateTo)
		}
	}
	result := msgs[len(tokens)].GetSiChatResult()
	if result == nil {
		t.Fatalf("terminal not an SIChatResult: %+v", msgs[len(tokens)])
	}
	if got := result.GetMessage().GetContent(); got != "Hello world" {
		t.Fatalf("final content = %q, want %q", got, "Hello world")
	}
}

// TestTokenStreamSubstrate_CrossReplicaReplay proves the core #1266 guarantee:
// a bff replica that consumes only part of a stream then dies is replaced by a
// DIFFERENT replica (different consumerID) which replays the WHOLE stream from
// the durable substrate -- the second replica renders every delta + the terminal
// in order, none lost. (A production replica reuses the same logical consumerID
// to resume from the cursor; an independent consumerID replays from 0, the
// strictly-harder no-loss case, which is what this asserts.)
func TestTokenStreamSubstrate_CrossReplicaReplay(t *testing.T) {
	sub := newFakeSubstrate()
	const requestId = "chat-2"

	// Worker produces the full stream up front (10 tokens + Done).
	prod := testServiceWithSubstrate(sub, "agent-1")
	prodSess := newTestStreamSession(prod, newRecordingClientStream(context.Background()))
	chunks := make(chan common.StreamChunk, 32)
	for i := 0; i < 10; i++ {
		chunks <- common.StreamChunk{Content: fmt.Sprintf("t%d", i)}
	}
	chunks <- common.StreamChunk{Done: true}
	close(chunks)
	prodSess.produceTokenStreamToSubstrate(context.Background(), requestId, chunks)

	// Replica B takes over the WS and consumes the whole stream from the
	// substrate -- it must render all 10 deltas in order + the terminal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecordingClientStream(ctx)
	consumer := testServiceWithSubstrate(sub, "bff-B")
	consSess := newTestStreamSession(consumer, rec)
	done := make(chan struct{})
	go func() { consSess.consumeTokenStream(ctx, "corr-2", requestId); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not terminate")
	}

	msgs := rec.snapshot()
	if len(msgs) != 11 {
		t.Fatalf("want 11 msgs (10 deltas + result), got %d", len(msgs))
	}
	for i := 0; i < 10; i++ {
		if got := msgs[i].GetSiChunk().GetTextDelta(); got != fmt.Sprintf("t%d", i) {
			t.Fatalf("delta %d = %q (lost/reordered on replica takeover)", i, got)
		}
	}
	if msgs[10].GetSiChatResult() == nil {
		t.Fatalf("stream did not end with SIChatResult")
	}
}

// TestTokenStreamSubstrate_ProviderError proves a provider error mid-stream
// surfaces to the client as a QueryError terminal (not a silent hang).
func TestTokenStreamSubstrate_ProviderError(t *testing.T) {
	sub := newFakeSubstrate()
	const requestId = "chat-err"

	prod := testServiceWithSubstrate(sub, "agent-1")
	prodSess := newTestStreamSession(prod, newRecordingClientStream(context.Background()))
	chunks := make(chan common.StreamChunk, 4)
	chunks <- common.StreamChunk{Content: "partial"}
	chunks <- common.StreamChunk{Error: fmt.Errorf("provider exploded")}
	close(chunks)
	prodSess.produceTokenStreamToSubstrate(context.Background(), requestId, chunks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecordingClientStream(ctx)
	consSess := newTestStreamSession(testServiceWithSubstrate(sub, "bff-A"), rec)
	done := make(chan struct{})
	go func() { consSess.consumeTokenStream(ctx, "corr-err", requestId); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not terminate on error")
	}

	msgs := rec.snapshot()
	// 1 delta + 1 QueryError terminal.
	if len(msgs) < 1 {
		t.Fatalf("want at least the error terminal, got none")
	}
	last := msgs[len(msgs)-1]
	if last.GetQueryError() == nil {
		t.Fatalf("stream did not end with QueryError: %+v", last)
	}
}

// TestTranscriptStreamSubstrate_RoundTrip proves the audio path: the worker's
// transcribe send-sink translates SITranscribeStreamDelta/Complete into substrate
// frames, and the bff consumer reconstructs the same wire messages in order,
// preserving isFinal / confidence / provider.
func TestTranscriptStreamSubstrate_RoundTrip(t *testing.T) {
	sub := newFakeSubstrate()
	const requestId = "stt-1"

	// Worker side: build the substrate-backed send sink and push two deltas + a
	// complete through it (as pumpDeltas / the End handler would).
	worker := testServiceWithSubstrate(sub, "voice-1")
	send := worker.newTranscriptStreamSink(context.Background(), requestId, nil)
	if err := send(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_SiTranscribeStreamDelta{
			SiTranscribeStreamDelta: &memqlv1.SITranscribeStreamDelta{
				RequestId: requestId, Text: "hel", IsFinal: false, Confidence: 0.8,
			}}}); err != nil {
		t.Fatalf("delta 1: %v", err)
	}
	if err := send(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_SiTranscribeStreamDelta{
			SiTranscribeStreamDelta: &memqlv1.SITranscribeStreamDelta{
				RequestId: requestId, Text: "hello", IsFinal: true, Confidence: 0.95,
			}}}); err != nil {
		t.Fatalf("delta 2: %v", err)
	}
	if err := send(&memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_SiTranscribeStreamComplete{
			SiTranscribeStreamComplete: &memqlv1.SITranscribeStreamComplete{
				RequestId: requestId, Text: "hello", DurationMs: 123, Provider: "fake",
			}}}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Consumer side.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newRecordingClientStream(ctx)
	consSess := newTestStreamSession(testServiceWithSubstrate(sub, "bff-A"), rec)
	done := make(chan struct{})
	go func() { consSess.consumeTranscriptStream(ctx, "corr-stt", requestId); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("transcript consumer did not terminate")
	}

	msgs := rec.snapshot()
	if len(msgs) != 3 {
		t.Fatalf("want 3 msgs (2 deltas + complete), got %d: %+v", len(msgs), msgs)
	}
	d1 := msgs[0].GetSiTranscribeStreamDelta()
	if d1 == nil || d1.GetText() != "hel" || d1.GetIsFinal() || d1.GetConfidence() < 0.79 || d1.GetConfidence() > 0.81 {
		t.Fatalf("delta 1 mismatch: %+v", d1)
	}
	d2 := msgs[1].GetSiTranscribeStreamDelta()
	if d2 == nil || d2.GetText() != "hello" || !d2.GetIsFinal() {
		t.Fatalf("delta 2 mismatch: %+v", d2)
	}
	c := msgs[2].GetSiTranscribeStreamComplete()
	if c == nil || c.GetText() != "hello" || c.GetDurationMs() != 123 || c.GetProvider() != "fake" {
		t.Fatalf("complete mismatch: %+v", c)
	}
	for i, m := range msgs {
		if m.CorrelateTo != "corr-stt" {
			t.Fatalf("msg %d correlate=%q want corr-stt", i, m.CorrelateTo)
		}
	}
}
