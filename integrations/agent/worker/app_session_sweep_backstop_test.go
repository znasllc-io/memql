//go:build agent

package worker

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// app_session_sweep_backstop_test.go -- the stale sweep's max-age backstop is
// a DSL literal derived from a Go constant in this package.
//
// workerAppSessionStaleSweep (dsl/worker/logic.memql) fails ANY live session
// started longer ago than its max age, however recently its holder reported:
// nothing legitimately runs that long. "That long" is defaultAppSessionMaxDuration
// -- the ceiling this package hands every delegated session -- plus a grace, and
// the DSL holds it as a hand copy ("16200", four and a half hours). Raise the Go
// ceiling and the sweep starts failing sessions that are still inside it, with
// every gate green. This is where the two meet.
//
// component/worker pins the other default, the stall grace, against the
// heartbeat cadence it is judged by.

const sweepLogicPath = "../../../dsl/worker/logic.memql"

// backstopGrace is the least slack the max age must leave past the ceiling:
// a session that ends AT its ceiling still has its transcript and recording to
// finish, and the sweep runs every two minutes.
const backstopGrace = 15 * time.Minute

func sweepMaxAgeDefault(t *testing.T) time.Duration {
	t.Helper()
	raw, err := os.ReadFile(sweepLogicPath)
	if err != nil {
		t.Fatalf("read the sweep's logic: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "logic workerAppSessionStaleSweep")
	if start < 0 {
		t.Fatalf("%s no longer declares logic workerAppSessionStaleSweep; move this pin with it", sweepLogicPath)
	}
	body := src[start:]
	bind := regexp.MustCompile(`(\w+)\s*:=\s*query globalVariable\(name:\s*"MEMQL_APP_SESSION_MAX_AGE_SECONDS"\)`).FindStringSubmatch(body)
	if bind == nil {
		t.Fatal("the sweep no longer reads MEMQL_APP_SESSION_MAX_AGE_SECONDS through globalVariable; move this pin with it")
	}
	def := regexp.MustCompile(regexp.QuoteMeta(bind[1]) + `\.first\(\)\.value\s*\?\?\s*"(\d+)"`).FindStringSubmatch(body)
	if def == nil {
		t.Fatal("the sweep no longer defaults MEMQL_APP_SESSION_MAX_AGE_SECONDS with ?? \"<seconds>\"; move this pin with it")
	}
	seconds, err := strconv.Atoi(def[1])
	if err != nil {
		t.Fatalf("the max-age default %q is not a whole number of seconds", def[1])
	}
	return time.Duration(seconds) * time.Second
}

// TestTheSweepsBackstopOutlastsTheSessionCeiling.
func TestTheSweepsBackstopOutlastsTheSessionCeiling(t *testing.T) {
	maxAge := sweepMaxAgeDefault(t)
	if floor := defaultAppSessionMaxDuration + backstopGrace; maxAge < floor {
		t.Fatalf("the sweep fails every session older than %s, and a delegated session may run for %s; the "+
			"backstop must be at least %s, or the sweep fails sessions still inside their own ceiling",
			maxAge, defaultAppSessionMaxDuration, floor)
	}
	// And the inference door's much shorter ceiling sits under it by
	// construction; asserted so a change to either reads here first.
	if appSessionMaxDuration >= maxAge {
		t.Fatalf("an inference session may run %s, past the sweep's %s backstop", appSessionMaxDuration, maxAge)
	}
}
