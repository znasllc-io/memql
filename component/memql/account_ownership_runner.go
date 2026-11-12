package memql

// account_ownership_runner.go -- this node's half of account stamping's
// rollout: the flag the write path reads, and the background component that
// sets it and then runs the assignment of existing rows.
//
// # A flag in the process, a switch in the database
//
// Whether a node may record accountId on a row is a fact about the whole
// fleet: it may once no node of the previous release is left to refuse the row
// (memorynodes.EnableAccountStampingWhenReady has the reason). So the decision
// is one row in the database, turned on once for the cluster. The write path
// cannot ask the database before every write, so each node keeps a copy in an
// atomic flag, and the runner here is the only thing that sets it: every five
// seconds it runs the handshake, and once the switch is on it sets the flag and
// runs one batch of the assignment job, which also carries the rows that follow
// a parent whose account changed (account_follow.go).
//
// # Which node holds the flag, and which node needs it
//
// Every node holds its own and runs its own runner, on every node type,
// because the stamp is in the write path every node type runs. Nothing is
// forwarded between them. A replica learns that the switch turned on by reading
// the table at its own next tick, up to five seconds after the replica that
// turned it on. In that window one replica stamps and another does not, and the
// row the second one writes has no account yet, like a row written before the
// switch; the assignment job is what finds such a row. What the window cannot
// contain is a node that refuses a stamped row, because the switch turns on
// only after every live node has said it knows the field.
//
// # One way
//
// The runner sets the flag and never clears it. Rows written under it carry the
// field, and no later event takes the field off them: not a handshake that
// fails, not a database that goes away, not the switch's own tables being
// dropped. A process that restarts starts with the flag off and reads the
// switch again before it reports ready, so a node joining a cluster that
// already stamps does not serve its first requests unstamped (run).
//
// # The hold
//
// MEMQL_ACCOUNT_STAMPING_ACTIVATE=false, read once at startup. A held node
// still runs the handshake, which advertises it, so the rest of the fleet is
// not kept waiting and releasing the hold on one replica needs no rollout of
// the others. Its own flag stays off even after another replica has turned the
// switch on, and it runs no assignment. A hold keeps the way back to the
// previous release open only while it is set on every replica: one replica
// without it turns the switch on and stamps rows the previous release cannot
// update.
//
// Tests: account_ownership_runner_test.go, and account_stamping_db_test.go for
// the handshake against a real fleet table.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/component"
	"github.com/znasllc-io/memql/core/env"
)

const (
	// accountOwnershipInterval is the handshake's own: the fleet is looked at
	// once per five seconds for the whole cluster, so a node that asked more
	// often would learn nothing sooner.
	accountOwnershipInterval = 5 * time.Second
	// accountOwnershipDeadline bounds one tick, the handshake and the batch
	// together, and the first reading of the switch, which is the only thing
	// here a node waits for before it serves (run). A tick that cannot finish
	// in the time before the next one is due is better abandoned than stacked.
	accountOwnershipDeadline = 5 * time.Second
	// accountOwnershipBackoff is how long a node waits after a tick that
	// failed. The usual failure is a rollout's own: only the identity node or
	// the migrate job runs migrations, so a node of this release can be serving
	// before the migration that creates the switch has run. Asking again every
	// five seconds would write the same warning twelve times a minute.
	accountOwnershipBackoff = time.Minute
)

// AccountStampingEnabled reports whether this node records the account of the
// rows it writes. It is false until this node's AccountOwnershipRunner has read
// the cluster's switch as on, and the switch does not turn on while a node that
// does not know the accountId field is still live. Every caller that would
// write the field asks first: with it off, a write is exactly what the previous
// release wrote.
func (e *MemQLEngine) AccountStampingEnabled() bool {
	return e.accountStamping.Load()
}

// SetAccountStampingEnabled sets this node's copy of the switch. The runner
// calls it with true and never with false; a test calls it to put an engine on
// either side of the switch without a fleet. It decides nothing about the
// cluster: the switch itself is a row only the handshake turns on.
func (e *MemQLEngine) SetAccountStampingEnabled(on bool) {
	e.accountStamping.Store(on)
}

// AccountOwnershipRunner mirrors the cluster's account-stamping switch into the
// engine's flag and, behind it, runs the assignment of existing rows one batch
// per tick. One per node; see the file header for what it may and may not do.
//
// It keeps the default component order, which is the engine's, and the app
// registers it after the engine (app/engine.go). So it is started after the
// engine and stopped before it, while the database it reads is still up.
type AccountOwnershipRunner struct {
	*component.Component
	engine   *MemQLEngine
	provider func() *bun.DB
	nodeID   func() string
	logger   *slog.Logger
	// allowActivation is false on a held node (MEMQL_ACCOUNT_STAMPING_ACTIVATE).
	// Captured at construction: the environment of a running process is not a
	// second way to set or release the hold.
	allowActivation bool
	// interval and assign are accountOwnershipInterval and the engine's
	// runAccountAssignmentBatch. Seams, not configuration: nothing sets them
	// outside a test, where a loop has to tick in milliseconds and a batch has
	// to be counted.
	interval time.Duration
	assign   func(context.Context) error
}

// NewAccountOwnershipRunner builds the runner for one node. provider and nodeID
// are resolved on every tick, because both arrive after the runner is built:
// the database connects and reconnects, and a node learns its id in the cluster
// phase. A node with no id yet does not advertise, and one with no database
// skips the tick.
//
// It refuses a MEMQL_ACCOUNT_STAMPING_ACTIVATE that is not a boolean, which
// refuses the boot. Unset means true: the switch turns on by itself, and only
// an explicit false holds it.
func NewAccountOwnershipRunner(e *MemQLEngine, provider func() *bun.DB, nodeID func() string, logger *slog.Logger) (*AccountOwnershipRunner, error) {
	if e == nil || provider == nil || nodeID == nil {
		return nil, errors.New("account ownership runner: the engine, the database provider and the node id are all required")
	}
	activate, err := env.NewEnvReader("MEMQL_ACCOUNT_STAMPING").OptionalBool("ACTIVATE")
	if err != nil {
		return nil, err
	}
	comp, err := component.New(common.ComponentName("accountOwnershipRunner"))
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &AccountOwnershipRunner{
		Component:       comp,
		engine:          e,
		provider:        provider,
		nodeID:          nodeID,
		logger:          logger,
		allowActivation: activate == nil || *activate,
		interval:        accountOwnershipInterval,
		assign:          e.runAccountAssignmentBatch,
	}
	if err := r.ConfigureLifecycle(component.WithRunHook(r.run)); err != nil {
		return nil, err
	}
	if !r.allowActivation {
		logger.Info("account stamping is held on this node by MEMQL_ACCOUNT_STAMPING_ACTIVATE=false: " +
			"it advertises itself, and neither stamps nor assigns")
	}
	return r, nil
}

// run reads the switch once BEFORE it reports the runner started, then ticks.
//
// Every process starts with the flag off, and the app holds a node back from
// requests until each dependency has reported started (waitForDependenciesReady
// in app/run.go). Reading first means a node that restarts into a cluster that
// already stamps has its flag on by the time it serves. Reading on the first
// tick instead would leave every restarted node writing unstamped rows for its
// first five seconds, on every restart, and the job that later assigns those
// rows does not know the call that wrote them.
//
// The first reading is the switch only, never a batch: maintenance does not
// stand between a node and serving. It is bounded like any tick, and a reading
// that fails does not keep the node down. The node comes up with the flag off
// and asks again, which is what a node started before the migration that
// creates the switch has to do.
func (r *AccountOwnershipRunner) run(ctx context.Context, markStarted func()) error {
	retryAfter := r.pass(ctx, time.Now(), func(ctx context.Context) error {
		_, err := r.readSwitch(ctx)
		return err
	})
	markStarted()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if now.Before(retryAfter) {
				continue
			}
			retryAfter = r.pass(ctx, now, r.tick)
		}
	}
}

// pass runs one step under the tick's deadline and answers when the next one
// may run: at once after a step that worked, after the backoff after one that
// failed. A step cut short because the runner is stopping is not a failure, and
// nothing runs after it.
func (r *AccountOwnershipRunner) pass(ctx context.Context, now time.Time, step func(context.Context) error) time.Time {
	stepCtx, cancel := context.WithTimeout(ctx, accountOwnershipDeadline)
	defer cancel()
	if err := step(stepCtx); err != nil && ctx.Err() == nil {
		r.logger.Warn("account ownership pass failed", "error", err, "retryIn", accountOwnershipBackoff.String())
		return now.Add(accountOwnershipBackoff)
	}
	return time.Time{}
}

// tick is one pass: the handshake, the flag, then one batch. The batch also
// carries followers of rows whose account changed (account_follow.go): that is
// two rules of the job, not a second runner.
//
// The flag is set before the batch runs, never after: a node that assigned
// existing rows while its own writes still went out unstamped would be making
// the rows of its next pass.
func (r *AccountOwnershipRunner) tick(ctx context.Context) error {
	acts, err := r.readSwitch(ctx)
	if err != nil || !acts {
		return err
	}
	return r.assign(ctx)
}

// readSwitch runs the handshake and sets this node's flag once the switch is
// on. It reports whether the node acts on the switch: it is on, and the node is
// not held.
//
// The handshake runs on a held node too, because advertising is the part of it
// a held node still owes the fleet. What a held node does not do is act on the
// answer: the switch being on for the cluster is not this node's permission to
// stamp.
func (r *AccountOwnershipRunner) readSwitch(ctx context.Context) (bool, error) {
	db := r.provider()
	if db == nil {
		return false, nil
	}
	on, err := memorynodes.EnableAccountStampingWhenReady(ctx, db, r.nodeID(), r.allowActivation)
	if err != nil || !on || !r.allowActivation {
		return false, err
	}
	if !r.engine.AccountStampingEnabled() {
		r.engine.SetAccountStampingEnabled(true)
		r.logger.Info("account stamping is on: this node now records the account of the rows it writes", "nodeId", r.nodeID())
	}
	return true, nil
}
