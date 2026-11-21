package calls

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise native query limits with more than the engine's default 500 roots.
// All rows are benign fixtures in this test's isolated site/database.
func TestReportsBudgetIncludesLaterPagesAndStopsAtSiteCeiling(t *testing.T) {
	cw := newCallWorld(t, "reports-budget-pages", 20)
	if cw == nil {
		return
	}
	enableReports(t, cw)
	ctx := context.Background()
	now := cw.clock.Now()
	for n := 0; n < 1000; n++ {
		key, room := fmt.Sprintf("budget-key-%04d", n), fmt.Sprintf("budget-room-%04d", n)
		if n >= 750 && n < 755 {
			key = "target-key"
		}
		if n >= 800 && n < 900 {
			room = "target-room"
		}
		r := &reportCase{ID: fmt.Sprintf("budget-case-%04d", n), SiteID: cw.site, ReporterKeyID: key, RoomID: room, IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour), Revision: 1, State: reportState{Category: "other", Description: "Benign budget fixture", Status: "submitted"}}
		require.NoError(t, cw.svc.store.saveReport(ctx, cw.owner, r, now))
		switch n {
		case 599:
			require.NoError(t, cw.svc.store.reportBudget(ctx, cw.owner, cw.site, "unused-key", "unused-room", now), "more than one page can still be below every budget")
		case 799:
			require.ErrorContains(t, cw.svc.store.reportBudget(ctx, cw.owner, cw.site, "target-key", "unused-room", now), "report_rate_limited", "a key's five reports on later pages must count")
		case 899:
			require.ErrorContains(t, cw.svc.store.reportBudget(ctx, cw.owner, cw.site, "unused-key", "target-room", now), "report_rate_limited", "a room's hundred reports on later pages must count")
		}
	}
	require.ErrorContains(t, cw.svc.store.reportBudget(ctx, cw.owner, cw.site, "unused-key", "unused-room", now), "report_rate_limited", "the site ceiling applies even with unique reporters and rooms")
	require.NoError(t, cw.svc.store.reportBudget(ctx, cw.owner, cw.site, "unused-key", "unused-room", now.Add(24*time.Hour)), "the rolling window excludes reports exactly 24 hours old")
	require.NoError(t, cw.svc.store.reportBudget(ctx, cw.owner, "different-site", "unused-key", "unused-room", now), "another site has a separate budget")
	require.NoError(t, cw.svc.store.reportBudget(ctx, "different-owner", cw.site, "unused-key", "unused-room", now), "the inventory never crosses owner scope")
}
