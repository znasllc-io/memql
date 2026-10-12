package work

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// human_feedback_chain_db_test.go -- a renewal chain takes its locks with one
// connection of the gate pool (memql#6022, F9 fix round 4).
//
// A computer-access question whose grant expired is renewed under a successor
// receipt, and every receipt of the chain is a decision lock: an answer to any of
// them must not land while the chain is walked. Taken one inside another, the
// locks were a gate-pool connection each, so a chain longer than the pool could
// never be renewed -- every renewal waited six seconds for a connection its own
// earlier locks held, and answered busy.

// chainGatePool is a real gate pool of `size` connections, or a skip when there
// is no database.
func chainGatePool(t *testing.T, size int) *sql.DB {
	t.Helper()
	pool, err := database.NewGatePool(dbtest.DSN(), size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.PingContext(ping); err != nil {
		dbtest.Unreachable(t, "the decision gates of a renewal chain", dbtest.DSN(), err)
		return nil
	}
	return pool
}

// renewalChainIDs is the chain of receipt ids a scope question renews through,
// first to last, as parkHumanDecision derives them.
func renewalChainIDs(run, step string, subject map[string]any, n int) []string {
	first := "question-" + workstate.ArtifactHash(map[string]any{"run": run, "step": step, "question": workstate.ArtifactHash(subject), "kind": "scopeElevation"})[:32]
	chain := []string{first}
	for len(chain) < n {
		chain = append(chain, "question-"+workstate.ArtifactHash(map[string]any{"renewalOf": chain[len(chain)-1]})[:32])
	}
	return chain
}

// TestARenewalChainLongerThanTheGatePoolIsRenewed: five expired grants in a row,
// so the question renews to a sixth receipt; the gate pool has two connections.
// The renewal completes, at once, and creates the sixth receipt.
func TestARenewalChainLongerThanTheGatePoolIsRenewed(t *testing.T) {
	const poolSize, expired = 2, 5
	pool := chainGatePool(t, poolSize)
	if pool == nil {
		return
	}
	eng := newRecordingEngine()
	i := New(eng, testLogger())
	i.SetNow(func() time.Time { return testNow })
	i.SetGateDB(func() *sql.DB { return pool })

	subject := map[string]any{"scope": "observe", "summary": "Read the requested file", "agentId": "agent"}
	chain := renewalChainIDs("r1", "reason", subject, expired+1)
	eng.reply("workRunForOwner", map[string]any{"id": "r1", "ownerUserId": "u-alice", "status": "running"})
	for _, id := range chain[:expired] {
		eng.replyWhen("workApprovalForOwner", id, map[string]any{"id": id, "decision": "approved", "expiresAt": testNow.Add(-time.Second).Format(time.RFC3339Nano)})
	}
	ctx, cancel := context.WithTimeout(common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "r1", GoalId: "g1", OwnerUserId: "u-alice", StepKey: "reason"}), 20*time.Second)
	defer cancel()

	started := time.Now()
	got, err := i.AskComputerScope(ctx, "u-alice", "r1", subject)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("a renewal chain of %d receipts on a gate pool of %d connections answered %v after %s", len(chain), poolSize, err, elapsed.Round(time.Millisecond))
	}
	if got != chain[len(chain)-1] {
		t.Errorf("the renewal answered %s, want the chain's last receipt %s", got, chain[len(chain)-1])
	}
	creates := eng.callsTo("createWorkApproval")
	if len(creates) != 1 || !strings.Contains(creates[0].Query, chain[len(chain)-1]) {
		t.Errorf("the renewal created %d receipts, want exactly the chain's last: %v", len(creates), creates)
	}
	if elapsed > 3*time.Second {
		t.Errorf("the renewal took %s: it waited for gate connections", elapsed.Round(time.Millisecond))
	}
}
