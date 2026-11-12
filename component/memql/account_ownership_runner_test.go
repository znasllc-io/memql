package memql

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

const accountStampingActivateEnv = "MEMQL_ACCOUNT_STAMPING_ACTIVATE"

// newTestAccountOwnershipRunner builds one node's runner as the app does, with
// the hold read from the environment as it stands at that moment. The
// assignment job is replaced by the caller where a test counts its batches.
func newTestAccountOwnershipRunner(t *testing.T, e *MemQLEngine, db *bun.DB, nodeID, activate string) *AccountOwnershipRunner {
	t.Helper()
	t.Setenv(accountStampingActivateEnv, activate)
	r, err := NewAccountOwnershipRunner(e, func() *bun.DB { return db }, func() string { return nodeID },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return r
}

// The switch turns on by itself, and only an explicit false holds it. A value
// that is not a boolean refuses the boot, as the schema-reference switch's
// does: read as true it would turn on what the operator may have meant to hold,
// and the switch does not turn off; read as a hold it would leave a cluster
// unstamped with nothing to say why.
func TestAccountStampingActivationIsHeldOnlyByAnExplicitFalse(t *testing.T) {
	provider := func() *bun.DB { return nil }
	nodeID := func() string { return "a" }
	for _, tc := range []struct {
		name    string
		value   string
		unset   bool
		want    bool
		wantErr bool
	}{
		{name: "unset activates", unset: true, want: true},
		{name: "true activates", value: "true", want: true},
		{name: "false holds", value: "false"},
		{name: "invalid refuses startup", value: "hold", wantErr: true},
		{name: "empty refuses startup", value: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(accountStampingActivateEnv, tc.value)
			if tc.unset {
				require.NoError(t, os.Unsetenv(accountStampingActivateEnv))
			}
			r, err := NewAccountOwnershipRunner(&MemQLEngine{}, provider, nodeID, nil)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorContains(t, err, accountStampingActivateEnv, "the refusal does not name the variable to fix")
				require.Nil(t, r, "invalid configuration returned a runnable runner")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, r.allowActivation)
			// Read once, at startup: a later change to the process environment
			// is not a second way to release or set the hold.
			t.Setenv(accountStampingActivateEnv, "false")
			require.Equal(t, tc.want, r.allowActivation)
		})
	}
}

// A runner missing one of its parts would start, tick, and either panic five
// seconds later or sit there doing nothing while the flag stayed off for good.
func TestAccountOwnershipRunnerRefusesToBeBuiltWithoutItsParts(t *testing.T) {
	t.Setenv(accountStampingActivateEnv, "true")
	provider := func() *bun.DB { return nil }
	nodeID := func() string { return "a" }
	_, err := NewAccountOwnershipRunner(nil, provider, nodeID, nil)
	require.Error(t, err)
	_, err = NewAccountOwnershipRunner(&MemQLEngine{}, nil, nodeID, nil)
	require.Error(t, err)
	_, err = NewAccountOwnershipRunner(&MemQLEngine{}, provider, nil, nil)
	require.Error(t, err)
}

// The database handle is resolved on every tick, and a node whose database is
// not connected yet, or is reconnecting, has nothing to ask: it waits for the
// next tick with the flag as it was.
func TestAccountOwnershipRunnerWithoutADatabaseLeavesTheFlagAlone(t *testing.T) {
	e := &MemQLEngine{}
	require.False(t, e.AccountStampingEnabled(), "an engine starts with stamping off")
	r := newTestAccountOwnershipRunner(t, e, nil, "a", "true")
	batches := 0
	r.assign = func(context.Context) error { batches++; return nil }

	require.NoError(t, r.tick(context.Background()))
	require.False(t, e.AccountStampingEnabled())
	require.Zero(t, batches)

	// And it does not clear a flag that is on.
	e.SetAccountStampingEnabled(true)
	require.NoError(t, r.tick(context.Background()))
	require.True(t, e.AccountStampingEnabled())
	require.Zero(t, batches)
}

// The flag is this node's copy of the cluster's switch. It is off until the
// switch is on, each node sets its own at its own tick, the assignment job runs
// only behind it, and nothing clears it: rows written under it carry the field.
func TestAccountOwnershipRunnerTurnsStampingOnOnceEveryNodeAdvertisesAndNeverOff(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")
	f.node("b", "healthy")

	a, b := &MemQLEngine{}, &MemQLEngine{}
	runnerA := newTestAccountOwnershipRunner(t, a, f.db, "a", "true")
	runnerB := newTestAccountOwnershipRunner(t, b, f.other, "b", "true")
	// What the flag said when each of a's batches started.
	var batches []bool
	runnerA.assign = func(context.Context) error {
		batches = append(batches, a.AccountStampingEnabled())
		return nil
	}

	require.NoError(t, runnerA.tick(f.ctx))
	require.False(t, a.AccountStampingEnabled(), "a stamped while b had not said it knows the field")
	require.Empty(t, batches, "the assignment job ran before the switch turned on")

	f.due()
	require.NoError(t, runnerB.tick(f.ctx))
	require.True(t, f.on())
	require.True(t, b.AccountStampingEnabled())
	// Nothing tells a. It reads the switch at its own next tick.
	require.False(t, a.AccountStampingEnabled())
	require.NoError(t, runnerA.tick(f.ctx))
	require.True(t, a.AccountStampingEnabled())
	require.Equal(t, []bool{true}, batches, "the batch must start with the flag already on")

	// A fleet that no longer looks ready.
	_, err := f.db.ExecContext(f.ctx, `DELETE FROM account_stamping_readers`)
	require.NoError(t, err)
	f.node("c", "healthy")
	require.NoError(t, runnerA.tick(f.ctx))
	require.True(t, a.AccountStampingEnabled())
	require.Equal(t, []bool{true, true}, batches, "a tick behind the switch runs one batch")

	// A batch that fails is reported, so the loop backs off, and changes nothing.
	failed := errors.New("batch failed")
	runnerA.assign = func(context.Context) error { return failed }
	require.ErrorIs(t, runnerA.tick(f.ctx), failed)
	require.True(t, a.AccountStampingEnabled())

	// The switch itself going away, as the down migration takes it.
	applyLivenessMigration(t, f.ctx, f.db, "20261112100000_account_ownership.down.sql")
	require.Error(t, runnerA.tick(f.ctx))
	require.True(t, a.AccountStampingEnabled(), "losing the switch table cleared the flag on a node that already stamps")
}

// A held node says it knows the field, so the rest of the fleet is not kept
// waiting on it. It still neither stamps nor assigns, even after another
// replica has turned the switch on.
func TestAccountOwnershipRunnerHeldNodeAdvertisesButNeitherStampsNorAssigns(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("held", "healthy")
	f.node("free", "healthy")

	held, free := &MemQLEngine{}, &MemQLEngine{}
	heldRunner := newTestAccountOwnershipRunner(t, held, f.db, "held", "false")
	freeRunner := newTestAccountOwnershipRunner(t, free, f.other, "free", "true")
	batches := 0
	heldRunner.assign = func(context.Context) error { batches++; return nil }

	require.NoError(t, heldRunner.tick(f.ctx))
	require.True(t, f.advertised("held"), "a held node did not advertise")
	require.False(t, held.AccountStampingEnabled())

	f.due()
	require.NoError(t, freeRunner.tick(f.ctx))
	require.True(t, f.on(), "a held replica kept the rest of the fleet from turning the switch on")
	require.True(t, free.AccountStampingEnabled())

	require.NoError(t, heldRunner.tick(f.ctx))
	require.False(t, held.AccountStampingEnabled(), "a held node started stamping because another replica turned the switch on")
	require.Zero(t, batches, "a held node ran the assignment job")
}

// startTestAccountOwnershipRunner starts the component as the app starts it and
// waits for it as the app waits for every dependency before the node takes
// requests.
func startTestAccountOwnershipRunner(t *testing.T, ctx context.Context, r *AccountOwnershipRunner) {
	t.Helper()
	r.Start(ctx)
	t.Cleanup(func() { r.Stop(context.Background()) })
	select {
	case <-r.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("the runner never reported ready: the app waits on every dependency before the node serves")
	}
}

// The component, started as the app starts it: nothing calls tick but its own
// loop. A runner whose loop was never installed is ready, running and silent,
// and neither its flag nor the assignment job ever moves on that node.
func TestAccountOwnershipRunnerTicksOnItsOwnOnceStarted(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")
	e := &MemQLEngine{}
	r := newTestAccountOwnershipRunner(t, e, f.db, "a", "true")
	require.Equal(t, 5*time.Second, r.interval, "the runner ticks every five seconds")
	r.interval = 20 * time.Millisecond
	var batches atomic.Int32
	r.assign = func(context.Context) error { batches.Add(1); return nil }

	startTestAccountOwnershipRunner(t, f.ctx, r)
	// Batches run on ticks only, so more than one is the loop going round.
	require.Eventually(t, func() bool { return batches.Load() >= 2 }, 10*time.Second, 10*time.Millisecond,
		"a started runner on a one-node fleet never got to the assignment job")
	require.True(t, e.AccountStampingEnabled())

	r.Stop(context.Background())
	require.False(t, r.IsRunning())
}

// Every process starts with the flag off, and the node takes requests as soon
// as its dependencies report ready. So the runner reads the switch once before
// it reports ready: a node restarting into a cluster that already stamps would
// otherwise serve its first seconds of writes unstamped, on every restart, and
// those rows would get their account from the assignment job's rules instead of
// from the call that wrote them.
func TestAccountOwnershipRunnerReadsTheSwitchBeforeItReportsReady(t *testing.T) {
	t.Run("a cluster that already stamps", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		_, err := f.db.ExecContext(f.ctx, `UPDATE account_stamping_storage SET enabled=true`)
		require.NoError(t, err)
		e := &MemQLEngine{}
		// A node id that is not registered yet, as at boot: reading a switch
		// that is on needs neither the id nor the row.
		r := newTestAccountOwnershipRunner(t, e, f.db, "restarted", "true")
		r.interval = time.Hour // nothing here waits for a tick
		var batches atomic.Int32
		r.assign = func(context.Context) error { batches.Add(1); return nil }

		startTestAccountOwnershipRunner(t, f.ctx, r)
		require.True(t, e.AccountStampingEnabled(), "the node reported ready with stamping still off in a cluster that stamps")
		// Maintenance does not stand between a node and serving.
		require.Zero(t, batches.Load(), "a batch of the assignment job ran before the node was ready")
	})

	t.Run("a held node", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		_, err := f.db.ExecContext(f.ctx, `UPDATE account_stamping_storage SET enabled=true`)
		require.NoError(t, err)
		e := &MemQLEngine{}
		r := newTestAccountOwnershipRunner(t, e, f.db, "restarted", "false")
		r.interval = time.Hour

		startTestAccountOwnershipRunner(t, f.ctx, r)
		require.False(t, e.AccountStampingEnabled())
	})

	// A node of this release can start before the migration that creates the
	// switch has run. It must still come up, with the flag off, and ask again.
	t.Run("a switch that cannot be read does not keep the node down", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		applyLivenessMigration(t, f.ctx, f.db, "20261112100000_account_ownership.down.sql")
		e := &MemQLEngine{}
		r := newTestAccountOwnershipRunner(t, e, f.db, "early", "true")
		r.interval = time.Hour

		startTestAccountOwnershipRunner(t, f.ctx, r)
		require.False(t, e.AccountStampingEnabled())
		require.True(t, r.IsRunning())
	})
}
