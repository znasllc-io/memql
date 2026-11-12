package shopify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// heal_fence_db_test.go -- a sweep's write against the engine that enforces
// its fence: a delivery that commits between heal's read and its write is not
// replaced by the older state the sweep's page carried.

// overtakenEngine is the real engine with a delivery in it: just before the
// first mirror write of one row reaches the engine, `meanwhile` runs.
type overtakenEngine struct {
	memql.IntegrationEngineAccess
	rowID     string
	meanwhile func()
	once      stdsync.Once
}

func (e *overtakenEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "insert(") && strings.Contains(q, e.rowID) {
		e.once.Do(e.meanwhile)
	}
	return e.IntegrationEngineAccess.Execute(ctx, q)
}

func TestAHealOvertakenByADeliveryDoesNotPutTheMirrorBack(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	cleanMirrored(t, db, "product")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	plain := &Connector{engine: eng, logger: logger, now: time.Now}
	spec := generated.Types["product"]
	state := func(n int, title, updatedAt string, read time.Time) (string, []memqlsync.MirrorWrite) {
		gid := fmt.Sprintf("gid://shopify/Product/%d", 15000+n)
		return gid, mapObject(spec, authorityTestStoreId, map[string]any{"id": gid, "title": title, "updatedAt": updatedAt}, "", read)[:1]
	}
	title := func(gid string) string {
		t.Helper()
		var out string
		if err := db.QueryRowContext(context.Background(),
			`SELECT payload->>'title' FROM "MemoryNodes" WHERE concept = $1 AND payload->>'gid' = $2 AND payload->>'storeId' = $3 ORDER BY "createdAt" DESC LIMIT 1`,
			generated.ConceptID("product"), gid, authorityTestStoreId).Scan(&out); err != nil {
			t.Fatalf("read the title: %v", err)
		}
		return out
	}
	day := func(clock string) time.Time {
		at, err := time.Parse(time.RFC3339, "2026-11-11T"+clock+"Z")
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	for n, tc := range []struct {
		name      string
		held      string // the state held before, "" for no row
		meanwhile string // the state a delivery commits in between
		healed    int
		want      string
	}{
		{name: "a delivery wrote a newer state meanwhile", held: "2026-11-11T10:00:00Z", meanwhile: "2026-11-11T13:00:00Z", want: "a delivery"},
		{name: "a delivery wrote an older state meanwhile", held: "2026-11-11T10:00:00Z", meanwhile: "2026-11-11T11:00:00Z", healed: 1, want: "the sweep's page"},
		{name: "a delivery created the row meanwhile, newer", meanwhile: "2026-11-11T13:00:00Z", want: "a delivery"},
		{name: "nobody else wrote", held: "2026-11-11T10:00:00Z", healed: 1, want: "the sweep's page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gid, page := state(n, "the sweep's page", "2026-11-11T12:00:00Z", day("12:25:00"))
			if tc.held != "" {
				_, held := state(n, "held", tc.held, day("10:30:00"))
				if healed, failed := plain.heal(context.Background(), held); healed != 1 || failed != 0 {
					t.Fatalf("seed the held row: healed %d, failed %d", healed, failed)
				}
			}
			racing := &overtakenEngine{IntegrationEngineAccess: eng, rowID: MirrorRowID(authorityTestStoreId, gid)}
			racing.meanwhile = func() {
				if tc.meanwhile == "" {
					return
				}
				_, delivered := state(n, "a delivery", tc.meanwhile, day("12:20:00"))
				if healed, failed := plain.heal(context.Background(), delivered); healed != 1 || failed != 0 {
					t.Errorf("the delivery's write: healed %d, failed %d", healed, failed)
				}
			}
			c := &Connector{engine: racing, logger: logger, now: time.Now}
			healed, failed := c.heal(context.Background(), page)
			if healed != tc.healed || failed != 0 {
				t.Fatalf("healed %d, failed %d; want %d and 0", healed, failed, tc.healed)
			}
			if got := title(gid); got != tc.want {
				t.Fatalf("the mirror holds %q, want %q", got, tc.want)
			}
		})
	}
}
