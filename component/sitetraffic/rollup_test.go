package sitetraffic

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
)

// The window a run looks for open days in is the request log's configured
// retention, counted back from today: a run never asks the log where its oldest
// row is (that row sits in a compressed chunk, so the asking would decompress
// it every five minutes), and never reaches past the 400 days the daily table
// keeps, should the retention ever be configured longer than that.
func TestTheOpenWindowIsTheRequestLogsRetention(t *testing.T) {
	today := time.Date(2036, 6, 15, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		retentionDays int
		want          time.Time
	}{
		{30, time.Date(2036, 5, 16, 0, 0, 0, 0, time.UTC)},
		{1, time.Date(2036, 6, 14, 0, 0, 0, 0, time.UTC)},
		// 2036 is a leap year, so 365 days back from 2036-06-15 is 2035-06-16.
		{365, time.Date(2035, 6, 16, 0, 0, 0, 0, time.UTC)},
		// Capped at the daily table's 400 days: 2035-05-12.
		{500, time.Date(2035, 5, 12, 0, 0, 0, 0, time.UTC)},
	} {
		if got := openFrom(today, c.retentionDays); !got.Equal(c.want) {
			t.Errorf("openFrom(%s, %d) = %s, want %s", today.Format(time.DateOnly), c.retentionDays, got.Format(time.DateOnly), c.want.Format(time.DateOnly))
		}
	}
}

// NO MAINTENANCE SESSION, NO ROLLUP. The plug-in contract
// (memql.PluginContext.OpenMaintenanceSession) has a plug-in that needs the
// session refuse the capability rather than fall back to the request pool,
// whose statement timeout and 10 s read deadline end a day's recompute part-way
// -- the reason the session exists. So a node without one answers an error that
// says so, and never reaches for the pool; and a session that cannot be opened
// ends the run with the opener's own error, having done nothing.
func TestTheRollupRefusesANodeWithoutAMaintenanceSession(t *testing.T) {
	touched := false
	i := NewIntegration(nil, func() *bun.DB { touched = true; return nil }, nil, quietLogger())
	ctx := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor(auth.AutomationOriginEngine, "siteStatisticsRollupSweep"))
	nodes, err := i.handleRollup(ctx, nil, 0)
	if err == nil || nodes != nil || !strings.Contains(err.Error(), "maintenance session") {
		t.Errorf("got %d nodes and error %v, want a refusal naming the maintenance session", len(nodes), err)
	}
	if touched {
		t.Error("the rollup reached for the request pool instead of refusing")
	}
	if _, err := Rollup(context.Background(), nil, time.Now()); !errors.Is(err, errNoMaintenanceSession) {
		t.Errorf("Rollup with no session answered %v, want errNoMaintenanceSession", err)
	}

	unavailable := errors.New("maintenance session unavailable on this database")
	opened := 0
	rep, err := Rollup(context.Background(), func(context.Context) (*sql.Conn, func(), error) {
		opened++
		return nil, nil, unavailable
	}, time.Now())
	if !errors.Is(err, unavailable) || opened != 1 || rep != (RollupReport{}) {
		t.Errorf("a session that cannot be opened answered %+v, %v after %d opens; want the opener's error after one, and nothing done", rep, err, opened)
	}
}
