package outbound

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

type htmlSender struct{ sent email.Message }

func (s *htmlSender) Send(_ context.Context, m email.Message, _ email.SendAs) error {
	s.sent = m
	return nil
}
func TestEmailTransportSendsTheHTMLAlternative(t *testing.T) {
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := &htmlSender{}
	if err = engine.RegisterIntegration(email.NewIntegration(sender, quietLogger())); err != nil {
		t.Fatal(err)
	}
	transport := NewEmailTransport(engine, quietLogger())
	if err = transport.Deliver(context.Background(), Request{Target: "owner@example.com", Subject: "Hello", Payload: "Plain", HTMLBody: "<p>HTML</p>"}); err != nil {
		t.Fatal(err)
	}
	if sender.sent.TextBody != "Plain" || sender.sent.HTMLBody != "<p>HTML</p>" {
		t.Fatalf("message: %+v", sender.sent)
	}
}
func TestWorkerPreservesHTMLAndRefusesOversizedAlternative(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		row := pendingRow("html-request")
		row["medium"], row["target"] = "email", "owner@example.com"
		row["htmlBody"] = "<p>HTML</p>"
		if oversized {
			row["htmlBody"] = strings.Repeat("x", maxOutboundHTMLBytes+1)
		}
		engine := &fakeEngine{pending: []map[string]any{row}}
		transport := &fakeTransport{}
		worker := newTestWorker(engine, transport, nil)
		worker.drainOnce(context.Background())
		if oversized {
			if transport.count() != 0 {
				t.Fatal("oversized HTML was sent")
			}
			continue
		}
		if transport.count() != 1 || transport.delivered[0].HTMLBody != "<p>HTML</p>" {
			t.Fatalf("HTML lost: %+v", transport.delivered)
		}
	}
}
