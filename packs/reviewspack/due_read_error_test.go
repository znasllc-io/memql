package reviewspack

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type requestReadFailure struct {
	*dueReader
	requestID string
}

func (r requestReadFailure) Rows(ctx context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if query == "reviewRequestById" && (r.requestID == "" || args["requestId"] == r.requestID) {
		return nil, errors.New("request state temporarily unavailable")
	}
	return r.dueReader.Rows(ctx, query, args)
}

// A read failure is not proof the buyer has never been asked. Replacing a
// possibly sent request's hash would invalidate the existing emailed link.
func TestUnreadRequestStateNeitherReplacesLinkNorMailsBuyer(t *testing.T) {
	r, w, m, p := dueFixture(t)
	p.reader = requestReadFailure{dueReader: r}
	nodes, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 0 || len(m.calls) != 0 {
		t.Fatalf("unread request caused %d writes and %d mail calls; want none", len(w.calls), len(m.calls))
	}
	var tally map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &tally); err != nil {
		t.Fatal(err)
	}
	if tally["failed"] != float64(1) {
		t.Fatalf("tally=%v; want one failed line for a later retry", tally)
	}
}

func TestUnreadRequestDoesNotStopOtherBuyers(t *testing.T) {
	r, w, m, p := dueFixture(t)
	other := map[string]any{}
	for k, v := range r.orders[0] {
		other[k] = v
	}
	other["gid"] = "gid://shopify/Order/10"
	r.orders = append(r.orders, other)
	r.lines["gid://shopify/Order/10"] = r.lines["gid://shopify/Order/9"]
	p.reader = requestReadFailure{dueReader: r, requestID: requestIDFor("store-live", "gid://shopify/Order/9", "floss-picks")}
	if _, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || m.calls[0]["orderGid"] != "gid://shopify/Order/10" || len(w.calls) != 2 {
		t.Fatalf("mail calls=%v, writes=%d; want only the readable second order mailed", m.calls, len(w.calls))
	}
}
