package shopify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// backfill.go -- the initial load, as a Bulk Operation.
//
// Bulk is not an optimisation here, it is the only workable shape: bulk
// queries are EXEMPT from the cost bucket, so a hundred thousand orders cost
// nothing against the limit that paging them would exhaust in minutes. What
// they cost instead is asynchrony -- Shopify runs the query, writes a JSONL
// file and tells you when it is ready -- and resumability, because a file of
// a million lines will outlive at least one deploy.
//
// The JSONL is flat: parents first, then children carrying __parentId. That
// is why the mapper takes a parentGID rather than reading children out of a
// nested object -- one mapper, two arrival shapes.

// backfillCursor is the resume point. Serialised as JSON into syncState's
// opaque cursor field, so the shape can change without a migration.
//
// IT NAMES THE OPERATION AND NEVER ITS DOWNLOAD LINK. The link opens a store's
// whole export for a week, and the cursor is a field of the domain's health
// row, which the cluster owner's queries hand to a browser. So each step asks
// the origin for the operation's link and uses it for that step alone
// (BackfillStore). A cursor an earlier build stored may still hold one, under
// "url": it is not read, and it is gone from the row at the cursor's next
// write.
type backfillCursor struct {
	// OperationID is the running (or completed) bulk operation.
	OperationID string `json:"op,omitempty"`
	// OpIndex is which of a multi-part domain's operations this is.
	OpIndex int `json:"i,omitempty"`
	// Line is how many JSONL lines have already been applied. Resuming
	// re-downloads and skips: re-reading a prefix is cheaper than any
	// bookkeeping that would let us seek.
	Line int `json:"line,omitempty"`
	// Restarts counts the operations this backfill started again because the
	// origin's download was gone (bulkDownloadGone) or the origin ended the
	// operation (EXPIRED, CANCELED, FAILED). It is the backfill's, not one
	// operation's: it is carried across the parts of a multi-part domain and
	// bounded by maxDownloadRestarts.
	Restarts int `json:"r,omitempty"`
	// Rule is the write rule the backfill BEGAN under: originVersionRule for
	// one that began from an empty cursor on this build, nothing for one an
	// earlier build began. It rides every cursor of the backfill and the one
	// it completes with, so a completed domain's row says which rule read it
	// (backfillReadUnderRule).
	Rule int `json:"v,omitempty"`
	// At is when this engine started the export the cursor follows
	// (memqlsync.ReadTime): the time the origin was read for every line of it.
	At string `json:"at,omitempty"`
}

// originVersionRule is the write rule under which every bulk line carries the
// origin's own version and the time its export was started. Before it, a
// line was stamped with the time it was streamed, and a snapshot older than a
// delivery could replace what the delivery recorded. A domain read whole
// under the earlier rule is read once more: the boot pass queues it
// (queueUnstartedBackfills). One being read when this build started is not
// carried to its end first: it begins again at its next step, from nothing
// and under the rule (BackfillStore).
const originVersionRule = 1

// backfillReadUnderRule reports whether a domain's stored cursor says its
// backfill began under originVersionRule.
func backfillReadUnderRule(cursor string) bool {
	return decodeBackfillCursor(cursor).Rule >= originVersionRule
}

// maxDownloadRestarts is how many times one backfill starts an operation again,
// in all: after its download was gone, or after the origin reported the
// operation EXPIRED, CANCELED or FAILED. The next of either fails the
// backfill: a fault that survives fresh operations is not a passing one, and
// starting again for ever would hide it.
const maxDownloadRestarts = 2

// bulkDownloadGone is a bulk download that is no longer there: the origin
// refused it (Status 400, 403, 404 or 410 -- the signed link has expired, or
// the file with it), or gives no link for it (Status 0) -- for an operation
// whose file was already being read, or one it no longer knows. Only a fresh
// operation recovers from it. Any other failure of the download -- a 5xx, a
// 429, a broken connection -- is not this error.
type bulkDownloadGone struct{ Status int }

func (e bulkDownloadGone) Error() string {
	if e.Status == 0 {
		return "shopify: the origin no longer gives the bulk download's link (a signed URL expires after one week; restart the operation)"
	}
	return fmt.Sprintf("shopify: bulk download HTTP %d (a signed URL expires after one week; restart the operation)", e.Status)
}

func (c backfillCursor) encode() string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeBackfillCursor(s string) backfillCursor {
	var out backfillCursor
	if s == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// backfillBatchLines bounds one Backfill step. A step returns writes the
// caller applies before asking for the next, so this is the unit of progress
// a crash can lose -- small enough that re-doing it is cheap, large enough
// that the round trip is not the cost.
const backfillBatchLines = 500

// bulkRunMutation selects the user error's code: bulkOperationRunQuery's
// userErrors are BulkOperationUserError (Admin API 2026-07), whose code
// OPERATION_IN_PROGRESS is the one refusal that says the shop is busy rather
// than that anything is wrong.
const bulkRunMutation = `mutation ShopifyBulkRun($query: String!) {
  bulkOperationRunQuery(query: $query) {
    bulkOperation { id status }
    userErrors { field message code }
  }
}`

// bulkOperationInProgress is BulkOperationUserErrorCode's answer for a shop
// already running a bulk query: Shopify runs one at a time per shop and app.
const bulkOperationInProgress = "OPERATION_IN_PROGRESS"

const bulkStatusQuery = `query ShopifyBulkStatus($id: ID!) {
  node(id: $id) {
    ... on BulkOperation {
      id
      status
      errorCode
      objectCount
      url
      partialDataUrl
      completedAt
    }
  }
}`

// bulkOperation is Shopify's own view of a running export.
type bulkOperation struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	ErrorCode   string `json:"errorCode"`
	ObjectCount string `json:"objectCount"`
	URL         string `json:"url"`
	CompletedAt string `json:"completedAt"`
}

// exported reports whether the origin counted any object in the operation's
// export. Shopify answers objectCount as an unsigned integer in a string; a
// count it did not give, or cannot be read as one, is no count.
func (op bulkOperation) exported() bool {
	n, err := strconv.ParseUint(strings.TrimSpace(op.ObjectCount), 10, 64)
	return err == nil && n > 0
}

// Backfill implements sync.Connector for one store's domain.
//
// The runtime never calls it: the connector is partitioned, and each store is
// backfilled from its own cursor through BackfillPartition. It stays for the
// contract, as a loop over Partitions that takes one step for the first store
// with work left, under the one cursor it is handed.
func (c *Connector) Backfill(ctx context.Context, concept, cursor string) (memqlsync.BackfillPage, error) {
	parts, err := c.Partitions(ctx)
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	for _, part := range parts {
		page, err := c.BackfillPartition(ctx, part, concept, cursor)
		if err != nil {
			return page, err
		}
		if !page.Done || len(page.Writes) > 0 {
			return page, nil
		}
	}
	return memqlsync.BackfillPage{Done: true}, nil
}

// BackfillStore advances one store's backfill of one domain by one step.
// Every write it returns carries the store as its partition (mapObject).
func (c *Connector) BackfillStore(ctx context.Context, store Store, concept, cursor string) (memqlsync.BackfillPage, error) {
	spec := generated.Types[generated.ConceptFromID(concept)]
	if spec == nil {
		return memqlsync.BackfillPage{}, fmt.Errorf("shopify: %q is not a mirrored concept", concept)
	}
	if !spec.Bulk || len(spec.BulkOps) == 0 {
		// A domain with no bulk query is materialised with its parent or
		// re-listed; saying so beats a silent empty page that reads like
		// "done".
		return memqlsync.BackfillPage{Done: true}, nil
	}

	cur := decodeBackfillCursor(cursor)
	if strings.TrimSpace(cursor) == "" {
		// A backfill beginning: its every line carries the origin's version.
		cur.Rule = originVersionRule
	}
	if !cur.underRule() {
		// A backfill an earlier build began, running when this build
		// started. Its lines were stamped with the time they were streamed,
		// and its cursor does not say when its export started, which is the
		// read time every line must carry (memqlsync.Refuses): continued, a
		// line from an export started before a delete would bring the record
		// back. It is begun again now, once, under the rule; the lines it has
		// not reached would be read again by that re-read anyway.
		c.logger.Info("shopify: a backfill begun before every line carried the origin's version and its export's start; beginning it again",
			"store", store.ID, "concept", spec.Concept)
		return memqlsync.BackfillPage{NextCursor: backfillCursor{Rule: originVersionRule}.encode()}, nil
	}
	if cur.OpIndex >= len(spec.BulkOps) {
		return c.finished(cur), nil
	}

	// 1. No operation yet: start one. Its start is the read time of every
	// line it exports, however much later the file is streamed: the origin
	// takes its snapshot after this moment.
	if cur.OperationID == "" {
		startedAt := c.now()
		op, err := c.startBulkOperation(ctx, store, spec, cur.OpIndex)
		if err != nil {
			return memqlsync.BackfillPage{}, err
		}
		cur.At = memqlsync.ReadTime(startedAt)
		cur.OperationID = op.ID
		cur.Line = 0
		return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
	}

	// 2. Ask the origin for the operation: whether it is done and, once it is,
	// the link its file is read from. The link is asked for at EVERY step and
	// used for this step alone; the cursor never keeps it (backfillCursor).
	op, err := c.bulkStatus(ctx, store, cur.OperationID)
	if errors.Is(err, errBulkOperationUnknown) {
		// The origin no longer knows the operation, so it has no link to give
		// for it: gone, as a download it refuses is.
		return c.downloadGone(store, spec, cur, bulkDownloadGone{})
	}
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	switch op.Status {
	case "COMPLETED":
		if op.URL == "" && cur.Line == 0 && !op.exported() {
			// A completed operation with no link, nothing read from it and
			// nothing counted: the query matched nothing. That is a legitimate
			// empty domain and the step is done, not stuck. One that counted
			// objects and gives no link has a file that is gone -- an origin
			// may stop giving the link once it has expired -- and is taken as
			// a gone download is (below).
			return c.advanceOperation(cur, spec, "no rows")
		}
	case "CREATED", "RUNNING":
		return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
	case "EXPIRED", "CANCELED", "FAILED":
		// A bulk operation must finish within ten days and its file
		// expires a week after that. Restarting is the only recovery,
		// and it is safe: every write is idempotent on the row id. It
		// counts toward the same bound as a gone download: an origin
		// that fails the export every time is not asked for ever.
		if cur.Restarts >= maxDownloadRestarts {
			code := op.ErrorCode
			if code == "" {
				code = "none"
			}
			return memqlsync.BackfillPage{}, fmt.Errorf("shopify: the bulk operation of %s for store %s ended %s (errorCode %s) after %d restarts of this backfill; it is not started again",
				spec.Concept, store.ID, op.Status, code, cur.Restarts)
		}
		c.logger.Warn("shopify: restarting bulk operation", "store", store.ID, "concept", spec.Concept, "status", op.Status, "errorCode", op.ErrorCode, "restart", cur.Restarts+1)
		return memqlsync.BackfillPage{NextCursor: backfillCursor{OpIndex: cur.OpIndex, Restarts: cur.Restarts + 1, Rule: cur.Rule}.encode()}, nil
	default:
		return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
	}

	// 3. Stream the next batch of lines, from the link the origin just gave.
	// No link for an operation whose file was already being read is the file
	// gone, as a refused download is.
	if op.URL == "" {
		return c.downloadGone(store, spec, cur, bulkDownloadGone{})
	}
	startedAt, _ := memqlsync.VersionTime(cur.At)
	writes, consumed, eof, err := c.streamBulk(ctx, store, spec, op.URL, cur.Line, backfillBatchLines, startedAt)
	var gone bulkDownloadGone
	if errors.As(err, &gone) {
		return c.downloadGone(store, spec, cur, gone)
	}
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	cur.Line += consumed
	if !eof {
		return memqlsync.BackfillPage{Writes: writes, NextCursor: cur.encode()}, nil
	}
	next, err := c.advanceOperation(cur, spec, fmt.Sprintf("%d lines", cur.Line))
	if err != nil {
		return memqlsync.BackfillPage{}, err
	}
	next.Writes = writes
	return next, nil
}

// downloadGone is a step's answer when its download is gone: the link
// outlived its week (a backfill paused or stopped past it), the file went
// with it, or the origin no longer gives a link for the operation at all.
// The operation is dropped and the SAME part starts again on the next step,
// from its first line: every write is idempotent on the row id, so re-reading
// what was already applied changes nothing. Once the backfill's restarts are
// spent (maxDownloadRestarts) it is the error instead, which fails the
// backfill.
func (c *Connector) downloadGone(store Store, spec *generated.TypeSpec, cur backfillCursor, gone bulkDownloadGone) (memqlsync.BackfillPage, error) {
	if cur.Restarts >= maxDownloadRestarts {
		return memqlsync.BackfillPage{}, gone
	}
	c.logger.Warn("shopify: the bulk download is gone; starting the operation again",
		"store", store.ID, "concept", spec.Concept, "status", gone.Status, "restart", cur.Restarts+1)
	return memqlsync.BackfillPage{NextCursor: backfillCursor{OpIndex: cur.OpIndex, Restarts: cur.Restarts + 1, Rule: cur.Rule}.encode()}, nil
}

// advanceOperation moves to the next bulk operation of a multi-part domain,
// or reports the domain done. The next part is a new export, with a start of
// its own.
func (c *Connector) advanceOperation(cur backfillCursor, spec *generated.TypeSpec, note string) (memqlsync.BackfillPage, error) {
	if cur.OpIndex+1 < len(spec.BulkOps) {
		return memqlsync.BackfillPage{
			NextCursor: backfillCursor{OpIndex: cur.OpIndex + 1, Restarts: cur.Restarts, Rule: cur.Rule}.encode()}, nil
	}
	return c.finished(cur), nil
}

// finished is a backfill's last step: done, with a cursor naming the rule it
// read the domain under, which the boot pass reads (queueUnstartedBackfills).
func (c *Connector) finished(cur backfillCursor) memqlsync.BackfillPage {
	return memqlsync.BackfillPage{Done: true, NextCursor: backfillCursor{Rule: cur.Rule}.encode()}
}

// underRule reports whether a cursor belongs to a backfill begun under
// originVersionRule, whose every export records when it was started.
func (c backfillCursor) underRule() bool {
	return c.Rule >= originVersionRule && (c.OperationID == "" || c.At != "")
}

func (c *Connector) startBulkOperation(ctx context.Context, store Store, spec *generated.TypeSpec, opIndex int) (bulkOperation, error) {
	document, err := operationBody(spec.BulkDocument, spec.BulkOps[opIndex])
	if err != nil {
		return bulkOperation{}, err
	}
	resp, err := c.adminCall(ctx, store, bulkRunMutation, "ShopifyBulkRun", map[string]any{"query": document})
	if err != nil {
		return bulkOperation{}, err
	}
	var decoded struct {
		BulkOperationRunQuery struct {
			BulkOperation bulkOperation `json:"bulkOperation"`
			UserErrors    []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"userErrors"`
		} `json:"bulkOperationRunQuery"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return bulkOperation{}, err
	}
	for _, ue := range decoded.BulkOperationRunQuery.UserErrors {
		if ue.Code == bulkOperationInProgress {
			// Another export of this shop is running -- another domain's,
			// one a paused domain left behind, or one started elsewhere.
			// Busy, not failed: the runtime asks again on its next round.
			return bulkOperation{}, memqlsync.Busy(ConnectorName, ue.Message)
		}
	}
	if ueErr := userErrorsFrom(resp, "bulkOperationRunQuery"); ueErr != nil {
		return bulkOperation{}, ueErr
	}
	if decoded.BulkOperationRunQuery.BulkOperation.ID == "" {
		return bulkOperation{}, fmt.Errorf("shopify: bulkOperationRunQuery returned no operation for %s", spec.Concept)
	}
	return decoded.BulkOperationRunQuery.BulkOperation, nil
}

// errBulkOperationUnknown is the origin answering that it knows no bulk
// operation by the id asked for.
var errBulkOperationUnknown = errors.New("shopify: the origin knows no such bulk operation")

// bulkStatus asks the origin for one bulk operation: its state and, once it
// is complete, the link of its file. An operation the origin does not know is
// errBulkOperationUnknown.
func (c *Connector) bulkStatus(ctx context.Context, store Store, id string) (bulkOperation, error) {
	resp, err := c.adminCall(ctx, store, bulkStatusQuery, "ShopifyBulkStatus", map[string]any{"id": id})
	if err != nil {
		return bulkOperation{}, err
	}
	var decoded struct {
		Node bulkOperation `json:"node"`
	}
	if err := resp.DecodeInto(&decoded); err != nil {
		return bulkOperation{}, err
	}
	if decoded.Node.ID == "" {
		return bulkOperation{}, fmt.Errorf("%w: %s", errBulkOperationUnknown, id)
	}
	return decoded.Node, nil
}

// operationBody extracts ONE named operation from a multi-operation document.
//
// bulkOperationRunQuery takes a query STRING, and Shopify refuses a document
// carrying more than one operation. A domain split across parts therefore has
// to hand over exactly the part being run.
func operationBody(document, operation string) (string, error) {
	marker := "query " + operation + "("
	idx := strings.Index(document, marker)
	if idx < 0 {
		marker = "query " + operation + " "
		idx = strings.Index(document, marker)
	}
	if idx < 0 {
		return "", fmt.Errorf("shopify: operation %q not found in the generated bulk document", operation)
	}
	rest := document[idx:]
	// Find the matching close brace of the operation body.
	depth := 0
	for i, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1], nil
			}
		}
	}
	return "", fmt.Errorf("shopify: operation %q is not balanced in the generated bulk document", operation)
}

// streamBulk downloads the JSONL and maps up to `limit` lines starting at
// `skip`, returning the writes, how many lines were consumed, and whether the
// file ended.
//
// Every line carries readAt, when its export was started (BackfillStore).
func (c *Connector) streamBulk(ctx context.Context, store Store, spec *generated.TypeSpec, url string, skip, limit int, readAt time.Time) ([]memqlsync.MirrorWrite, int, bool, error) {
	// The URL arrived inside a bulk-operation status rather than from a
	// caller, which makes it more trusted than a request parameter and not
	// trusted enough to fetch unchecked: the status came from a host named on
	// the store row. checkBulkDownloadURL guards this hop and the client
	// guards every redirect after it.
	if err := c.admin.checkDownloadURL(url); err != nil {
		return nil, 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, false, downloadFailed(store, spec, url, err)
	}
	resp, err := bulkDownloadClient(c.admin.http, c.admin.checkDownloadURL).Do(req)
	if err != nil {
		return nil, 0, false, downloadFailed(store, spec, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A signed URL is valid for one week. Past that the download is
		// refused (403, or 400, 404, 410), and the only recovery is a fresh
		// operation -- said as bulkDownloadGone, which BackfillStore turns
		// into a restart of the operation, rather than as a silent zero.
		// Anything else is the download failing, not the link being gone.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			return nil, 0, false, bulkDownloadGone{Status: resp.StatusCode}
		}
		return nil, 0, false, fmt.Errorf("shopify: bulk download HTTP %d (a signed URL expires after one week; restart the operation)", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var writes []memqlsync.MirrorWrite
	// parents holds the origin version of each parent line seen, so a child
	// line that publishes no version of its own carries its parent's. A
	// child follows its parent in the file, and a batch may begin between
	// them: the last parent line skipped is read for that, and only that.
	parents := map[string]string{}
	lastParent := ""
	line := 0
	consumed := 0
	for scanner.Scan() {
		line++
		if line <= skip {
			if raw := strings.TrimSpace(scanner.Text()); raw != "" && !strings.Contains(raw, `"__parentId":`) {
				lastParent = raw
			}
			continue
		}
		if lastParent != "" {
			if obj, err := decodeJSONObject(lastParent); err == nil {
				c.mapBulkLineUnder(store, spec, obj, parents, readAt)
			}
			lastParent = ""
		}
		if consumed >= limit {
			return writes, consumed, false, nil
		}
		consumed++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		obj, err := decodeJSONObject(raw)
		if err != nil {
			// One malformed line does not fail the export. It is logged
			// without its content -- a bulk line is a mirrored payload --
			// and the row it described is picked up by reconciliation.
			c.logger.Warn("shopify: skipping unparseable bulk line", "store", store.ID, "concept", spec.Concept, "line", line)
			continue
		}
		writes = append(writes, c.mapBulkLineUnder(store, spec, obj, parents, readAt)...)
	}
	if err := scanner.Err(); err != nil {
		return nil, consumed, false, downloadFailed(store, spec, url, err)
	}
	return writes, consumed, true, nil
}

// downloadFailed says that a bulk download failed and why, and never where
// from.
//
// The link opens a store's whole export for a week, and this error is the
// backfill's: the runner keeps it on the domain's health row, which the Store
// page and Cluster > Data draw, and logs it. net/http's *url.Error quotes the
// whole link, query string and all, and a dial error names the host and port,
// so neither is passed on: what is kept is the cause -- the syscall error under
// a dial, the resolver's answer under a lookup -- and whether it timed out. Any
// text that still carries a part of the link is said without its cause.
func downloadFailed(store Store, spec *generated.TypeSpec, link string, err error) error {
	timedOut := false
	var ue *neturl.Error
	if errors.As(err, &ue) {
		timedOut = ue.Timeout()
		err = ue.Err
	}
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.As(err, &dns):
		timedOut = timedOut || dns.IsTimeout
		err = errors.New("the download host could not be resolved: " + dns.Err)
	case errors.As(err, &op):
		timedOut = timedOut || op.Timeout()
		if op.Err != nil {
			err = op.Err
		} else {
			err = errors.New(op.Op + " failed")
		}
	}
	if err == nil || carriesLink(err.Error(), link) {
		err = errors.New("the cause named the download link and is not repeated")
	}
	how := "failed"
	if timedOut || errors.Is(err, context.DeadlineExceeded) {
		how = "timed out"
	}
	return fmt.Errorf("shopify: the bulk download of %s for store %s %s: %w", spec.Concept, store.ID, how, err)
}

// carriesLink reports whether text holds any part of a download link that
// would identify or open it: the link, its host, its path or its query.
func carriesLink(text, link string) bool {
	parts := []string{link}
	if u, err := neturl.Parse(link); err == nil {
		parts = append(parts, u.Host, u.Hostname(), u.EscapedPath(), u.Path, u.RawQuery)
		for _, values := range u.Query() {
			parts = append(parts, values...)
		}
	}
	for _, part := range parts {
		if len(part) > 1 && strings.Contains(text, part) {
			return true
		}
	}
	return false
}

// mapBulkLine turns one JSONL line into writes.
//
// A line is either a PARENT (no __parentId) or a CHILD. Which child concept
// it is comes from its GID -- gid://shopify/LineItem/1 is a LineItem -- which
// is more reliable than position: the JSONL interleaves the children of
// different connections, so the only thing that identifies a line is what it
// says it is.
func (c *Connector) mapBulkLine(store Store, spec *generated.TypeSpec, obj map[string]any) []memqlsync.MirrorWrite {
	return c.mapBulkLineUnder(store, spec, obj, nil, c.now())
}

// mapBulkLineUnder is mapBulkLine within one stream, read at readAt: a parent
// line versioned by its updatedAt records it in parents (when parents is not
// nil), and a child line carries its parent's where it has none of its own
// (mapObjectUnder).
func (c *Connector) mapBulkLineUnder(store Store, spec *generated.TypeSpec, obj map[string]any, parents map[string]string, readAt time.Time) []memqlsync.MirrorWrite {
	parentGID, _ := obj["__parentId"].(string)
	gid, _ := obj["id"].(string)
	lineSpec := spec
	switch {
	case parentGID == "" && gid == "":
		return nil
	case parentGID != "" && gid != "":
		lineSpec = specForGID(gid)
	case parentGID != "":
		// A child that declares no id says nothing about its type; it is
		// whichever of the parent's id-less children its key satisfies.
		lineSpec = idlessChildOf(spec, obj)
	}
	if lineSpec == nil {
		// A child of a type this build does not mirror, or one no single
		// child claims. Skipped silently: the bulk query only asks for
		// what the allowlist names, so this means the allowlist changed
		// under a running operation.
		return nil
	}
	writes := mapObjectUnder(lineSpec, store.ID, obj, parentGID, parents[parentGID], readAt)
	if parentGID == "" && parents != nil && lineSpec.VersionField == "updatedAt" && len(writes) > 0 && writes[0].Payload["versionSource"] == memqlsync.VersionFromOrigin {
		parents[gid] = writes[0].Version
	}
	return writes
}

// specForGID resolves the mirrored type a GID names.
func specForGID(gid string) *generated.TypeSpec {
	typeName := gidType(gid)
	if typeName == "" {
		return nil
	}
	for _, spec := range generated.Types {
		if spec.GraphQLType == typeName {
			return spec
		}
	}
	return nil
}

func decodeJSONObject(raw string) (map[string]any, error) {
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// onBulkOperationFinish handles the bulk_operations/finish topic: Shopify
// telling us an export we started is ready.
//
// It does not do the work, and it does not hurry it: it logs the notice and
// marks the operation in bulkReadySet, which nothing reads. The backfill's
// next step asks the origin for the operation's status as every step does
// (BackfillStore), finds it complete and reads its file.
func (c *Connector) onBulkOperationFinish(ctx context.Context, store Store, req memqlsync.InboundRequest) error {
	obj, err := decodeJSONObject(string(req.Body))
	if err != nil {
		return nil
	}
	opID := firstString(obj, "admin_graphql_api_id", "id")
	if opID == "" {
		return nil
	}
	c.logger.Info("shopify: bulk operation finished", "store", store.ID, "operation", opID)
	c.bulkReady.mark(opID, c.now())
	return nil
}

// bulkReadySet records which operations Shopify has said are finished.
// Process-local, and read by nothing: no step skips its status poll for a
// marked operation, so a missed notification costs nothing at all.
type bulkReadySet struct {
	applied *applied
}

func (b *bulkReadySet) mark(id string, now time.Time) {
	if b == nil || b.applied == nil {
		return
	}
	b.applied.check(id, now)
}

var _ = io.EOF
