package emailstats

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/statsperiod"
)

func TestStatisticsAllAccountBoundRefusesIncompleteAggregate(t *testing.T) {
	db := emptyStatsTables(t)
	_, err := db.ExecContext(context.Background(), `INSERT INTO email_stat_fact(kind,source_id,account_id,day)
 SELECT 'sent', 'bounded-' || n, 'account-' || n, DATE '2041-06-12' FROM generate_series(1,?) AS n`, maxStatisticsAccounts+1)
	if err != nil {
		t.Fatal(err)
	}
	g := &selectiveStatisticsGrants{fakeGrants: fakeGrants{staff()}}
	_, err = readStatistics(context.Background(), db, g.grant, nil, statsperiod.Last7Days, statsNow, g, nil)
	if err == nil || !strings.Contains(err.Error(), "too many organizations") {
		t.Fatalf("oversized discovery was not refused: %v", err)
	}
	for _, asked := range g.asked {
		if !strings.HasPrefix(asked, "/") {
			t.Fatalf("partial discovery reached per-account authorization: %v", g.asked)
		}
	}
}

func TestStatisticsAllAccountDiscoveryFailureIsNotEmptySuccess(t *testing.T) {
	db := emptyStatsTables(t)
	// The private table continues to shadow the shared table. Make only its
	// schema unreadable, without dropping it and exposing the shared fallback.
	_, err := db.ExecContext(context.Background(), `ALTER TABLE email_stat_fact RENAME COLUMN account_id TO unavailable_account`)
	if err != nil {
		t.Fatal(err)
	}
	g := fakeGrants{staff()}
	_, err = readStatistics(context.Background(), db, g.grant, nil, statsperiod.Last7Days, statsNow, g, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve statistics organizations") {
		t.Fatalf("failed discovery was not refused: %v", err)
	}
}

func TestStatisticsAllAccountAuthorizationFailureReturnsNoAggregate(t *testing.T) {
	db := emptyStatsTables(t)
	_, err := db.ExecContext(context.Background(), `INSERT INTO email_stat_fact(kind,source_id,account_id,day) VALUES ('sent','private-one','private-account',DATE '2041-06-12')`)
	if err != nil {
		t.Fatal(err)
	}
	g := &selectiveStatisticsGrants{fakeGrants: fakeGrants{staff()}, failAccount: "private-account"}
	f := &statsFixture{t: t, db: db}
	i := f.integration(g)
	nodes, err := i.handleStatistics(context.Background(), map[string]any{"period": "7d"}, 0)
	if err == nil || !strings.Contains(err.Error(), "capability evidence unavailable") || len(nodes) != 0 {
		t.Fatalf("failed per-account evidence returned an aggregate: nodes=%v error=%v", nodes, err)
	}
}
