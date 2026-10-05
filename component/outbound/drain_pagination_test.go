package outbound

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

type pagedDeliveryEngine struct {
	*fakeEngine
	pages   map[string]*memql.ExecuteResult
	cursors map[string][]string
}

func (e *pagedDeliveryEngine) Execute(ctx context.Context, q string) (any, error) {
	if !strings.HasPrefix(q, "query outboundRequestsByStatus") {
		return e.fakeEngine.Execute(ctx, q)
	}
	status := "pending"
	if strings.Contains(q, `status: "retrying"`) {
		status = "retrying"
	}
	cursor := memql.CursorFromContext(ctx)
	e.cursors[status] = append(e.cursors[status], cursor)
	if page := e.pages[status+":"+cursor]; page != nil {
		return page, nil
	}
	return memql.NewResultWithOutput([]map[string]any{}), nil
}
func deliveryPage(rows []map[string]any, cursor string) *memql.ExecuteResult {
	r := memql.NewResultWithOutput(rows)
	r.GetMeta().Cursor = cursor
	return r
}

func TestDrainAdvancesPastOtherMediaAndFutureRetries(t *testing.T) {
	now := time.Now().UTC()
	var pending, retrying []map[string]any
	for i := 0; i < 50; i++ {
		row := pendingRow(fmt.Sprintf("peer-%d", i))
		row["medium"] = "email"
		pending = append(pending, row)
		future := pendingRow(fmt.Sprintf("future-%d", i))
		future["status"] = "retrying"
		future["nextAttemptAt"] = now.Add(time.Hour).Format(time.RFC3339)
		retrying = append(retrying, future)
	}
	due := pendingRow("due")
	due["status"] = "retrying"
	due["nextAttemptAt"] = now.Add(-time.Minute).Format(time.RFC3339)
	eng := &pagedDeliveryEngine{fakeEngine: &fakeEngine{}, cursors: map[string][]string{}, pages: map[string]*memql.ExecuteResult{
		"pending:":             deliveryPage(pending, "pending-next"),
		"retrying:":            deliveryPage(retrying, "retry-next"),
		"pending:pending-next": deliveryPage([]map[string]any{pendingRow("ready")}, "pending-end"),
		"retrying:retry-next":  deliveryPage([]map[string]any{due}, "retry-end"),
	}}
	transport := &fakeTransport{}
	worker := newTestWorker(eng.fakeEngine, transport, nil)
	worker.engine = eng
	worker.cfg.EmailAllowlist = nil
	worker.now = func() time.Time { return now }
	worker.drainOnce(context.Background())
	if transport.count() != 0 || len(eng.stamped()) != 0 {
		t.Fatal("skipped rows were delivered or modified")
	}
	worker.drainOnce(context.Background())
	if transport.count() != 2 {
		t.Fatalf("later deliveries starved: %d", transport.count())
	}
	worker.drainOnce(context.Background()) // exhaustion resets each independent cursor
	worker.drainOnce(context.Background()) // revisit the skipped first pages
	for status, cursors := range eng.cursors {
		if len(cursors) != 4 || cursors[0] != "" || cursors[1] == "" || cursors[2] == "" || cursors[3] != "" {
			t.Errorf("%s did not continue then reset: %v", status, cursors)
		}
	}
	if transport.count() != 2 {
		t.Fatal("revisiting skipped rows caused delivery")
	}
}
