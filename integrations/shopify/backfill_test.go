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
	// 3. Completed -> the same step remembers the URL and streams it. One
	// step rather than two on purpose: a poll that found a finished
	// operation and then returned without reading it would cost a whole
	// runner tick for nothing.
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

	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: url, Line: 3, Rule: originVersionRule}
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
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: h.admin.server.URL + "/download/gone.jsonl", Restarts: maxDownloadRestarts}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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

// A completed operation with no URL means the query matched nothing. That is
// a legitimately empty domain and the step is DONE, not stuck.
func TestAnEmptyDomainCompletes(t *testing.T) {
	h := newHarness(t)
	h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/1", "COMPLETED", ""))
	// A backfill this build began: its cursor carries the rule it began
	// under, so its end is the end (finished).
	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", Rule: originVersionRule}
	page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done {
		t.Fatalf("page = %+v, want done", page)
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

			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", OpIndex: 1, Line: 1500, URL: gone.URL + "/download/old.jsonl"}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
			if err != nil {
				t.Fatalf("a download the origin no longer keeps failed the backfill: %v", err)
			}
			if page.Done || len(page.Writes) != 0 {
				t.Fatalf("page = %+v, want a step that starts the operation again", page)
			}
			next := decodeBackfillCursor(page.NextCursor)
			want := backfillCursor{OpIndex: 1, Restarts: 1}
			if next != want {
				t.Errorf("next cursor = %+v, want %+v: no operation, no link, line 0, the same part, one restart", next, want)
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

	cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/3", URL: gone.URL + "/download/old.jsonl", Restarts: maxDownloadRestarts}
	_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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

			cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: srv.URL + "/download/file.jsonl", Restarts: 1}
			page, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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
			cur.URL = gonePath.URL + "/download/file.jsonl"
			page, err = h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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

		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: link}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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

		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: link}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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
		cur := backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: link}
		_, err := h.conn.BackfillStore(context.Background(), h.store(t), "v1:shopify:product", cur.encode())
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
	cursor := backfillCursor{OperationID: "gid://shopify/BulkOperation/1"}.encode()
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
		cursor = next.encode()
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
			h.admin.reply("ShopifyBulkStatus", operationStatus("gid://shopify/BulkOperation/2", "CANCELED", ""))

			// One gone link: the first restart.
			page, err := h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product",
				backfillCursor{OperationID: "gid://shopify/BulkOperation/1", URL: gone.URL + "/download/a.jsonl"}.encode())
			if err != nil {
				t.Fatal(err)
			}
			// One failed operation: the second.
			next := decodeBackfillCursor(page.NextCursor)
			next.OperationID = "gid://shopify/BulkOperation/2"
			page, err = h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product", next.encode())
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
				next.URL = gone.URL + "/download/b.jsonl"
			}
			if _, err := h.conn.BackfillStore(ctx, h.store(t), "v1:shopify:product", next.encode()); err == nil {
				t.Fatalf("a third restart (%s) was allowed", last)
			}
		})
	}
}
