// Package pipelinehop proves the pipelines seam's cross-node claim (epic
// memql#5477, issue #5491): a GitHub delivery staged on one node opens a run
// that ANOTHER node claims and drives, and nothing about the run crosses
// between them except its row and its events.
//
// Two engines share one Postgres and nothing else a process could share by
// accident: each has its own pool, its own bus, its own result cache and its
// own pipelines plug-in, built by the plug-in's own factory. A link joins the
// two buses through the routing rules the mesh applies
// (node.ForwardDecisionFor), so a run's created event reaches the agent only
// if component/node/routing.go says it may. The one GitHub both nodes call,
// and the step runner, are fakes; the inbound receiver, the shipped trigger
// automation, the DSL store, the work journal and the advisory gate are the
// real ones.
//
// It lives in the root module, beside test/inboundhop, for that test's
// reason: the hop crosses component/inbound, component/automations and
// component/pipelinerun, and only the root module may depend on all of them.
//
// Postgres-gated: it skips when no database is reachable, and CI's db-tests
// lane runs it with MEMQL_REQUIRE_DB=1, where a skip is a failure.
package pipelinehop

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/znasllc-io/memql/component/database/dbtest"
)

// TestMain applies the schema once before the package's db-gated tests run,
// so the first test does not race another package's migration on the shared
// database (memql#2551). When no database is reachable EnsureSchema is a
// no-op and the test self-skips through dbtest.Unreachable.
func TestMain(m *testing.M) {
	if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "dbtest.EnsureSchema: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
