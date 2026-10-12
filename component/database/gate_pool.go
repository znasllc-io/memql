package database

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// gate_pool.go -- the pool write gates run on (memql#6022, F9 fix round 3).
//
// # What a write gate is
//
// A write gate (component/memql, AcquireWriteGates) is a transaction that holds a
// transaction-scoped advisory lock until its holder is done. The transaction owns
// one connection for as long as the lock is held, and sits idle in it while the
// holder works.
//
// # Why not the request pool
//
// Two things went wrong with gates on the request pool, and neither can be
// configured away there:
//
//   - The holder's own work needs a connection of the pool it holds a gate of.
//     With four connections, four gate holders left none for the work the gates
//     exist to protect, and capping the gates at one fewer than the pool left ONE
//     connection for every holder's reads: they queued behind each other while
//     holding their locks.
//   - The request pool ends a session that sits idle in a transaction after 60 s
//     (idle_in_transaction_session_timeout) -- rightly, for a request. PostgreSQL
//     ends the session and the advisory lock goes with it, silently: the holder
//     goes on writing without the lock it believes it has. A burst of 16 signups
//     was measured to lose its audience locks at 60.2 s.
//
// So a gate takes its connection from a pool of its own. The holder keeps using the
// request pool for its work; the gate pool carries nothing else, so a holder never
// waits for a connection to do its work, and the number of gates open at once is
// the size of this pool (DefaultGatePoolSize per process) and nothing else.
//
// # Its sessions
//
//   - idle_in_transaction_session_timeout is DefaultGateIdleInTxTimeout (5 min):
//     above every phase a gate is held for (the callers bound theirs, 30 s for a
//     signup), so the server never ends a live gate, and still low enough that a
//     session orphaned by a dead pod gives its lock back within minutes.
//   - There is no statement_timeout. The only statement that waits is the lock
//     request, and its wait is bounded per acquisition by a lock_timeout the gate
//     sets from the caller's own deadline; a limit on every statement of the
//     session would only add a second, unrelated way for that one to end.
//   - idle_session_timeout is the request pool's, for the same reason as there.
//
// The pool opens from the main DSN. A gate's lock is transaction-scoped, so it
// holds behind a transaction pooler (the transaction pins its backend), and the
// direct endpoint's budget is for the session-scoped work that cannot.

const (
	envDBGatePoolSize          = "MEMQL_DB_GATE_POOL_SIZE"
	envDBGateIdleInTxTimeoutMs = "MEMQL_DB_GATE_IDLE_IN_TX_TIMEOUT_MS"

	defaultGatePoolSize          = 8
	defaultGateIdleInTxTimeoutMs = 300000

	// gatePoolMaxIdle: connections the pool keeps warm. A gate is taken and given
	// back within seconds, and a cold connect costs a TCP+TLS round trip a signup
	// would pay for.
	gatePoolMaxIdle = 2
)

// The numbers other packages build on, read from here rather than copied: the
// gate wait must stay under the driver's read deadline, a gated phase under the
// gate session's idle limit, and the request pool's own limits are what the gate
// pool exists to be free of.
const (
	// DefaultGatePoolSize is how many write gates one process can hold open at
	// once (MEMQL_DB_GATE_POOL_SIZE).
	DefaultGatePoolSize = defaultGatePoolSize

	// DefaultGateIdleInTxTimeout is how long a gate session may sit idle in its
	// transaction before PostgreSQL ends it (MEMQL_DB_GATE_IDLE_IN_TX_TIMEOUT_MS).
	DefaultGateIdleInTxTimeout = defaultGateIdleInTxTimeoutMs * time.Millisecond

	// DefaultRequestStatementTimeout and DefaultRequestIdleInTxTimeout are the
	// request pool's defaults (MEMQL_DB_STATEMENT_TIMEOUT_MS,
	// MEMQL_DB_IDLE_IN_TX_TIMEOUT_MS).
	DefaultRequestStatementTimeout = defaultStatementTimeoutMs * time.Millisecond
	DefaultRequestIdleInTxTimeout  = defaultIdleInTxTimeoutMs * time.Millisecond
)

// gateConnParams builds the per-connection SET parameters of a gate session: the
// request pool's, with a longer idle-in-transaction limit and no statement
// timeout. See the header for why.
func gateConnParams() map[string]any {
	params := sessionConnParams()
	delete(params, "statement_timeout")
	delete(params, "idle_in_transaction_session_timeout")
	if ms := envIntDefault(envDBGateIdleInTxTimeoutMs, defaultGateIdleInTxTimeoutMs); ms > 0 {
		params["idle_in_transaction_session_timeout"] = strconv.Itoa(ms)
	}
	return params
}

// gatePoolSize is the configured number of connections of the gate pool.
func gatePoolSize() int {
	if n := envIntDefault(envDBGatePoolSize, defaultGatePoolSize); n > 0 {
		return n
	}
	return defaultGatePoolSize
}

// NewGatePool opens a pool for write gates over dsn, configured as the Database
// component configures its own: gateConnParams on every session, bounded at size
// connections (the configured size when size <= 0). The caller closes it.
//
// Exported for the tests of the packages that take gates, which must run them on
// a pool shaped like the deployed one rather than on whatever pool they have.
func NewGatePool(dsn string, size int) (*sql.DB, error) {
	return openGatePool(pgSQLOpener(gateConnParams), "bun:pg", dsn, size)
}

func openGatePool(opener SQLOpener, driverName, dsn string, size int) (*sql.DB, error) {
	if dsn == "" {
		return nil, fmt.Errorf("a gate pool needs a DSN")
	}
	if size <= 0 {
		size = gatePoolSize()
	}
	db, err := opener(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(size)
	db.SetMaxIdleConns(min(gatePoolMaxIdle, size))
	return db, nil
}

// WithGateOpener sets the opener of the gate pool. The default opens a real pool
// over the DSN with gateConnParams; a test supplies its own (and, like the
// migration pool, a custom WithSQLOpener alone leaves no gate pool at all).
func (d *Database) WithGateOpener(opener SQLOpener) DatabaseArg {
	return OptionalCtorArg("gate_opener", func(c *config) {
		if opener != nil {
			c.gateOpener = opener
		}
	})
}

// GateDB returns the pool write gates run on, or nil when it was not opened
// (before the database started, after it stopped, or under a custom SQL opener).
// There is no fallback to the request pool: a gate that cannot have its own
// connection must say so (component/memql: "shared write coordination is
// unavailable"), not take one from the pool its holder works on.
func (d *Database) GateDB() *sql.DB {
	d.Lock()
	defer d.Unlock()
	return d.Gate
}

// openGatePool opens the gate pool beside the request pool. Best-effort on
// error: it logs and leaves GateDB nil, so a gate refuses instead of the node
// failing to start.
func (d *Database) openGatePool() {
	if d.config.gateOpener == nil || d.config.dsn == nil || *d.config.dsn == "" {
		return
	}
	gate, err := openGatePool(d.config.gateOpener, d.config.driverName, *d.config.dsn, 0)
	if err != nil {
		d.Logger.Error("failed to open the write-gate pool; write gates will refuse", "error", err)
		return
	}
	d.Lock()
	previous := d.Gate
	d.Gate = gate
	d.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	d.Logger.Info("write-gate pool opened", "size", gate.Stats().MaxOpenConnections)
}
