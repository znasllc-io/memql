package worker

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// app_session_sweep_defaults_test.go -- the stale sweep's defaults are DSL
// literals judged against cadences this package owns.
//
// workerAppSessionStaleSweep (dsl/worker/logic.memql) fails a live session
// whose heartbeat is older than its STALL GRACE, and nothing in the DSL knows
// how often the heartbeat is written: that is recordingPublishInterval, here.
// Raise the interval, or shorten the grace, and the sweep starts failing
// sessions that are merely between beats -- with every gate green, because the
// two numbers never meet in code. This file is where they meet.
//
// integrations/agent/worker pins the other default, the max-age backstop,
// against the Go session ceiling it is derived from.

const appSessionSweepLogic = "../../dsl/worker/logic.memql"

// sweepDefaultSeconds reads the `?? "<n>"` default the sweep logic applies to
// a global variable. It fails rather than guessing when the shape moved: a pin
// that silently matched nothing would pass for the wrong reason.
func sweepDefaultSeconds(t *testing.T, variable string) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(appSessionSweepLogic)
	if err != nil {
		t.Fatalf("read the sweep's logic: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "logic workerAppSessionStaleSweep")
	if start < 0 {
		t.Fatalf("%s no longer declares logic workerAppSessionStaleSweep; move this pin with it", appSessionSweepLogic)
	}
	body := src[start:]
	bind := regexp.MustCompile(`(\w+)\s*:=\s*query globalVariable\(name:\s*"` + regexp.QuoteMeta(variable) + `"\)`).FindStringSubmatch(body)
	if bind == nil {
		t.Fatalf("the sweep no longer reads %s through globalVariable; move this pin with it", variable)
	}
	def := regexp.MustCompile(regexp.QuoteMeta(bind[1]) + `\.first\(\)\.value\s*\?\?\s*"(\d+)"`).FindStringSubmatch(body)
	if def == nil {
		t.Fatalf("the sweep no longer defaults %s with ?? \"<seconds>\"; move this pin with it", variable)
	}
	seconds, err := strconv.Atoi(def[1])
	if err != nil {
		t.Fatalf("the %s default %q is not a whole number of seconds", variable, def[1])
	}
	return time.Duration(seconds) * time.Second
}

// TestTheStallGraceOutlastsManyFlushes: the grace must be many flushes long,
// so a replica that is merely slow -- a missed write, a busy engine -- is never
// taken for a dead one.
func TestTheStallGraceOutlastsManyFlushes(t *testing.T) {
	grace := sweepDefaultSeconds(t, "MEMQL_APP_SESSION_STALL_GRACE_SECONDS")
	if floor := 10 * recordingPublishInterval; grace < floor {
		t.Fatalf("the sweep's stall grace is %s and the holder heartbeats every %s; the grace must be at least "+
			"ten beats (%s) or a slow replica's sessions are failed while they run", grace, recordingPublishInterval, floor)
	}
}

// TestTheUnheldCloseFitsInsideTheStallGrace: the runner releases its hold
// before the recording closes (holdThroughEnd), so the close is the one
// stretch with no heartbeat. It is bounded; the bound must stay inside the
// grace, or a session that is closing cleanly is failed by the sweep.
func TestTheUnheldCloseFitsInsideTheStallGrace(t *testing.T) {
	grace := sweepDefaultSeconds(t, "MEMQL_APP_SESSION_STALL_GRACE_SECONDS")
	unheld := seqAllocationTimeout + closeRecordingTimeout
	if unheld+2*recordingPublishInterval >= grace {
		t.Fatalf("the unheld close can take %s and the stall grace is %s; the close must fit inside the grace "+
			"with a flush to spare on either side", unheld, grace)
	}
}
