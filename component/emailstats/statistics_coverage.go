package emailstats

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// statistics_coverage.go -- how far a read's figures are measured: the
// cluster's first recorded day and each source's walk start from below, and
// from above how far the rollup has walked each source (statistics.go,
// "Absent is never zero").

// coverageGrace is how far behind the read's clock a source may have been
// walked with the range under way still counted as walked: three of the
// rollup's five-minute runs (emailStatisticsRollupSweep,
// dsl/campaigns/automations.memql). A healthy rollup records each source as
// walked through the clock it asked the source's last page by, every run
// (rollup.go, "Walked, nothing there"), so a read made between two runs finds
// each up to one run old and must not show today's figures as missing for it;
// three runs old belongs to a rollup that has stopped or is catching up. The
// price is that the range under way, and one that ended within this long of
// the read, may lack those last minutes' facts.
//
// ONLY THE RANGE UNDER WAY. A range that ended longer ago than this needs its
// sources walked through its end: given the grace too, a rollup that stopped
// in a day's last quarter hour would leave that day measured but short, while
// every bucket after it read as null
// (TestAFinishedDayIsMeasuredOnlyOnceItsSourcesAreWalkedThroughItsEnd).
const coverageGrace = 15 * time.Minute

// The sources each figure is counted from (facts.go): a figure is measured
// only where all of them have been walked. A rate is its sends and their opens.
var (
	fromDeliveries = []string{sourceDelivery}
	fromEngagement = []string{sourceEngagement}
	fromBoth       = []string{sourceDelivery, sourceEngagement}
	fromConsent    = []string{sourceConsent}
)

// coverage is what a read's figures are judged measured by: the cluster's
// first recorded day, where each source's walk starts and how far it has
// read, in the read's own snapshot (statistics.go, "Absent is never zero").
type coverage struct {
	since time.Time
	// walked is how far each source has been walked (walked_through, rollup.go,
	// "Never past the clock"); a source with none was never walked.
	walked map[string]time.Time
	// startsAt is where a source's walk starts when a first walk that could
	// not read its first page moved it (rollup.go): the walk never reads what
	// is before it. A source with none starts at the retention horizon, which
	// since already bounds.
	startsAt map[string]time.Time
	// now is the read's clock: a range under way is walked through it at
	// most.
	now time.Time
}

// covers reports whether a range [start, end) is measured for a figure
// counted from these sources: it ends after the first recorded day, and each
// source's walk began no later than its start and has been walked through its
// end. A range under way -- one that ends after the read's clock less
// coverageGrace -- needs its sources walked only through the sooner of its end
// and that clock, less the grace. A range that straddles a source's start is
// not measured for it: the part before the start was never read.
func (c coverage) covers(start, end time.Time, sources ...string) bool {
	if !end.After(c.since) {
		return false
	}
	need := end
	if end.After(c.now.Add(-coverageGrace)) {
		if c.now.Before(need) {
			need = c.now
		}
		need = need.Add(-coverageGrace)
	}
	for _, s := range sources {
		through, ok := c.walked[s]
		if !ok || through.Before(need) {
			return false
		}
		if from, moved := c.startsAt[s]; moved && start.Before(from) {
			return false
		}
	}
	return true
}

// readCoverage reads where each source's walk starts and how far it has read
// (rollup.go), in the read's transaction.
func readCoverage(ctx context.Context, tx bun.Tx, since, now time.Time) (coverage, error) {
	c := coverage{since: since, now: now, walked: map[string]time.Time{}, startsAt: map[string]time.Time{}}
	var walked []struct {
		Source  string    `bun:"source"`
		Through time.Time `bun:"walked_through"`
	}
	if err := tx.NewRaw(`SELECT source, walked_through FROM email_stat_watermark WHERE walked_through IS NOT NULL`).Scan(ctx, &walked); err != nil {
		return coverage{}, fmt.Errorf("read how far the rollup has walked: %w", err)
	}
	for _, r := range walked {
		c.walked[r.Source] = r.Through
	}
	var starts []struct {
		Source   string    `bun:"source"`
		StartsAt time.Time `bun:"starts_at"`
	}
	if err := tx.NewRaw(`SELECT source, starts_at FROM email_stat_walk_start`).Scan(ctx, &starts); err != nil {
		return coverage{}, fmt.Errorf("read where the rollup's walks start: %w", err)
	}
	for _, r := range starts {
		c.startsAt[r.Source] = r.StartsAt
	}
	return c, nil
}
