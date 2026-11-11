package shopify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
type backfillCursor struct {
	// OperationID is the running (or completed) bulk operation.
	OperationID string `json:"op,omitempty"`
	// OpIndex is which of a multi-part domain's operations this is.
	OpIndex int `json:"i,omitempty"`
	// Line is how many JSONL lines have already been applied. Resuming
	// re-downloads and skips: the signed URL is valid for a week and
	// re-reading a prefix is cheaper than any bookkeeping that would let
	// us seek.
	Line int `json:"line,omitempty"`
	// URL is the signed download, remembered so a resume does not have to
	// re-poll a completed operation.
	URL string `json:"url,omitempty"`
	// Restarts counts the operations this backfill started again because the
	// origin refused a download as gone (bulkDownloadGone). It is the
	// backfill's, not one operation's: it is carried across the parts of a
	// multi-part domain and bounded by maxDownloadRestarts.
	Restarts int `json:"r,omitempty"`
}

// maxDownloadRestarts is how many times one backfill starts an operation again
// after its download was refused as gone. The next refusal fails the backfill:
// a link that is refused on a fresh operation too is not an expired link, and
// starting again forever would hide it.
const maxDownloadRestarts = 2

// bulkDownloadGone is a bulk download the origin refused as no longer there
// (400, 403, 404 or 410): the signed link has expired, or the file with it.
// Only a fresh operation recovers from it. Any other failure of the download
// -- a 5xx, a 429, a broken connection -- is not this error.
type bulkDownloadGone struct{ Status int }

func (e bulkDownloadGone) Error() string {
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
	if cur.OpIndex >= len(spec.BulkOps) {
		return memqlsync.BackfillPage{Done: true}, nil
	}

	// 1. No operation yet: start one.
	if cur.OperationID == "" {
		op, err := c.startBulkOperation(ctx, store, spec, cur.OpIndex)
		if err != nil {
			return memqlsync.BackfillPage{}, err
		}
		cur.OperationID = op.ID
		cur.Line = 0
		cur.URL = ""
		return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
	}

	// 2. No download URL yet: poll.
	if cur.URL == "" {
		op, err := c.bulkStatus(ctx, store, cur.OperationID)
		if err != nil {
			return memqlsync.BackfillPage{}, err
		}
		switch op.Status {
		case "COMPLETED":
			if op.URL == "" {
				// A completed operation with no URL means the query
				// matched nothing. That is a legitimate empty domain and
				// the step is done, not stuck.
				return c.advanceOperation(cur, spec, "no rows")
			}
			cur.URL = op.URL
			cur.Line = 0
		case "CREATED", "RUNNING":
			return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
		case "EXPIRED", "CANCELED", "FAILED":
			// A bulk operation must finish within ten days and its file
			// expires a week after that. Restarting is the only recovery,
			// and it is safe: every write is idempotent on the row id.
			c.logger.Warn("shopify: restarting bulk operation", "store", store.ID, "concept", spec.Concept, "status", op.Status, "errorCode", op.ErrorCode)
			return memqlsync.BackfillPage{NextCursor: backfillCursor{OpIndex: cur.OpIndex, Restarts: cur.Restarts}.encode()}, nil
		default:
			return memqlsync.BackfillPage{NextCursor: cur.encode()}, nil
		}
	}

	// 3. Stream the next batch of lines.
	writes, consumed, eof, err := c.streamBulk(ctx, store, spec, cur.URL, cur.Line, backfillBatchLines)
	var gone bulkDownloadGone
	if errors.As(err, &gone) && cur.Restarts < maxDownloadRestarts {
		// The link outlived its week (a backfill paused or stopped past it),
		// or the file went with it. The operation and its link are dropped and
		// the SAME part starts again on the next step, from its first line:
		// every write is idempotent on the row id, so re-reading what was
		// already applied changes nothing.
		c.logger.Warn("shopify: the bulk download is gone; starting the operation again",
			"store", store.ID, "concept", spec.Concept, "status", gone.Status, "restart", cur.Restarts+1)
		return memqlsync.BackfillPage{NextCursor: backfillCursor{OpIndex: cur.OpIndex, Restarts: cur.Restarts + 1}.encode()}, nil
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

// advanceOperation moves to the next bulk operation of a multi-part domain,
// or reports the domain done.
func (c *Connector) advanceOperation(cur backfillCursor, spec *generated.TypeSpec, note string) (memqlsync.BackfillPage, error) {
	if cur.OpIndex+1 < len(spec.BulkOps) {
		return memqlsync.BackfillPage{
			NextCursor: backfillCursor{OpIndex: cur.OpIndex + 1, Restarts: cur.Restarts}.encode()}, nil
	}
	return memqlsync.BackfillPage{Done: true}, nil
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
			// Busy, not failed: the runtime asks again on its next tick.
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
		return bulkOperation{}, fmt.Errorf("shopify: bulk operation %s not found", id)
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
func (c *Connector) streamBulk(ctx context.Context, store Store, spec *generated.TypeSpec, url string, skip, limit int) ([]memqlsync.MirrorWrite, int, bool, error) {
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
		return nil, 0, false, err
	}
	resp, err := bulkDownloadClient(c.admin.http, c.admin.checkDownloadURL).Do(req)
	if err != nil {
		return nil, 0, false, fmt.Errorf("shopify: download bulk result: %w", err)
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
	line := 0
	consumed := 0
	for scanner.Scan() {
		line++
		if line <= skip {
			continue
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
		writes = append(writes, c.mapBulkLine(store, spec, obj)...)
	}
	if err := scanner.Err(); err != nil {
		return nil, consumed, false, fmt.Errorf("shopify: read bulk stream: %w", err)
	}
	return writes, consumed, true, nil
}

// mapBulkLine turns one JSONL line into writes.
//
// A line is either a PARENT (no __parentId) or a CHILD. Which child concept
// it is comes from its GID -- gid://shopify/LineItem/1 is a LineItem -- which
// is more reliable than position: the JSONL interleaves the children of
// different connections, so the only thing that identifies a line is what it
// says it is.
func (c *Connector) mapBulkLine(store Store, spec *generated.TypeSpec, obj map[string]any) []memqlsync.MirrorWrite {
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
	return mapObject(lineSpec, store.ID, obj, parentGID, c.now())
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
// It does not do the work. It clears the domain's WAIT so the next runner
// tick picks it up immediately instead of on the poll interval -- which is
// the difference between a backfill that finishes in minutes and one that
// finishes on the hour.
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

// bulkReadySet records which operations Shopify has said are finished, so a
// runner can skip the status poll for them. Process-local and advisory: a
// missed notification costs one poll, never a stuck backfill.
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
