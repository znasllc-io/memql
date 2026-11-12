package datasync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// inbound_restage_db_test.go -- what a redelivery does to a delivery that is
// already staged.
//
// A delivery's row id is the digest of its signed payload, so the origin
// sending the same payload again lands on the row of the first. The receiver
// stages it again with the same mutation, and that mutation keeps what the
// row already holds of the delivery's handling (its status, its headers, when
// it was received, who verified it). THE BODY IS ONE OF THOSE NOW. A
// redelivery under the same key carries the same body by definition, so for
// an ordinary delivery nothing changes; but an erasure blanks the body of a
// processed delivery, and a redelivery that wrote it back would put a
// person's name and address into a row nothing looks at again.

// stagedBodies is the body of every stored version of one delivery, oldest
// first, as the table holds them.
func stagedBodies(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT COALESCE(payload->>'body', '<no body key>') FROM "MemoryNodes" WHERE concept = 'v1:platform:inboundRequest' AND id = $1 ORDER BY "createdAt"`,
		"v1:platform:inboundRequest:"+id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

func TestARedeliveryKeepsTheBodyTheRowHolds(t *testing.T) {
	eng, db := healthEngine(t)
	if eng == nil {
		return
	}
	source := fmt.Sprintf("dbtest-restage-%d", time.Now().UnixNano())
	cleanInbound(t, db, source)
	f := newRetryFixture(t, engineSeam{eng}, source)
	f.clock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	restage := func(id, body string) {
		t.Helper()
		if _, err := f.store.exec(f.ctx, call("mutation", "stageInboundRequest",
			arg{"requestId", id}, arg{"source", f.source}, arg{"medium", "webhook"}, arg{"body", body},
			arg{"headersJson", `{"x-shopify-topic":"products/update","x-redelivered":"yes"}`},
			arg{"signatureVerified", true}, arg{"verifiedBy", "connector"},
			arg{"receivedAt", f.clock.Add(time.Hour).Format(time.RFC3339)})); err != nil {
			t.Fatalf("staging %s again: %v", id, err)
		}
	}

	// A first staging stores its body.
	first := f.stage("7001")
	const body = `{"admin_graphql_api_id":"gid://shopify/Product/7001"}`
	if got := stagedBodies(t, db, first); len(got) != 1 || got[0] != body {
		t.Fatalf("a first staging stored %v, want its body", got)
	}

	// An ordinary processed delivery, redelivered: nothing it did not change
	// before changes now. Its status stays, its headers and receipt stay, and
	// its body is the body.
	f.try(first, f.clock, nil)
	before := f.row(first)
	if before["status"] != "processed" {
		t.Fatalf("the fixture's delivery is %v, want processed", before["status"])
	}
	restage(first, body)
	after := f.row(first)
	for _, field := range []string{"status", "body", "headersJson", "receivedAt", "verifiedBy", "attempts"} {
		if fmt.Sprint(after[field]) != fmt.Sprint(before[field]) {
			t.Errorf("a redelivery of an ordinary processed delivery changed %s: %v, was %v", field, after[field], before[field])
		}
	}

	// A delivery whose body was blanked, redelivered: it stays blank.
	blanked := f.stage("7002")
	f.try(blanked, f.clock, nil)
	if _, err := db.ExecContext(context.Background(),
		`UPDATE "MemoryNodes" SET payload = jsonb_set(payload, '{body}', '""'::jsonb, true) WHERE concept = 'v1:platform:inboundRequest' AND id = $1`,
		"v1:platform:inboundRequest:"+blanked); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(stagedBodies(t, db, blanked), "|"); strings.Trim(got, "|") != "" {
		t.Fatalf("the fixture did not blank the delivery: %q", got)
	}
	restage(blanked, `{"admin_graphql_api_id":"gid://shopify/Product/7002"}`)
	for i, got := range stagedBodies(t, db, blanked) {
		if got != "" {
			t.Errorf("version %d of a blanked delivery holds a body again after its redelivery: %s", i, got)
		}
	}
	if got := f.row(blanked); got["status"] != "processed" || got["body"] != "" {
		t.Errorf("the blanked delivery reads status %v and body %q after its redelivery, want it processed and blank", got["status"], got["body"])
	}
}
