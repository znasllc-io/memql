package automations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/events"
)

// TestClusterGuard_FailOpenWithoutDB: with no reachable DB the guard executes
// UNGUARDED (returns true so legitimate work is never dropped) but records the
// claim error so the double-fire window is observable.
func TestClusterGuard_FailOpenWithoutDB(t *testing.T) {
	g := NewClusterExecutionGuard(func() *bun.DB { return nil }, nil)

	ok := g.Claim(context.Background(), "someAutomation", "head-abc")
	assert.True(t, ok, "fail-open: execute rather than drop work when the DB is unreachable")
	assert.Equal(t, int64(1), g.ClaimErrors(), "the unguarded window must be counted")
	assert.Equal(t, int64(0), g.DuplicatesPrevented())
	assert.Equal(t, int64(0), g.Claimed())
	assert.Equal(t, ClusterGuardComponentName, g.ComponentName())
}

// The guard satisfies the executor's claim interface.
func TestClusterGuard_ImplementsClaimer(t *testing.T) {
	var _ executionClaimer = NewClusterExecutionGuard(func() *bun.DB { return nil }, nil)
}

// fakeClaimer is a deterministic executionClaimer for executor tests: it
// remembers keys and returns false the second time a key is seen (mirroring a
// cross-replica duplicate being collapsed).
type fakeClaimer struct {
	seen  map[string]bool
	calls int
}

func (f *fakeClaimer) Claim(_ context.Context, automation, key string) bool {
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.calls++
	k := automation + "\x00" + key
	if f.seen[k] {
		return false // already claimed by a "different replica"
	}
	f.seen[k] = true
	return true
}

// recordingClaimer captures what the executor passes and returns a fixed
// verdict, so we can assert the executor consults the guard with the right
// (automation, dedup-key) and honours its decision.
type recordingClaimer struct {
	result        bool
	calls         int
	gotAutomation string
	gotKey        string
}

func (r *recordingClaimer) Claim(_ context.Context, automation, key string) bool {
	r.calls++
	r.gotAutomation = automation
	r.gotKey = key
	return r.result
}

// TestExecutor_ClusterGuardSkipsCrossReplicaDuplicate is the #561 integration
// at the executor layer: when the cluster guard reports the (automation,
// event) is already claimed by another replica, the event executor SKIPS --
// it does not run the automation a second time.
func TestExecutor_ClusterGuardSkipsCrossReplicaDuplicate(t *testing.T) {
	guard := &recordingClaimer{result: false} // "another replica already owns it"
	e := NewExecutor(ExecutorOptions{
		ChainTrackingEnabled:    true,
		ClusterGuard:            guard,
		MaxConcurrentExecutions: 1,
	})

	automation := &Automation{Name: "ensureDailySpaceOnAuthSession"}
	event := &events.Event{
		Topic:   "graph.node.created._system.v1:identity:authSession",
		Payload: map[string]any{"id": "v1:identity:authSession:abc"},
	}

	exec, err := e.ExecuteWithEvent(context.Background(), automation, "event:authSession.created", event)
	require.NoError(t, err)
	assert.Equal(t, "skipped", exec.Status)
	assert.Contains(t, exec.Error, "cluster guard")

	assert.Equal(t, 1, guard.calls, "the guard must be consulted exactly once")
	assert.Equal(t, "ensureDailySpaceOnAuthSession", guard.gotAutomation)
	assert.NotEmpty(t, guard.gotKey, "the InitialChainHead must be passed as the dedup key")
}
