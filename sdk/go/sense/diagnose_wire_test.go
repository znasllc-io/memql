package sense

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
	"google.golang.org/grpc/metadata"
)

// diagnoseStream is a MemqlService_StreamClient that hands the test each sent
// message and replies with whatever the test pushes, correlated by the test.
type diagnoseStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
}

func (m *diagnoseStream) Send(msg *memqlv1.MemqlClientMessage) error { m.sendCh <- msg; return nil }
func (m *diagnoseStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}
func (m *diagnoseStream) Header() (metadata.MD, error) { return nil, nil }
func (m *diagnoseStream) Trailer() metadata.MD         { return nil }
func (m *diagnoseStream) CloseSend() error             { close(m.recvCh); return nil }
func (m *diagnoseStream) Context() context.Context     { return context.Background() }
func (m *diagnoseStream) SendMsg(any) error            { return nil }
func (m *diagnoseStream) RecvMsg(any) error            { return nil }

// Diagnose sends the document's tree path -- which is what asks the cluster to
// run its load over the source (memql#5434) -- and a load refusal comes back
// with its rule id in Code, where a caller keys a quick fix.
func TestDiagnoseSendsTheTreePathAndReturnsTheRuleCode(t *testing.T) {
	stream := &diagnoseStream{
		sendCh: make(chan *memqlv1.MemqlClientMessage, 1),
		recvCh: make(chan *memqlv1.MemqlServerMessage, 1),
	}
	d := client.NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()
	c := NewClient(d)

	type result struct {
		diags []Diagnostic
		err   error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		diags, err := c.Diagnose(ctx, "query widget q { filter row => row.nope == 1 }", "beta/queries.memql")
		done <- result{diags, err}
	}()

	sent := <-stream.sendCh
	msg := sent.GetSenseDiagnose()
	if msg == nil {
		t.Fatalf("expected a SenseDiagnoseMsg, got %T", sent.GetPayload())
	}
	if msg.GetFilePath() != "beta/queries.memql" {
		t.Errorf("FilePath = %q -- without it the cluster cannot place the document and runs no load", msg.GetFilePath())
	}
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: sent.GetMessageId(),
		Payload: &memqlv1.MemqlServerMessage_SenseDiagnoseResult{
			SenseDiagnoseResult: &memqlv1.SenseDiagnoseResult{
				Diagnostics: []*memqlv1.SenseDiagnostic{{
					Range: &memqlv1.SenseRange{
						Start: &memqlv1.SensePosition{Line: 1, Column: 33},
						End:   &memqlv1.SensePosition{Line: 1, Column: 41},
					},
					Severity: memqlv1.SenseSeverity_SENSE_SEVERITY_ERROR,
					Message:  "`row.nope` does not lower in a query filter: `nope` is not a declared field of v1:beta:widget",
					Code:     "lower_unknown_field",
				}},
			},
		},
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("Diagnose: %v", got.err)
	}
	if len(got.diags) != 1 || got.diags[0].Code != "lower_unknown_field" || got.diags[0].Severity != 1 {
		t.Errorf("want the load refusal as an Error carrying its rule id, got %+v", got.diags)
	}
}
