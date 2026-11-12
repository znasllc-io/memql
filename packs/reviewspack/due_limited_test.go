package reviewspack

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
)

// limitedOrderRoute answers every order send as the campaigns order route
// answers one that met the send-rate limit: not sent, not skipped, not queued.
type limitedOrderRoute struct{ memql.IntegrationEngineAccess }

func (limitedOrderRoute) Execute(context.Context, string) (*memql.ExecuteResult, error) {
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{{
		Id: "campaignRecipientSend",
		Payload: &structpb.Struct{Fields: map[string]*structpb.Value{
			"sent": structpb.NewBoolValue(false), "skipped": structpb.NewBoolValue(false), "limited": structpb.NewBoolValue(true),
			"reason": structpb.NewStringValue("The send-rate limit is reached; this email was not queued. Send it again later."),
		}},
	}}}}, nil
}

// An order message that met the send-rate limit is not queued by the order
// route; the daily ask records the request failed, and the next run asks
// again, with a new link.
func TestALimitedOrderSendIsRecordedFailedAndAskedAgain(t *testing.T) {
	status, _, err := (&engineMailer{engine: limitedOrderRoute{}}).SendOrderMessage(context.Background(), map[string]any{})
	if err != nil || status != "failed" {
		t.Fatalf("a limited answer is %q, %v; want failed, which the next run asks again", status, err)
	}
	id := requestIDFor("store-live", "gid://shopify/Order/9", "floss-picks")
	r, _, m, p := dueFixture()
	r.requests = map[string]map[string]any{id: {"id": "v1:reviews:reviewRequest:" + id, "sendState": status}}
	if _, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 {
		t.Fatalf("a request recorded %q was mailed %d times on the next run; want once", status, len(m.calls))
	}
}
