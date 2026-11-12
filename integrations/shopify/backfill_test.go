package shopify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// backfill_test.go -- Bulk Operations, streamed and resumable (#4394).

func bulkLines(lines ...map[string]any) string {
	var b strings.Builder
	for _, l := range lines {
		raw, _ := json.Marshal(l)
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}

// exportStartedAt is when a cursor's export was started: a cursor of a
// backfill this build began carries it (backfillCursor.At).
var exportStartedAt = "2026-08-23T09:00:00.000000000Z"

// inFlight is a cursor as a backfill this build began writes it: the rule it
// began under and, once it names an operation, when that export was started.
// A cursor without them is an earlier build's, which begins again at once.
func inFlight(c backfillCursor) string {
	c.Rule = originVersionRule
	if c.OperationID != "" && c.At == "" {
		c.At = exportStartedAt
	}
	return c.encode()
}

func startedOperation(id string) map[string]any {
	return map[string]any{"bulkOperationRunQuery": map[string]any{
		"bulkOperation": map[string]any{"id": id, "status": "CREATED"},
		"userErrors":    []any{},
	}}
}

func operationStatus(id, status, url string) map[string]any {
	return map[string]any{"node": map[string]any{
		"id": id, "status": status, "url": url, "objectCount": "2",
	}}
}

// completedAt scripts the origin's answer for an operation: complete, its
// file at link. A cursor never holds the link, so a test that resumes mid-file
// says where the origin would send the step.
func completedAt(h *testHarness, id, link string) {
	h.admin.reply("ShopifyBulkStatus", operationStatus(id, "COMPLETED", link))
}

func TestBackfillStartsPollsThenStreams(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()

	// 1. No cursor -> the operation is started and nothing is written yet.
	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(page.Writes) != 0 || page.NextCursor == "" {
		t.Fatalf("start step = %+v", page)
	}

	// 2. Still running -> the cursor is unchanged and nothing is written.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "RUNNING", ""))
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(page.Writes) != 0 {
		t.Fatalf("polling step = %+v", page)
	}

	url := h.admin.serveJSONL("/download/products.jsonl", bulkLines(
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T10:00:00Z"},
		map[string]any{"id": "gid://shopify/ProductVariant/9", "title": "M", "__parentId": "gid://shopify/Product/1", "updatedAt": "2026-08-23T10:00:00Z"},
		map[string]any{"id": "gid://shopify/Product/2", "title": "Hat", "updatedAt": "2026-08-23T10:00:00Z"},
	))
	// 3. Completed -> the same step is handed the link and streams from it.
	// One step rather than two on purpose: a poll that found a finished
	// operation and then returned without reading it would cost a whole
	// round of the pass for nothing.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done {
		t.Fatalf("the whole file should have been consumed in one batch: %+v", page)
	}
	if len(page.Writes) != 3 {
		t.Fatalf("got %d writes, want two products and one variant", len(page.Writes))
	}
	// A __parentId line becomes a CHILD of the concept its own GID names --
	// the JSONL interleaves connections, so position identifies nothing.
	var variant, product int
	for _, w := range page.Writes {
		switch w.Concept {
		case "v1:shopify:productVariant":
			variant++
			if w.Payload["parentGid"] != "gid://shopify/Product/1" {
				t.Errorf("variant parent = %q", w.Payload["parentGid"])
			}
		case "v1:shopify:product":
			product++
		}
	}
	if product != 2 || variant != 1 {
		t.Errorf("products=%d variants=%d", product, variant)
	}
}

// The whole point of the cursor: a crash mid-file resumes at the line it
// reached rather than re-applying the prefix or, worse, restarting.
func TestBackfillResumesFromTheLineOffset(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()

	var lines []map[string]any
	for i := 1; i <= 5; i++ {
		lines = append(lines, map[string]any{
			"id": "gid://shopify/Product/" + string(rune('0'+i)), "title": "P", "updatedAt": "2026-08-23T10:00:00Z",
		})
	}
	url := h.admin.serveJSONL("/download/five.jsonl", bulkLines(lines...))

	completedAt(h, "gid://shopify/BulkOperation/1", url)
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Line: 3, Rule: originVersionRule, At: exportStartedAt}
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", cur.encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Writes) != 2 {
		t.Fatalf("resumed with %d writes, want the two lines after the offset", len(page.Writes))
	}
	if !page.Done {
		t.Error("the file ended, so the step must report done")
	}
}

// A signed URL is valid for one week. Past that the download 403s, and the
// only recovery is a fresh operation. The connector starts it again itself,
// twice per backfill; past that the refusal is an error rather than a silent
// zero.
func TestAnExpiredDownloadIsAnErrorNotAnEmptyPage(t *testing.T) {
	h := newHarness(t)
	completedAt(h, "gid://shopify/BulkOperation/1", h.admin.server.URL+"/download/gone.jsonl")
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Restarts: maxDownloadRestarts}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	if err == nil || !strings.Contains(err.Error(), "one week") {
		t.Fatalf("err = %v, want an expiry explanation", err)
	}
}

// A bulk operation must finish within ten days and can be cancelled or fail.
// Restarting is the only recovery, and it is safe because every write is
// idempotent on the row id.
func TestAnExpiredOperationRestarts(t *testing.T) {
	for _, status := range []string{"EXPIRED", "CANCELED", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t)
			h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", status, ""))
			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
			if err != nil {
				t.Fatal(err)
			}
			if page.Done {
				t.Fatal("a failed operation must not report the domain done")
			}
			if decodeBackfillCursor(page.NextCursor).OperationID != "" {
				t.Error("the cursor still names the dead operation, so the restart never happens")
			}
		})
	}
}

// linklessCompletion is the origin's answer for a completed operation that
// gives no link, counting count objects ("" for an answer with no count).
func linklessCompletion(id, count string) map[string]any {
	node := map[string]any{"id": id, "status": "COMPLETED", "url": nil}
	if count != "" {
		node["objectCount"] = count
	}
	return map[string]any{"node": node}
}

// A completed operation with no URL that exported nothing means the query
// matched nothing. That is a legitimately empty domain and the step is DONE,
// not stuck.
func TestAnEmptyDomainCompletes(t *testing.T) {
	for name, count := range map[string]string{"a count of zero": "0", "no count": ""} {
		h := newHarness(t)
		h.admin.reply("ShopifyBulkStatus", linklessCompletion("gid://shopify/BulkOperation/1", count))
		// A backfill this build began: its cursor carries the rule it began
		// under, so its end is the end (finished).
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Rule: originVersionRule, At: exportStartedAt}
		page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !page.Done {
			t.Fatalf("%s: page = %+v, want done", name, page)
		}
	}
}

// A completed operation that COUNTED objects and gives no link, with nothing
// read from it, is not an empty kind: its file is gone -- an origin that stops
// giving a link once it has expired answers this way -- and it takes the path
// a gone download takes, started again within the two restarts.
func TestACompletedExportThatCountedObjectsAndGivesNoLinkIsAGoneDownload(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyBulkStatus", linklessCompletion("gid://shopify/BulkOperation/1", "2"))
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	if err != nil {
		t.Fatalf("a completed export with no link failed the backfill: %v", err)
	}
	if page.Done {
		t.Fatalf("page = %+v: an export that counted 2 objects and gave no file was read as an empty kind", page)
	}
	if next, want := decodeBackfillCursor(page.NextCursor), (backfillCursor{Restarts: 1, Rule: originVersionRule}); next != want {
		t.Errorf("next cursor = %+v, want %+v: started again, one restart", next, want)
	}

	cur.Restarts = maxDownloadRestarts
	_, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	var gone bulkDownloadGone
	if !errors.As(err, &gone) {
		t.Errorf("past the restarts: err = %v, want a gone download", err)
	}
}

func TestADomainWithNoBulkQuerySaysSo(t *testing.T) {
	h := newHarness(t)
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:orderLineItem", "")
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done || len(page.Writes) != 0 {
		t.Fatalf("page = %+v, want a done page with no writes -- the domain is filled by its parent", page)
	}
}

// bulkOperationRunQuery takes a query STRING and Shopify refuses a document
// carrying more than one operation, so a split domain has to hand over
// exactly the part being run.
func TestOperationBodyExtractsOneOperation(t *testing.T) {
	document := "query A($query: String) {\n  a { b }\n}\n\nquery B($query: String) {\n  c { d }\n}\n"
	body, err := operationBody(document, "B")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "query A") {
		t.Errorf("extracted both operations:\n%s", body)
	}
	if !strings.Contains(body, "c { d }") {
		t.Errorf("extracted the wrong body:\n%s", body)
	}
	if _, err := operationBody(document, "Missing"); err == nil {
		t.Error("a missing operation must be an error, not an empty query")
	}
}

// The runner orders domains parents-first: a child row carries parentGid, and
// a backfill that wrote line items before their order would leave a window in
// which the mirror answers "this line belongs to nothing".
func TestApplyOrderPutsParentsFirst(t *testing.T) {
	position := map[string]int{}
	for i, concept := range generated.ApplyOrder {
		position[concept] = i
	}
	if len(position) != len(generated.Types) {
		t.Fatalf("ApplyOrder covers %d of %d concepts", len(position), len(generated.Types))
	}
	for concept, spec := range generated.Types {
		if spec.Parent == "" {
			continue
		}
		if position[spec.Parent] > position[concept] {
			t.Errorf("%s is applied before its parent %s", concept, spec.Parent)
		}
	}
}

// The harness disables the bulk-download guard so its plaintext loopback
// server is reachable. That seam is also the one way streamBulk could stop
// consulting the guard without a test noticing -- so this removes it and
// checks the refusal comes back, against the same loopback URL every other
// backfill test streams happily.
func TestTheBulkDownloadGuardIsConsulted(t *testing.T) {
	h := newHarness(t)
	h.conn.admin.downloadGuard = nil
	store := h.store(t)
	ctx := context.Background()

	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", "")
	if err != nil {
		t.Fatal(err)
	}

	url := h.admin.serveJSONL("/download/products.jsonl", bulkLines(
		map[string]any{"id": "gid://shopify/Product/1", "title": "Shirt", "updatedAt": "2026-08-23T10:00:00Z"},
	))
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", url))
	_, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err == nil {
		t.Fatal("the download was fetched with no guard in front of it")
	}
	if !strings.Contains(err.Error(), "must be https") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// ---- a download the origin no longer keeps (Task 13b) ----

// twoPartProduct makes the product domain a two-part one for the test, so a
// restart can show it keeps the operation it was on rather than the domain's
// first. No generated domain is multi-part today.
func twoPartProduct(t *testing.T) {
	t.Helper()
	orig := generated.Types["product"]
	cp := *orig
	cp.BulkOps = []string{orig.BulkOps[0], orig.BulkOps[0]}
	generated.Types["product"] = &cp
	t.Cleanup(func() { generated.Types["product"] = orig })
}

func TestADownloadTheOriginNoLongerKeepsStartsTheOperationAgain(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			twoPartProduct(t)
			h := newHarness(t)
			var logs bytes.Buffer
			h.conn.logger = slog.New(slog.NewTextHandler(&logs, nil))
			gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			t.Cleanup(gone.Close)

			completedAt(h, "gid://shopify/BulkOperation/1", gone.URL+"/download/old.jsonl")
			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", OpIndex: 1, Line: 1500}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
			if err != nil {
				t.Fatalf("a download the origin no longer keeps failed the backfill: %v", err)
			}
			if page.Done || len(page.Writes) != 0 {
				t.Fatalf("page = %+v, want a step that starts the operation again", page)
			}
			next := decodeBackfillCursor(page.NextCursor)
			want := backfillCursor{OpIndex: 1, Restarts: 1, Rule: originVersionRule}
			if next != want {
				t.Errorf("next cursor = %+v, want %+v: no operation, line 0, the same part, one restart", next, want)
			}
			if got := decodeBackfillCursor(next.encode()); got != next {
				t.Errorf("the restart count did not survive the cursor's encode and decode: %+v", got)
			}
			line := logs.String()
			for _, needle := range []string{"level=WARN", "store=" + testStoreID, "concept=product", "status=" + strconv.Itoa(status)} {
				if !strings.Contains(line, needle) {
					t.Errorf("the warning does not carry %q:\n%s", needle, line)
				}
			}
		})
	}
}

func TestAThirdGoneDownloadFailsTheBackfill(t *testing.T) {
	h := newHarness(t)
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(gone.Close)

	completedAt(h, "gid://shopify/BulkOperation/3", gone.URL+"/download/old.jsonl")
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/3", Restarts: maxDownloadRestarts}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	if err == nil {
		t.Fatal("a third refused download did not fail the backfill: the restarts would never end")
	}
	if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "one week") {
		t.Errorf("err = %v, want the error as today, with the status", err)
	}
	var goneErr bulkDownloadGone
	if !errors.As(err, &goneErr) || goneErr.Status != http.StatusForbidden {
		t.Errorf("err = %v, want a bulkDownloadGone carrying 403", err)
	}
}

func TestAServerErrorOnTheDownloadIsNotARestart(t *testing.T) {
	broken := func(w http.ResponseWriter, _ *http.Request) {
		// The connection breaks before any answer: no status at all.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	cases := map[string]http.HandlerFunc{
		"503":               func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) },
		"429":               func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"broken connection": broken,
	}
	for name, handler := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			var down http.Handler = handler
			srv := httptest.NewServer(down)
			t.Cleanup(srv.Close)

			completedAt(h, "gid://shopify/BulkOperation/1", srv.URL+"/download/file.jsonl")
			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Restarts: 1}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
			if err == nil {
				t.Fatal("a failing download returned no error")
			}
			if name != "broken connection" && !strings.Contains(err.Error(), "HTTP "+name) {
				t.Errorf("err = %v, want the error as today, with the status", err)
			}
			var gone bulkDownloadGone
			if errors.As(err, &gone) {
				t.Errorf("err = %v is a gone download: this failure would be counted as a restart", err)
			}
			if page.NextCursor != "" {
				t.Fatalf("a failure moved the cursor to %q", page.NextCursor)
			}

			// The runner keeps the stored cursor on a failure. Taken again with
			// the link now gone, it still has its one restart left: the failure
			// above counted none.
			gonePath := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
			t.Cleanup(gonePath.Close)
			completedAt(h, "gid://shopify/BulkOperation/1", gonePath.URL+"/download/file.jsonl")
			page, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
			if err != nil {
				t.Fatalf("the restart left after the failure was not there: %v", err)
			}
			if got := decodeBackfillCursor(page.NextCursor).Restarts; got != 2 {
				t.Errorf("restarts after the failure and one gone link = %d, want 2", got)
			}
		})
	}
}

// ---- no part of a download link reaches an error (ruling 57) ----

// signedLink is a download link shaped as the origin signs one: a host, a
// path naming the export, and query parameters that open it for a week.
const signedLinkPath = "/exports/bulk-7781-products.jsonl"
const signedLinkQuery = "?X-Goog-Algorithm=GOOG4-RSA-SHA256&X-Goog-Credential=signer-credential&X-Goog-Signature=0badc0ffee5eed"

func assertNoPartOfTheLink(t *testing.T, text, link string) {
	t.Helper()
	// Cut by hand rather than parsed: an unparseable link must be checked too.
	host := link[strings.Index(link, "://")+3:]
	host = host[:strings.Index(host, "/")]
	hostname, port, _ := strings.Cut(host, ":")
	parts := []string{link, host, hostname, port, signedLinkPath, "bulk-7781-products", "X-Goog-Signature", "0badc0ffee5eed", "signer-credential", "X-Goog-Credential"}
	for _, part := range parts {
		if part != "" && strings.Contains(text, part) {
			t.Errorf("the text carries %q of the download link:\n%s", part, text)
		}
	}
}

func TestATransportFailureOnTheDownloadNeverCarriesTheLink(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		h := newHarness(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		link := srv.URL + signedLinkPath + signedLinkQuery
		srv.Close() // nothing listens there now

		completedAt(h, "gid://shopify/BulkOperation/1", link)
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
		if err == nil {
			t.Fatal("a refused connection returned no error")
		}
		assertNoPartOfTheLink(t, err.Error(), link)
		if !strings.Contains(err.Error(), "download") || !strings.Contains(err.Error(), "failed") || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("err = %v, want it to say the download failed and why", err)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("err = %v, want the cause kept", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t)
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		h.conn.admin.http = &http.Client{Timeout: 50 * time.Millisecond}
		link := srv.URL + signedLinkPath + signedLinkQuery

		completedAt(h, "gid://shopify/BulkOperation/1", link)
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
		if err == nil {
			t.Fatal("a download that timed out returned no error")
		}
		assertNoPartOfTheLink(t, err.Error(), link)
		if !strings.Contains(err.Error(), "download") || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("err = %v, want it to say the download timed out", err)
		}
	})
	t.Run("an unparseable link", func(t *testing.T) {
		h := newHarness(t)
		link := "https://files.example.test" + signedLinkPath + "%zz" + signedLinkQuery
		completedAt(h, "gid://shopify/BulkOperation/1", link)
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
		if err == nil {
			t.Fatal("an unparseable link returned no error")
		}
		assertNoPartOfTheLink(t, err.Error(), link)
	})
}

// The guard's own refusals name no part of the link either: it runs before
// every download and every redirect, and its error is the backfill's.
func TestTheDownloadGuardNamesNoPartOfTheLink(t *testing.T) {
	for _, link := range []string{
		"https://files.example.test" + signedLinkPath + "%zz" + signedLinkQuery,
		"https://10.0.0.7" + signedLinkPath + signedLinkQuery,
		"http://files.example.test" + signedLinkPath + signedLinkQuery,
	} {
		err := checkBulkDownloadURL(link)
		if err == nil {
			t.Fatalf("%s was not refused", link)
		}
		assertNoPartOfTheLink(t, err.Error(), link)
	}
}

// ---- an operation the origin fails is not started again for ever (ruling 57) ----

func TestAnOperationFailedThreeTimesFailsTheBackfill(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyBulkStatus", map[string]any{"node": map[string]any{
		"id": "gid://shopify/BulkOperation/1", "status": "FAILED", "errorCode": "INTERNAL_SERVER_ERROR", "url": nil,
	}})
	cursor := inFlight(backfillCursor{OperationID: "gid://shopify/BulkOperation/1"})
	for restart := 1; restart <= maxDownloadRestarts; restart++ {
		page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cursor)
		if err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
		next := decodeBackfillCursor(page.NextCursor)
		if next.OperationID != "" || next.Restarts != restart {
			t.Fatalf("restart %d: cursor = %+v, want no operation and %d restarts", restart, next, restart)
		}
		next.OperationID = "gid://shopify/BulkOperation/1" // the fresh operation, failed again
		cursor = inFlight(next)
	}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cursor)
	if err == nil {
		t.Fatal("a third failed operation was started again: the restarts would never end")
	}
	if !strings.Contains(err.Error(), "FAILED") || !strings.Contains(err.Error(), "INTERNAL_SERVER_ERROR") {
		t.Errorf("err = %v, want the status and the origin's errorCode", err)
	}
}

func TestAGoneLinkAndAFailedOperationShareTheRestarts(t *testing.T) {
	for _, last := range []string{"gone link", "failed operation"} {
		t.Run(last, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
			t.Cleanup(gone.Close)
			// One gone link: the first restart.
			completedAt(h, "gid://shopify/BulkOperation/1", gone.URL+"/download/a.jsonl")
			page, err := h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product",
				inFlight(backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}))
			if err != nil {
				t.Fatal(err)
			}
			// One failed operation: the second.
			h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/2", "CANCELED", ""))
			next := decodeBackfillCursor(page.NextCursor)
			next.OperationID = "gid://shopify/BulkOperation/2"
			page, err = h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product", inFlight(next))
			if err != nil {
				t.Fatal(err)
			}
			next = decodeBackfillCursor(page.NextCursor)
			if next.Restarts != 2 {
				t.Fatalf("restarts = %d after one of each, want 2", next.Restarts)
			}
			// The next of either kind fails the backfill.
			next.OperationID = "gid://shopify/BulkOperation/2"
			if last == "gone link" {
				completedAt(h, "gid://shopify/BulkOperation/2", gone.URL+"/download/b.jsonl")
			}
			if _, err := h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product", inFlight(next)); err == nil {
				t.Fatalf("a third restart (%s) was allowed", last)
			}
		})
	}
}

// ---- a download link is never stored (ruling 72) ----

// manyProducts is a bulk file of n product lines.
func manyProducts(n int) string {
	lines := make([]map[string]any, 0, n)
	for i := 1; i <= n; i++ {
		lines = append(lines, map[string]any{"id": "gid://shopify/Product/" + strconv.Itoa(i), "title": "P", "updatedAt": "2026-08-23T10:00:00Z"})
	}
	return bulkLines(lines...)
}

// assertCursorHoldsNoLink fails when a cursor carries a download link, or any
// part of this one.
func assertCursorHoldsNoLink(t *testing.T, cursor, link string) {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(cursor), &fields); err != nil {
		t.Fatalf("the cursor %q is not JSON: %v", cursor, err)
	}
	if _, ok := fields["url"]; ok {
		t.Errorf("the cursor holds a link: %s", cursor)
	}
	for _, v := range fields {
		if s, ok := v.(string); ok && (strings.Contains(s, "://") && !strings.HasPrefix(s, "gid://")) {
			t.Errorf("the cursor holds an address: %s", cursor)
		}
	}
	if carriesLink(cursor, link) {
		t.Errorf("the cursor carries a part of the download link %s: %s", link, cursor)
	}
}

func TestTheStoredCursorNeverHoldsTheDownloadLink(t *testing.T) {
	h := newHarness(t)
	store := h.store(t)
	ctx := context.Background()
	link := h.admin.serveJSONL("/download/bulk-7781-products.jsonl", manyProducts(backfillBatchLines+100))

	h.admin.reply("ShopifyBulkRun", startedOperation("gid://shopify/BulkOperation/1"))
	page, err := h.conn.BackfillStore(ctx, store, "v1:shopify:product", "")
	if err != nil {
		t.Fatal(err)
	}
	assertCursorHoldsNoLink(t, page.NextCursor, link)

	// The export is ready: this step is handed the link and reads the first
	// batch from it. The file has not ended, so the cursor it leaves is the one
	// the domain's health row keeps.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", link))
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if page.Done || len(page.Writes) != backfillBatchLines {
		t.Fatalf("the first batch = %d writes, done %t; want %d and a file not yet ended", len(page.Writes), page.Done, backfillBatchLines)
	}
	assertCursorHoldsNoLink(t, page.NextCursor, link)
	mid := decodeBackfillCursor(page.NextCursor)
	if mid.OperationID != "gid://shopify/BulkOperation/1" || mid.Line != backfillBatchLines {
		t.Fatalf("the cursor after the first batch = %+v, want the operation and the line reached", mid)
	}

	// The next step asks the origin for the link again and reads on.
	asked := h.admin.countOp("ShopifyBulkStatus")
	page, err = h.conn.BackfillStore(ctx, store, "v1:shopify:product", page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done || len(page.Writes) != 100 {
		t.Fatalf("the second batch = %d writes, done %t; want the last 100 and the end", len(page.Writes), page.Done)
	}
	if n := h.admin.countOp("ShopifyBulkStatus") - asked; n != 1 {
		t.Errorf("the step asked the origin for the operation %d times, want once: the link is asked for at every step", n)
	}
	assertCursorHoldsNoLink(t, page.NextCursor, link)
}

func TestAResumeFromACursorWithNoLinkAsksTheOriginAndContinuesAtTheStoredLine(t *testing.T) {
	h := newHarness(t)
	link := h.admin.serveJSONL("/download/five.jsonl", manyProducts(5))
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", link))

	cur := inFlight(backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Line: 3})
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Writes) != 2 || !page.Done {
		t.Fatalf("resumed with %d writes, done %t; want the two lines after the stored line, and the end", len(page.Writes), page.Done)
	}
	if got := page.Writes[0].Payload["gid"]; got != "gid://shopify/Product/4" {
		t.Errorf("the first line read after the resume is %v, want the fourth", got)
	}
	if n := h.admin.countOp("ShopifyBulkStatus"); n != 1 {
		t.Errorf("the origin was asked for the operation %d times, want once", n)
	}
}

func TestAnOlderCursorThatHoldsALinkContinuesAndLosesIt(t *testing.T) {
	h := newHarness(t)
	link := h.admin.serveJSONL("/download/now.jsonl", manyProducts(backfillBatchLines+10))
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", link))

	// The cursor an earlier build stored: its link, under "url", is one the
	// origin no longer serves. Followed, it would be refused as gone.
	stale := h.admin.server.URL + "/download/stale-link.jsonl"
	old := `{"op":"gid://shopify/BulkOperation/1","line":3,"url":"` + stale + `","v":1,"at":"` + exportStartedAt + `"}`
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", old)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Writes) != backfillBatchLines {
		t.Fatalf("an older cursor read %d lines, want a batch of %d from the line it had reached", len(page.Writes), backfillBatchLines)
	}
	if got := page.Writes[0].Payload["gid"]; got != "gid://shopify/Product/4" {
		t.Errorf("the first line read is %v, want the fourth: the cursor's line is kept", got)
	}
	next := decodeBackfillCursor(page.NextCursor)
	if next.OperationID != "gid://shopify/BulkOperation/1" || next.Line != 3+backfillBatchLines || next.Restarts != 0 {
		t.Errorf("the cursor it leaves = %+v, want the same operation, %d lines on, and no restart", next, backfillBatchLines)
	}
	assertCursorHoldsNoLink(t, page.NextCursor, stale)
	assertCursorHoldsNoLink(t, page.NextCursor, link)
}

func TestALinkTheOriginNoLongerGivesStartsTheOperationAgainWithinTheTwoRestarts(t *testing.T) {
	twoPartProduct(t)
	h := newHarness(t)
	// Complete, 1500 lines of its file applied, and the origin no longer says
	// where the file is.
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", ""))
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", OpIndex: 1, Line: 1500}
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	if err != nil {
		t.Fatalf("a link the origin no longer gives failed the backfill: %v", err)
	}
	if page.Done || len(page.Writes) != 0 {
		t.Fatalf("page = %+v, want a step that starts the operation again", page)
	}
	if next, want := decodeBackfillCursor(page.NextCursor), (backfillCursor{OpIndex: 1, Restarts: 1, Rule: originVersionRule}); next != want {
		t.Errorf("next cursor = %+v, want %+v: no operation, line 0, the same part, one restart", next, want)
	}

	// The third time, the backfill fails: with the reason, and as a gone
	// download does.
	cur.Restarts = maxDownloadRestarts
	_, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
	if err == nil {
		t.Fatal("a third link the origin no longer gives was started again: the restarts would never end")
	}
	var gone bulkDownloadGone
	if !errors.As(err, &gone) || !strings.Contains(err.Error(), "one week") {
		t.Errorf("err = %v, want a gone download naming the week a link lasts", err)
	}

	// Nothing read from it yet, no link, and nothing counted: the export
	// matched nothing.
	h.admin.reply("ShopifyBulkStatus", linklessCompletion("gid://shopify/BulkOperation/1", "0"))
	page, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product",
		inFlight(backfillCursor{OperationID: "gid://shopify/BulkOperation/1", OpIndex: 1}))
	if err != nil || !page.Done {
		t.Errorf("an export with no file and nothing read = %+v, %v; want the domain done", page, err)
	}
}

// An operation the origin no longer knows has no link to give either. Before
// the link stopped being stored, a backfill resumed long after its export
// went straight to the link it kept, was refused, and started the operation
// again; asking the origin first must not turn that into a failure.
func TestAnOperationTheOriginNoLongerKnowsStartsAgainAsAGoneDownloadDoes(t *testing.T) {
	for _, line := range []int{0, 1500} {
		h := newHarness(t)
		h.admin.reply("ShopifyBulkStatus", map[string]any{"node": nil})
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Line: line}
		page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
		if err != nil {
			t.Fatalf("line %d: an operation the origin no longer knows failed the backfill: %v", line, err)
		}
		if next, want := decodeBackfillCursor(page.NextCursor), (backfillCursor{Restarts: 1, Rule: originVersionRule}); page.Done || next != want {
			t.Errorf("line %d: page done %t, next cursor %+v; want a step that starts the operation again, %+v", line, page.Done, next, want)
		}

		cur.Restarts = maxDownloadRestarts
		_, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", inFlight(cur))
		var gone bulkDownloadGone
		if !errors.As(err, &gone) {
			t.Errorf("line %d: after the two restarts, err = %v; want the backfill failed as a gone download fails it", line, err)
		}
	}
}
