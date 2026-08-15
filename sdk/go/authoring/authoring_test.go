package authoring

// authoring_test.go -- SDK-level coverage for DurablePromoteBundle's inverse,
// DurableDemoteBundle (memql#2163). It drives a real client.Dispatcher over a
// fake gRPC stream that echoes a DurableDemoteBundleResult correlated to the
// sent message, asserting the SDK maps the wire reply onto the typed DemoteResult
// and sends the right request payload.

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
	"google.golang.org/grpc/metadata"
)

// fakeStream implements memqlv1.MemqlService_StreamClient: it captures the
// client's sent message and, when replyFor is invoked, pushes a server reply
// correlated to that message so the dispatcher's SendAndWait returns it.
type fakeStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		sendCh: make(chan *memqlv1.MemqlClientMessage, 4),
		recvCh: make(chan *memqlv1.MemqlServerMessage, 4),
	}
}

func (m *fakeStream) Send(msg *memqlv1.MemqlClientMessage) error {
	m.sendCh <- msg
	return nil
}
func (m *fakeStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}
func (m *fakeStream) Header() (metadata.MD, error) { return nil, nil }
func (m *fakeStream) Trailer() metadata.MD         { return nil }
func (m *fakeStream) CloseSend() error             { close(m.recvCh); return nil }
func (m *fakeStream) Context() context.Context     { return context.Background() }
func (m *fakeStream) SendMsg(any) error            { return nil }
func (m *fakeStream) RecvMsg(any) error            { return nil }

// TestDurableDemoteBundle_MapsResult: the SDK sends a DurableDemoteBundleMsg
// carrying the sources and maps the correlated DurableDemoteBundleResult onto the
// typed DemoteResult.
func TestDurableDemoteBundle_MapsResult(t *testing.T) {
	stream := newFakeStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	c := NewClient(d)

	resCh := make(chan *DemoteResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := c.DurableDemoteBundle(ctx, "spec actorEnvelope mcpSessSpec { return role == \"admin\" }")
		resCh <- res
		errCh <- err
	}()

	// Capture the sent request and assert it carried the sources on the right
	// payload type.
	sent := <-stream.sendCh
	demoteMsg := sent.GetDurableDemoteBundle()
	if demoteMsg == nil {
		t.Fatalf("expected DurableDemoteBundleMsg payload; got %T", sent.GetPayload())
	}
	if demoteMsg.GetSources() == "" {
		t.Error("sources were not forwarded on the wire")
	}

	// Reply with a successful demote of one construct.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_DurableDemoteBundleResult{
			DurableDemoteBundleResult: &memqlv1.DurableDemoteBundleResult{
				Ok:      true,
				Demoted: []*memqlv1.AuthoringConstruct{{Kind: "spec", Name: "mcpSessSpec"}},
			},
		},
	}

	res := <-resCh
	if err := <-errCh; err != nil {
		t.Fatalf("DurableDemoteBundle: %v", err)
	}
	if res == nil || !res.OK {
		t.Fatalf("expected OK result, got %+v", res)
	}
	if len(res.Demoted) != 1 || res.Demoted[0].Name != "mcpSessSpec" || res.Demoted[0].Kind != "spec" {
		t.Errorf("demoted construct not mapped: %+v", res.Demoted)
	}
}

// TestDurableDemoteBundle_MapsConceptOutcomes: the demote reply's per-construct
// outcomes reach the SDK's own types (memql#3756).
//
// A concept with rows under it comes back RETIRED -- still registered, rows still
// readable, writes refused, name still claimed -- while the verbs bound to it come
// back REMOVED, and both are OK=true. A consumer that only sees Demoted cannot
// tell those apart at all, which is the case this pins: without the mapping the
// SDK silently drops the one field that says whether the name is free again, and
// the consumer's only remaining option is to reach past the SDK for the wire type.
func TestDurableDemoteBundle_MapsConceptOutcomes(t *testing.T) {
	stream := newFakeStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	c := NewClient(d)

	resCh := make(chan *DemoteResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := c.DurableDemoteBundle(ctx, "concept trainedWidget { }")
		resCh <- res
		errCh <- err
	}()

	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_DurableDemoteBundleResult{
			DurableDemoteBundleResult: &memqlv1.DurableDemoteBundleResult{
				Ok: true,
				Demoted: []*memqlv1.AuthoringConstruct{
					{Kind: "concept", Name: "trainedWidget"},
					{Kind: "mutation", Name: "createTrainedWidget"},
				},
				Outcomes: []*memqlv1.DurableDemoteOutcome{
					{Kind: "concept", Name: "trainedWidget", ConceptId: "v1:trainingns:trainedWidget", Outcome: "retired", RowCount: 412},
					{Kind: "mutation", Name: "createTrainedWidget", Outcome: "removed"},
				},
			},
		},
	}

	res := <-resCh
	if err := <-errCh; err != nil {
		t.Fatalf("DurableDemoteBundle: %v", err)
	}
	if len(res.Outcomes) != 2 {
		t.Fatalf("outcomes not mapped: %+v", res.Outcomes)
	}
	concept := res.Outcomes[0]
	if concept.Outcome != DemoteOutcomeRetired {
		t.Errorf("concept outcome = %q, want %q", concept.Outcome, DemoteOutcomeRetired)
	}
	if concept.ConceptId != "v1:trainingns:trainedWidget" || concept.RowCount != 412 {
		t.Errorf("concept outcome lost its id or the row count that chose it: %+v", concept)
	}
	if res.Outcomes[1].Outcome != DemoteOutcomeRemoved {
		t.Errorf("mutation outcome = %q, want %q", res.Outcomes[1].Outcome, DemoteOutcomeRemoved)
	}
}

// TestDurableDemoteBundle_SurfacesError: a server-side rejection comes back as
// OK=false with the Error populated (not a Go error -- that is reserved for
// wire-level failures).
func TestDurableDemoteBundle_SurfacesError(t *testing.T) {
	stream := newFakeStream()
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	c := NewClient(d)

	resCh := make(chan *DemoteResult, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := c.DurableDemoteBundle(ctx, "spec mcpSessSpec { }")
		resCh <- res
		errCh <- err
	}()

	sent := <-stream.sendCh
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_DurableDemoteBundleResult{
			DurableDemoteBundleResult: &memqlv1.DurableDemoteBundleResult{
				Ok:    false,
				Error: "authoring: durable demote found no demotable constructs",
			},
		},
	}

	res := <-resCh
	if err := <-errCh; err != nil {
		t.Fatalf("unexpected Go error for a server rejection: %v", err)
	}
	if res == nil || res.OK {
		t.Fatalf("expected OK=false result, got %+v", res)
	}
	if res.Error == "" {
		t.Error("server rejection error was not surfaced")
	}
}
