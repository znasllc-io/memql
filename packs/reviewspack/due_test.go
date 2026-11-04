package reviewspack

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// due_test.go -- THE DAILY ASK (reviewsSendDueRequests).
//
// Once a day the store's own automation runs this. For every store whose
// requests are on, every paid order processed the store's delay ago earns one
// link per paid product (request.go's EligibleLines), written as the store's
// merchant, and each is mailed to the order's buyer through the order mail
// route. A line already asked is not asked again; a send that failed is.

var dueNow = time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)

type dueReader struct {
	settings []map[string]any
	sites    []map[string]any
	orders   []map[string]any
	lines    map[string][]map[string]any
	products map[string]map[string]any
	requests map[string]map[string]any
	asked    map[string]map[string]string
}

func (d *dueReader) Rows(_ context.Context, query string, args map[string]string) ([]map[string]any, error) {
	if d.asked == nil {
		d.asked = map[string]map[string]string{}
	}
	d.asked[query] = args
	switch query {
	case "reviewSettingsAll":
		return d.settings, nil
	case "reviewDueOrders":
		return d.orders, nil
	case "reviewOrderLines":
		return d.lines[args["orderGid"]], nil
	case "shopifyProductByGid":
		if p, ok := d.products[args["gid"]]; ok {
			return []map[string]any{p}, nil
		}
	case "reviewRequestById":
		if r, ok := d.requests[args["requestId"]]; ok {
			return []map[string]any{r}, nil
		}
	}
	return nil, nil
}

func (d *dueReader) RowsWithList(_ context.Context, query string, _ map[string]string, _ string, _ []string) ([]map[string]any, error) {
	if query == "sitesBoundToStore" {
		return d.sites, nil
	}
	return nil, nil
}

type fakeMailer struct {
	calls  []map[string]any
	answer string
	err    error
}

func (f *fakeMailer) SendOrderMessage(_ context.Context, args map[string]any) (string, string, error) {
	f.calls = append(f.calls, args)
	if f.err != nil {
		return "", "", f.err
	}
	if f.answer == "" {
		return "sent", "", nil
	}
	return f.answer, "", nil
}

func dueFixture() (*dueReader, *fakeWriter, *fakeMailer, *Provider) {
	r := &dueReader{
		settings: []map[string]any{{"storeId": "store-live", "accountId": "acct-shop", "requestsEnabled": true,
			"requestTemplateId": "tmpl-ask", "requestDelayDays": 14, "linkLifetimeDays": 14}},
		sites: []map[string]any{liveSite},
		orders: []map[string]any{{"gid": "gid://shopify/Order/9", "name": "#1009", "email": "buyer@example.com",
			"displayFinancialStatus": "PAID", "totalPriceSet": money("24.00"),
			"processedAt": dueNow.Add(-14*24*time.Hour - time.Hour).Format(time.RFC3339)}},
		lines:    map[string][]map[string]any{"gid://shopify/Order/9": {line("gid://shopify/Product/1", "12.00")}},
		products: map[string]map[string]any{"gid://shopify/Product/1": {"handle": "floss-picks", "title": "Floss Picks"}},
	}
	w, m := &fakeWriter{}, &fakeMailer{}
	return r, w, m, &Provider{reader: r, writer: w, mailer: m, now: func() time.Time { return dueNow }}
}

func TestADueOrderEarnsOneMailedLinkPerPaidProduct(t *testing.T) {
	r, w, m, p := dueFixture()
	nodes, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The window ends the store's delay ago.
	if to := r.asked["reviewDueOrders"]["to"]; !strings.HasPrefix(to, dueNow.Add(-14*24*time.Hour).Format("2006-01-02T15")) {
		t.Fatalf("orders were asked up to %q, want 14 days ago", to)
	}
	var writes []writeCall
	for _, c := range w.calls {
		if c.mutation == "putReviewRequest" {
			writes = append(writes, c)
		}
	}
	if len(writes) != 2 {
		t.Fatalf("request writes = %+v, want the request written pending, then marked sent", writes)
	}
	for _, c := range writes {
		if c.actor != "merchant-1" {
			t.Fatalf("a request was written as %q, want the store's merchant", c.actor)
		}
	}
	if writes[0].args["sendState"] != "pending" || writes[1].args["sendState"] != "sent" || writes[1].args["sentAt"] == nil {
		t.Fatalf("writes = %+v, want pending then sent with its time", writes)
	}
	if len(m.calls) != 1 {
		t.Fatalf("mails = %d, want one", len(m.calls))
	}
	mail := m.calls[0]
	for k, want := range map[string]any{"storeId": "store-live", "orderGid": "gid://shopify/Order/9", "accountId": "acct-shop",
		"templateId": "tmpl-ask", "requestId": requestIDFor("store-live", "gid://shopify/Order/9", "floss-picks")} {
		if mail[k] != want {
			t.Fatalf("mail %s = %v, want %v", k, mail[k], want)
		}
	}
	fields, _ := mail["fields"].(map[string]any)
	url, _ := fields["reviewUrl"].(string)
	if !strings.HasPrefix(url, "https://shop.example.com/reviews/write?product=floss-picks&t=") || fields["productTitle"] != "Floss Picks" {
		t.Fatalf("fields = %v, want the link on the store's site and the product", fields)
	}
	if token := strings.TrimPrefix(url, "https://shop.example.com/reviews/write?product=floss-picks&t="); tokenHash(token) != writes[0].args["tokenHash"] {
		t.Fatal("the mailed link's token does not hash to the stored hash")
	}
	var summary map[string]any
	_ = json.Unmarshal(nodes[0].Payload, &summary)
	if summary["sent"] != float64(1) {
		t.Fatalf("summary = %v", summary)
	}
}

func TestALineAlreadyAskedIsNotAskedAgainButAFailedOneIs(t *testing.T) {
	id := requestIDFor("store-live", "gid://shopify/Order/9", "floss-picks")
	for state, wantMail := range map[string]bool{"sent": false, "skipped": false, "failed": true, "pending": true} {
		r, _, m, p := dueFixture()
		r.requests = map[string]map[string]any{id: {"id": "v1:reviews:reviewRequest:" + id, "sendState": state}}
		if _, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0); err != nil {
			t.Fatal(err)
		}
		if got := len(m.calls) == 1; got != wantMail {
			t.Errorf("a line whose request is %s was mailed: %v, want %v", state, got, wantMail)
		}
	}
}

func TestStoresThatAskNothing(t *testing.T) {
	cases := map[string]func(map[string]any){
		"requests off":    func(s map[string]any) { s["requestsEnabled"] = false },
		"no template":     func(s map[string]any) { delete(s, "requestTemplateId") },
		"no organization": func(s map[string]any) { delete(s, "accountId") },
	}
	for name, change := range cases {
		r, w, m, p := dueFixture()
		change(r.settings[0])
		if _, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0); err != nil {
			t.Fatal(err)
		}
		if len(m.calls) != 0 || len(w.calls) != 0 {
			t.Errorf("%s: the store was asked (%d mails, %d writes)", name, len(m.calls), len(w.calls))
		}
	}
}

func TestAFailedSendIsRecordedAndTheRunGoesOn(t *testing.T) {
	_, w, m, p := dueFixture()
	m.err = context.DeadlineExceeded
	if _, err := p.sendDueRequests(fromAutomation(), map[string]any{}, 0); err != nil {
		t.Fatalf("one failed send stopped the run: %v", err)
	}
	last := w.calls[len(w.calls)-1]
	if last.args["sendState"] != "failed" || last.args["sendNote"] == "" {
		t.Fatalf("last write = %v, want the request marked failed with why", last.args)
	}
}

func TestTheDailyAskRefusesAClient(t *testing.T) {
	_, w, m, p := dueFixture()
	if _, err := p.sendDueRequests(moderatorCtx("mod-1"), map[string]any{}, 0); err == nil {
		t.Fatal("a client started the daily ask")
	}
	if len(m.calls) != 0 || len(w.calls) != 0 {
		t.Fatal("a refused call did something")
	}
}
