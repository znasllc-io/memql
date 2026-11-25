package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/env"
	"github.com/znasllc-io/memql/core/logger"
)

type (
	DatabaseArg common.CtorArg[config]
	Migrator    interface {
		Migrate(ctx context.Context, db *bun.DB) error
	}

	Database struct {
		sync.Mutex
		componentName common.ComponentName
		Logger        *slog.Logger
		DB            *sql.DB
		Bun           *bun.DB
		// DirectDB / DirectBun are the optional second, NON-pooled endpoint
		// (DIRECT_DSN). Session-stateful work -- session-scoped advisory
		// locks, leader election, migrations -- rides this so it bypasses the
		// transaction pooler that recycles a server backend out from under a
		// held session (epic memql#1925). nil when DIRECT_DSN is unset, in
		// which case DirectBunDB() falls back to the main (pooled) Bun -- so
		// local/dev without a pooler is unaffected (env-agnostic).
		DirectDB  *sql.DB
		DirectBun *bun.DB
		// Gate is the pool write gates run on (gate_pool.go): connections used
		// for nothing else, so a gate holder never waits for one to do its work
		// and a session sitting idle in a gate's transaction is not held to the
		// request pool's limits. nil when it was not opened; there is no fallback
		// to the request pool.
		Gate         *sql.DB
		ticker       *time.Ticker
		firstPing    bool
		isRunning    bool
		config       *config
		heldLock     migrationLockWatch
		lifecycle    common.Lifecycle
		readyCh      chan struct{}
		migrationErr error

		// migrationsRan records that a migration attempt has COMPLETED without
		// error at least once. Distinct from migrationErr == nil, which is also
		// true before anything has been attempted -- and the difference is the
		// whole point (memql#3879): a node that started before the database was
		// reachable has no recorded error to retry on once its very first
		// attempt died at connect time, so "no error" must not be read as
		// "migrated".
		migrationsRan bool

		// migrationReachedDB records that the most recent attempt reached the
		// database: its session answered, or a custom migrator returned
		// something other than a lost connection. The run loop's first ping
		// reads it (startupPing).
		migrationReachedDB bool

		// migrationBackends are the backends this process's migration sessions
		// ran on, newest last. An attempt that finds the lock held by one of them
		// is looking at its own node's earlier statement and waits for it
		// (migration_session.go). Per process on purpose: a restarted process
		// owns no statement the last one left running.
		migrationBackends []migrationBackend
	}

	DatabaseEnvOptions struct {
		DSN                      string
		DirectDSN                string
		MigrateOnStart           *bool
		MaxConnRetries           *int
		MaxConnRetriesIntervalMs *int
		TickerIntervalMs         *int
		MigrationTimeoutMs       *int
		SQLDriver                string
		LoggerLevel              string
		// Connection pool configuration
		MaxOpenConns      *int
		MaxIdleConns      *int
		ConnMaxLifetimeMs *int
		ConnMaxIdleTimeMs *int
	}

	DatabaseEnvKeys struct {
		DSN                      string
		DirectDSN                string
		MigrateOnStart           string
		MaxConnRetries           string
		MaxConnRetriesIntervalMs string
		TickerIntervalMs         string
		MigrationTimeoutMs       string
		SQLDriver                string
		LoggerLevel              string
		// Connection pool configuration
		MaxOpenConns      string
		MaxIdleConns      string
		ConnMaxLifetimeMs string
		ConnMaxIdleTimeMs string
	}

	DatabaseEnvLoader struct {
		Prefix string
		Keys   DatabaseEnvKeys
	}

	config struct {
		dsn                      *string
		directDSN                *string
		driverName               string
		writer                   io.Writer
		level                    slog.Level
		tickerIntervalMs         int
		maxConnRetries           int
		maxConnRetriesIntervalMs int
		sqlOpener                SQLOpener
		migrationConnector       ConnectorOpener // nil: no migration session, so no migration runs (migration_session.go)
		gateOpener               SQLOpener       // nil: no gate pool (gate_pool.go)
		maintenanceOpener        SQLOpener       // nil: no maintenance session (maintenance_session.go)
		bunFactory               BunFactory
		bunOptions               []BunOption
		migrator                 Migrator
		migrations               *migrate.Migrations
		migrateOnStart           bool
		migrationTimeoutMs       int
		initHooks                []InitHook
		postMigrationHooks       []PostMigrationHook
		// Connection pool configuration
		maxOpenConns      int
		maxIdleConns      int
		connMaxLifetimeMs int
		connMaxIdleTimeMs int
	}

	InitHook          func(context.Context, *sql.DB) error
	PostMigrationHook func(context.Context, *bun.DB, *slog.Logger) error
	SQLOpener         func(driverName, dsn string) (*sql.DB, error)
	BunFactory        func(*sql.DB) (*bun.DB, error)
	BunOption         func(*bun.DB)
)

const (
	databaseLoggerLevelKey         = "CAPABILITIES_LOGGING_LOG_LEVEL"
	databaseLegacyLoggerLevelKey   = "LOGGER_LEVEL"
	databaseFallbackLoggerLevelKey = "LOG_LEVEL"
)

var (
	defaultDatabaseEnvKeys = DatabaseEnvKeys{
		DSN:                      "DSN",
		DirectDSN:                "DIRECT_DSN",
		MigrateOnStart:           "MIGRATE_ON_START",
		MaxConnRetries:           "MAX_CONN_RETRIES",
		MaxConnRetriesIntervalMs: "MAX_CONN_RETRIES_INTERVAL_MS",
		TickerIntervalMs:         "TICKER_INTERVAL_MS",
		MigrationTimeoutMs:       "MIGRATION_TIMEOUT_MS",
		SQLDriver:                "SQL_DRIVER",
		LoggerLevel:              databaseLoggerLevelKey,
		// Connection pool configuration
		MaxOpenConns:      "MAX_OPEN_CONNS",
		MaxIdleConns:      "MAX_IDLE_CONNS",
		ConnMaxLifetimeMs: "CONN_MAX_LIFETIME_MS",
		ConnMaxIdleTimeMs: "CONN_MAX_IDLE_TIME_MS",
	}
)

func OptionalCtorArg(name string, fn func(*config)) DatabaseArg {
	return newCtorArg(name, true, fn)
}

func RequiredCtorArg(name string, fn func(*config)) DatabaseArg {
	return newCtorArg(name, false, fn)
}

func WithDSN(dsn string) DatabaseArg {
	trimmed := strings.TrimSpace(dsn)
	return RequiredCtorArg("dsn", func(c *config) {
		value := trimmed
		c.dsn = &value
	})
}

// WithDirectDSN configures the optional second, NON-pooled DB endpoint
// (DIRECT_DSN). Session-stateful work (advisory locks, leader election,
// migrations) resolves its *bun.DB via DirectBunDB() so it bypasses the
// transaction pooler the main pool rides (epic memql#1925). An empty/blank
// value is a no-op: DirectBunDB() then falls back to the main pool, keeping
// behavior identical to today for local/dev without a pooler.
func WithDirectDSN(dsn string) DatabaseArg {
	trimmed := strings.TrimSpace(dsn)
	return OptionalCtorArg("direct_dsn", func(c *config) {
		if trimmed == "" {
			return
		}
		value := trimmed
		c.directDSN = &value
	})
}

func newCtorArg(name string, optional bool, fn func(*config)) DatabaseArg {
	return common.NewCtorArg(name, optional, fn)
}

func ensureContext(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	return context.Background()
}

func normalizeDSN(raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", fmt.Errorf("sql opener: empty dsn")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", "", fmt.Errorf("sql opener: parse dsn: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "postgres", "postgresql", "unix":
	default:
		return "", "", fmt.Errorf("sql opener: unsupported dsn scheme %q", u.Scheme)
	}

	if addNeonEndpoint(u) {
		u.RawQuery = u.Query().Encode()
	}

	return u.String(), u.Hostname(), nil
}

func addNeonEndpoint(u *url.URL) bool {
	if u == nil {
		return false
	}

	host := strings.ToLower(u.Hostname())

	if !strings.HasSuffix(host, ".neon.tech") {
		return false
	}

	parts := strings.Split(host, ".")

	if len(parts) == 0 {
		return false
	}

	endpoint := trimNeonEndpoint(parts[0])

	if endpoint == "" {
		return false
	}

	values := u.Query()
	opt := values.Get("options")

	if strings.Contains(opt, "endpoint=") {
		return false
	}

	if opt == "" {
		opt = "endpoint=" + endpoint
	} else {
		opt = opt + " endpoint=" + endpoint
	}

	values.Set("options", opt)

	u.RawQuery = values.Encode()

	return true
}

func trimNeonEndpoint(label string) string {
	trimmed := label
	suffixes := []string{"-pooler", "-rw", "-ro"}
	for _, suffix := range suffixes {
		trimmed = strings.TrimSuffix(trimmed, suffix)
	}
	return trimmed
}

func defaultConfig() *config {
	return &config{
		dsn:                      nil,
		writer:                   os.Stdout,
		level:                    slog.LevelInfo,
		tickerIntervalMs:         30000,
		maxConnRetries:           3,
		maxConnRetriesIntervalMs: 1000,
		driverName:               "bun:pg",
		sqlOpener:                pgSQLOpener(sessionConnParams),
		// No read deadline of its own: every statement the session sends
		// carries a context deadline (migration_session.go).
		migrationConnector: pgConnectorWith(migrationConnParams, pgdriver.WithReadTimeout(0)),
		gateOpener:         pgSQLOpener(gateConnParams),
		maintenanceOpener:  maintenanceSQLOpener(),
		bunFactory: func(db *sql.DB) (*bun.DB, error) {
			if db == nil {
				return nil, fmt.Errorf("bun factory: nil sql.DB")
			}
			return bun.NewDB(db, pgdialect.New()), nil
		},
		bunOptions:         nil,
		migrator:           nil,
		migrations:         migrate.NewMigrations(),
		migrateOnStart:     true,
		migrationTimeoutMs: 30000,
		initHooks:          nil,
		postMigrationHooks: nil,
	}
}

func DefaultDatabaseEnvKeys() DatabaseEnvKeys {
	return defaultDatabaseEnvKeys
}

func LoadDatabaseEnvOptions(loader DatabaseEnvLoader) (DatabaseEnvOptions, error) {
	keys := loader.Keys

	if keys == (DatabaseEnvKeys{}) {
		keys = defaultDatabaseEnvKeys
	}

	reader := env.NewEnvReader(loader.Prefix)

	var opts DatabaseEnvOptions

	if value, ok := reader.String(keys.DSN); ok {
		opts.DSN = value
	}

	if value, ok := reader.String(keys.DirectDSN); ok {
		opts.DirectDSN = value
	}

	migrateOnStart, err := reader.OptionalBool(keys.MigrateOnStart)

	if err != nil {
		return DatabaseEnvOptions{}, err
	}

	opts.MigrateOnStart = migrateOnStart

	maxConnRetries, err := reader.OptionalInt(keys.MaxConnRetries)

	if err != nil {
		return DatabaseEnvOptions{}, err
	}

	opts.MaxConnRetries = maxConnRetries

	maxConnRetriesInterval, err := reader.OptionalInt(keys.MaxConnRetriesIntervalMs)

	if err != nil {
		return DatabaseEnvOptions{}, err
	}

	opts.MaxConnRetriesIntervalMs = maxConnRetriesInterval

	tickerInterval, err := reader.OptionalInt(keys.TickerIntervalMs)

	if err != nil {
		return DatabaseEnvOptions{}, err
	}

	opts.TickerIntervalMs = tickerInterval

	migrationTimeout, err := reader.OptionalInt(keys.MigrationTimeoutMs)

	if err != nil {
		return DatabaseEnvOptions{}, err
	}

	opts.MigrationTimeoutMs = migrationTimeout

	if value, ok := reader.String(keys.SQLDriver); ok {
		opts.SQLDriver = value
	}

	if value, ok := readLoggerLevel(reader, keys.LoggerLevel); ok {
		opts.LoggerLevel = value
	}

	// Connection pool configuration
	maxOpenConns, err := reader.OptionalInt(keys.MaxOpenConns)
	if err != nil {
		return DatabaseEnvOptions{}, err
	}
	opts.MaxOpenConns = maxOpenConns

	maxIdleConns, err := reader.OptionalInt(keys.MaxIdleConns)
	if err != nil {
		return DatabaseEnvOptions{}, err
	}
	opts.MaxIdleConns = maxIdleConns

	connMaxLifetimeMs, err := reader.OptionalInt(keys.ConnMaxLifetimeMs)
	if err != nil {
		return DatabaseEnvOptions{}, err
	}
	opts.ConnMaxLifetimeMs = connMaxLifetimeMs

	connMaxIdleTimeMs, err := reader.OptionalInt(keys.ConnMaxIdleTimeMs)
	if err != nil {
		return DatabaseEnvOptions{}, err
	}
	opts.ConnMaxIdleTimeMs = connMaxIdleTimeMs

	return opts, nil
}

func readLoggerLevel(reader env.EnvReader, key string) (string, bool) {
	candidates := []string{
		key,
		defaultDatabaseEnvKeys.LoggerLevel,
		databaseLegacyLoggerLevelKey,
		databaseFallbackLoggerLevelKey,
	}

	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}

		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}

		if value, ok := reader.String(trimmed); ok {
			if trimmedValue := strings.TrimSpace(value); trimmedValue != "" {
				return trimmedValue, true
			}
		}
	}

	return "", false
}

func EnvOptionsToArgs(opts DatabaseEnvOptions) ([]DatabaseArg, error) {
	trimmedDSN := strings.TrimSpace(opts.DSN)
	if trimmedDSN == "" {
		return nil, fmt.Errorf("dsn is required")
	}

	args := []DatabaseArg{WithDSN(trimmedDSN)}
	var base Database

	if trimmedDirectDSN := strings.TrimSpace(opts.DirectDSN); trimmedDirectDSN != "" {
		args = append(args, WithDirectDSN(trimmedDirectDSN))
	}

	if levelName := strings.TrimSpace(opts.LoggerLevel); levelName != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToLower(levelName))); err != nil {
			return nil, fmt.Errorf("parse logger level: %w", err)
		}
		args = append(args, base.WithLoggerLevel(level))
	}

	if opts.MigrateOnStart != nil {
		args = append(args, base.WithMigrateOnStart(*opts.MigrateOnStart))
	}

	if opts.MaxConnRetries != nil {
		args = append(args, base.WithMaxConnRetries(*opts.MaxConnRetries))
	}

	if opts.MaxConnRetriesIntervalMs != nil {
		args = append(args, base.WithMaxConnRetriesInterval(*opts.MaxConnRetriesIntervalMs))
	}

	if opts.TickerIntervalMs != nil {
		args = append(args, base.WithTickerInterval(*opts.TickerIntervalMs))
	}

	if opts.MigrationTimeoutMs != nil {
		args = append(args, base.WithMigrationTimeoutMs(*opts.MigrationTimeoutMs))
	}

	if driver := strings.TrimSpace(opts.SQLDriver); driver != "" {
		args = append(args, base.WithSQLDriver(driver))
	}

	// Connection pool configuration
	if opts.MaxOpenConns != nil {
		args = append(args, base.WithMaxOpenConns(*opts.MaxOpenConns))
	}

	if opts.MaxIdleConns != nil {
		args = append(args, base.WithMaxIdleConns(*opts.MaxIdleConns))
	}

	if opts.ConnMaxLifetimeMs != nil {
		args = append(args, base.WithConnMaxLifetimeMs(*opts.ConnMaxLifetimeMs))
	}

	if opts.ConnMaxIdleTimeMs != nil {
		args = append(args, base.WithConnMaxIdleTimeMs(*opts.ConnMaxIdleTimeMs))
	}

	return args, nil
}

func NewDatabase(componentName common.ComponentName, args ...DatabaseArg) (*Database, error) {
	cfg := defaultConfig()

	for _, arg := range args {
		if arg == nil {
			continue
		}
		arg.Apply(cfg)
	}

	if cfg.dsn == nil {
		return nil, fmt.Errorf("missing required constructor argument 'dsn'")
	}

	d := &Database{
		componentName: componentName,
		Logger:        logger.New(componentName, cfg.writer, cfg.level),
		ticker:        time.NewTicker(time.Duration(cfg.tickerIntervalMs) * time.Millisecond),
		firstPing:     true,
		isRunning:     false,
		config:        cfg,
		lifecycle:     common.Lifecycle{},
		readyCh:       make(chan struct{}),
	}

	d.Logger.Info("instance created")

	return d, nil
}

func (d *Database) WithLoggerWriter(w io.Writer) DatabaseArg {
	return OptionalCtorArg("logger_writer", func(c *config) {
		if w != nil {
			c.writer = w
		}
	})
}

func (d *Database) WithLoggerLevel(level slog.Level) DatabaseArg {
	return OptionalCtorArg("logger_level", func(c *config) {
		c.level = level
	})
}

func (d *Database) WithTickerInterval(intervalMs int) DatabaseArg {
	return OptionalCtorArg("ticker_interval_ms", func(c *config) {
		if intervalMs > 0 {
			c.tickerIntervalMs = intervalMs
		}
	})
}

func (d *Database) WithMaxConnRetries(retries int) DatabaseArg {
	return OptionalCtorArg("max_conn_retries", func(c *config) {
		if retries > 0 {
			c.maxConnRetries = retries
		}
	})
}

func (d *Database) WithMaxConnRetriesInterval(intervalMs int) DatabaseArg {
	return OptionalCtorArg("max_conn_retries_interval_ms", func(c *config) {
		if intervalMs > 0 {
			c.maxConnRetriesIntervalMs = intervalMs
		}
	})
}

func (d *Database) WithMigrationTimeoutMs(timeoutMs int) DatabaseArg {
	return OptionalCtorArg("migration_timeout_ms", func(c *config) {
		if timeoutMs > 0 {
			c.migrationTimeoutMs = timeoutMs
		}
	})
}

func (d *Database) WithSQLDriver(driverName string) DatabaseArg {
	return OptionalCtorArg("sql_driver", func(c *config) {
		if driverName != "" {
			c.driverName = driverName
		}
	})
}

func (d *Database) WithSQLOpener(opener SQLOpener) DatabaseArg {
	return OptionalCtorArg("sql_opener", func(c *config) {
		if opener != nil {
			c.sqlOpener = opener
			c.migrationConnector = nil
			c.gateOpener = nil
			c.maintenanceOpener = nil
		}
	})
}

func (d *Database) WithBunFactory(factory BunFactory) DatabaseArg {
	return OptionalCtorArg("bun_factory", func(c *config) {
		if factory != nil {
			c.bunFactory = factory
		}
	})
}

func (d *Database) WithBunOptions(options ...BunOption) DatabaseArg {
	copied := append([]BunOption(nil), options...)
	return OptionalCtorArg("bun_options", func(c *config) {
		c.bunOptions = append(c.bunOptions, copied...)
	})
}

func (d *Database) WithMigrator(m Migrator) DatabaseArg {
	return OptionalCtorArg("migrator", func(c *config) {
		c.migrator = m
	})
}

func (d *Database) WithMigrateOnStart(enabled bool) DatabaseArg {
	return OptionalCtorArg("migrate_on_start", func(c *config) {
		c.migrateOnStart = enabled
	})
}

func (d *Database) WithInitHook(hook InitHook) DatabaseArg {
	return OptionalCtorArg("init_hook", func(c *config) {
		if hook != nil {
			c.initHooks = append(c.initHooks, hook)
		}
	})
}

func (d *Database) WithPostMigrationHook(hook PostMigrationHook) DatabaseArg {
	return OptionalCtorArg("post_migration_hook", func(c *config) {
		if hook != nil {
			c.postMigrationHooks = append(c.postMigrationHooks, hook)
		}
	})
}

func (d *Database) WithMigrations(migrations *migrate.Migrations) DatabaseArg {
	return OptionalCtorArg("migrations", func(c *config) {
		c.migrations = migrations
	})
}

// WithMaxOpenConns sets the maximum number of open connections to the database.
func (d *Database) WithMaxOpenConns(n int) DatabaseArg {
	return OptionalCtorArg("max_open_conns", func(c *config) {
		if n > 0 {
			c.maxOpenConns = n
		}
	})
}

// WithMaxIdleConns sets the maximum number of idle connections in the pool.
func (d *Database) WithMaxIdleConns(n int) DatabaseArg {
	return OptionalCtorArg("max_idle_conns", func(c *config) {
		if n > 0 {
			c.maxIdleConns = n
		}
	})
}

// WithConnMaxLifetimeMs sets the maximum amount of time a connection may be reused (in milliseconds).
func (d *Database) WithConnMaxLifetimeMs(ms int) DatabaseArg {
	return OptionalCtorArg("conn_max_lifetime_ms", func(c *config) {
		if ms > 0 {
			c.connMaxLifetimeMs = ms
		}
	})
}

// WithConnMaxIdleTimeMs sets the maximum amount of time a connection may be idle (in milliseconds).
func (d *Database) WithConnMaxIdleTimeMs(ms int) DatabaseArg {
	return OptionalCtorArg("conn_max_idle_time_ms", func(c *config) {
		if ms > 0 {
			c.connMaxIdleTimeMs = ms
		}
	})
}

func (d *Database) Start(ctx context.Context) {
	if err := d.lifecycle.Start(ctx, d.Logger, common.LifecycleHooks{
		Prepare: d.prepareForRun,
		Run:     d.run,
		OnStarted: func() {
			select {
			case <-d.readyCh:
			default:
				close(d.readyCh)
			}
		},
		OnStop: d.cleanupAfterRun,
	}); err != nil && !errors.Is(err, common.ErrLifecycleAlreadyRunning) && !errors.Is(err, context.Canceled) {
		d.Logger.Error("failed to start", "error", err)
	}
}

func (d *Database) IsRunning() bool {
	d.Lock()
	flagValue := d.isRunning
	sqlDB := d.DB
	d.Unlock()

	// If internal flag says running, trust it (ticker loop maintains this)
	if flagValue {
		return true
	}

	// If flag is false but we have a DB handle, try pinging to verify
	// This catches cases where the DB recovered but the flag hasn't been updated yet
	if sqlDB == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Just return the ping result - DO NOT mutate isRunning here
	// The ticker loop is responsible for managing that flag
	// Mutating here could race with the ticker's reconnection logic
	return sqlDB.PingContext(ctx) == nil
}

// BunDB returns the current Bun database handle in a thread-safe manner.
// Returns nil if the database is not connected.
func (d *Database) BunDB() *bun.DB {
	d.Lock()
	defer d.Unlock()
	return d.Bun
}

// DirectBunDB returns the Bun handle for the direct (non-pooled) endpoint when
// DIRECT_DSN is configured, else falls back to the main (pooled) Bun handle.
// Session-stateful work -- session-scoped advisory locks, leader election,
// migrations -- must resolve its *bun.DB through this getter so it bypasses
// the transaction pooler the bulk pool rides; transaction-mode PgBouncer
// recycles a server backend between statements and would drop a held
// session-scoped lock (epic memql#1925). When DIRECT_DSN is unset the fallback
// keeps behavior identical to today (single pool), so local/dev is unaffected.
func (d *Database) DirectBunDB() *bun.DB {
	d.Lock()
	defer d.Unlock()
	if d.DirectBun != nil {
		return d.DirectBun
	}
	return d.Bun
}

func (d *Database) Order() int {
	return 50
}

func (d *Database) ComponentName() common.ComponentName {
	if d == nil {
		return ""
	}
	return d.componentName
}

// Ready returns a channel that is closed when the database is ready.
func (d *Database) Ready() <-chan struct{} {
	return d.readyCh
}

func (d *Database) Stop(ctx context.Context) {
	if err := d.lifecycle.Stop(ctx, d.Logger); err != nil && !errors.Is(err, common.ErrLifecycleNotRunning) && !errors.Is(err, context.Canceled) {
		d.Logger.Error("failed to stop", "error", err)
	}
}

// PoolStats returns a snapshot of the underlying *sql.DB connection-pool
// statistics (open / in-use / idle counts + wait metrics). The bool reports
// whether a live pool handle was present: false when the database has not
// connected yet or has already been closed. Surfaces per-node active-
// connection counts for the periodic pool-stats log and the drain sequence so
// a deploy can watch the cluster-wide connection total against the TigerDB
// ceiling (memql#1875).
func (d *Database) PoolStats() (sql.DBStats, bool) {
	d.Lock()
	db := d.DB
	d.Unlock()

	if db == nil {
		return sql.DBStats{}, false
	}

	return db.Stats(), true
}

// ClosePool stops the connection monitor and closes the underlying pool,
// handing every open connection back to the database server. It is the
// explicit, intention-revealing entry point the drain sequence calls so a
// draining node releases its TigerDB sessions PROMPTLY instead of holding up
// to MaxOpenConns connections open until the process finally exits at the end
// of the blue-green scale-down window (memql#1875). Returns the pre-close pool
// stats so the caller can log how many connections were released.
//
// The monitor is stopped first (via Stop) so the ticker reconnect loop can't
// race in and re-open the pool we're about to close. Closing is then done
// directly as well, so the method also works (and stays idempotent) when the
// lifecycle was never started or has already stopped -- a second call, e.g.
// the later dependency Stop sweep, is a harmless no-op.
func (d *Database) ClosePool(ctx context.Context) sql.DBStats {
	stats, _ := d.PoolStats()

	// Cancel the monitor first so a ticker tick can't reconnect after we close.
	// No-op (ErrLifecycleNotRunning, swallowed) when not running.
	d.Stop(ctx)

	// Stop's OnStop hook (cleanupAfterRun) already closes + nils the handles
	// when the lifecycle was running. Guard the not-running path by closing any
	// handle still present, so ClosePool releases connections unconditionally.
	d.Lock()
	db := d.DB
	bunDB := d.Bun
	directDB := d.DirectDB
	directBun := d.DirectBun
	gate := d.Gate
	d.DB = nil
	d.Bun = nil
	d.DirectDB = nil
	d.DirectBun = nil
	d.Gate = nil
	d.isRunning = false
	d.Unlock()

	if bunDB != nil {
		_ = bunDB.Close()
	}

	if db != nil {
		_ = db.Close()
	}

	if directBun != nil {
		_ = directBun.Close()
	}

	if directDB != nil {
		_ = directDB.Close()
	}

	if gate != nil {
		_ = gate.Close()
	}

	return stats
}

func (d *Database) run(ctx context.Context, markStarted func()) error {
	ctx = ensureContext(ctx)

	ticker := d.currentTicker()
	if ticker == nil {
		ticker = time.NewTicker(time.Duration(d.config.tickerIntervalMs) * time.Millisecond)
		d.setTicker(ticker)
	}

	startupPingSuccess := d.startupPing(ctx)

	d.Lock()
	d.isRunning = startupPingSuccess
	d.firstPing = false
	d.Unlock()

	if startupPingSuccess {
		d.Logger.Info("startup ping succeeded", "dsn", d.safeDSN())
	} else {
		d.Logger.Warn("startup ping failed; monitoring will retry", "dsn", d.safeDSN())
	}

	markStarted()

	if err := ctx.Err(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			pingSuccess := d.tryPing(ctx)

			d.Lock()
			d.isRunning = pingSuccess
			isFirstPing := d.firstPing
			if d.firstPing {
				d.firstPing = false
			}
			d.Unlock()

			if isFirstPing {
				if pingSuccess {
					markStarted()
					d.Logger.Info("monitoring started", "dsn", d.safeDSN())
				} else {
					d.Logger.Error("monitoring failed", "dsn", d.safeDSN())
				}
			}

			// Surface per-node active-connection counts on the monitor
			// interval so a deploy can watch the cluster-wide connection
			// total against the TigerDB ceiling and confirm draining nodes
			// release their pools (memql#1875). Lightweight: one line per
			// monitor tick, reusing the existing ticker (no new goroutine).
			d.logPoolStats()
		}
	}
}

// logPoolStats emits a single concise connection-pool line (open / in-use /
// idle vs. the configured max, plus wait pressure). A no-op when no live pool
// handle is present. Called on the monitor interval from run (memql#1875).
func (d *Database) logPoolStats() {
	stats, ok := d.PoolStats()
	if !ok {
		return
	}

	d.Logger.Info("db pool stats",
		"open", stats.OpenConnections,
		"inUse", stats.InUse,
		"idle", stats.Idle,
		"maxOpen", stats.MaxOpenConnections,
		"waitCount", stats.WaitCount,
		"waitDuration", stats.WaitDuration.String(),
	)

	// The write-gate pool is a second budget against the same ceiling; a gate
	// that waits for a connection here is the pod being busy, and the line says so.
	if gate := d.GateDB(); gate != nil {
		g := gate.Stats()
		d.Logger.Info("db gate pool stats",
			"open", g.OpenConnections,
			"inUse", g.InUse,
			"maxOpen", g.MaxOpenConnections,
			"waitCount", g.WaitCount,
			"waitDuration", g.WaitDuration.String(),
		)
	}
}

func (d *Database) prepareForRun(ctx context.Context) (context.Context, context.CancelFunc, error) {
	db, err := d.openSQL()
	if err != nil {
		d.Logger.Error("failed to open SQL connection", "error", err)
		return nil, nil, err
	}

	if err := d.runInitHooks(ctx, db); err != nil {
		d.Logger.Error("failed to start", "error", err)
		_ = db.Close()
		return nil, nil, err
	}

	bunDB, err := d.openBun(db)
	if err != nil {
		d.Logger.Error("failed to open Bun", "error", err)
		bunDB = nil
	}

	d.Lock()
	ticker := d.ticker
	if ticker == nil {
		ticker = time.NewTicker(time.Duration(d.config.tickerIntervalMs) * time.Millisecond)
		d.ticker = ticker
	}
	d.DB = db
	d.Bun = bunDB
	d.firstPing = true
	d.Unlock()

	// Open the optional direct (non-pooled) endpoint. Best-effort: a failure
	// leaves DirectBun nil so DirectBunDB() falls back to the main pool rather
	// than blocking startup (epic memql#1925).
	d.openDirectPool()

	// The pool write gates run on (memql#6022): its own connections, so a gate
	// holder never waits for one to do its work. Best-effort like the direct pool.
	d.openGatePool()

	// The migrations themselves run on a session of their own, dialled on the
	// direct endpoint when one is configured, which holds the migration lock as
	// a session advisory lock (migration_session.go). The handle passed here is
	// what the post-migration hooks and a custom migrator run on: the DIRECT
	// pool when configured, else the main one (epic memql#1925).
	d.runMigrations(ctx, d.DirectBunDB())

	runCtx, cancel := context.WithCancel(ctx)
	return runCtx, cancel, nil
}

// openDirectPool opens the second, non-pooled DB endpoint from DIRECT_DSN and
// stores its *sql.DB + *bun.DB on the Database. A no-op when DIRECT_DSN is
// unset. Best-effort on error (logs + leaves the handles nil) so a bad direct
// DSN degrades to the main pool instead of failing the whole node.
func (d *Database) openDirectPool() {
	if d.config.directDSN == nil {
		return
	}

	directDB, err := d.openDirectSQL()
	if err != nil {
		d.Logger.Error("failed to open direct SQL connection; falling back to main pool", "error", err)
		return
	}

	directBun, err := d.openBun(directDB)
	if err != nil {
		d.Logger.Error("failed to open direct Bun; falling back to main pool", "error", err)
		_ = directDB.Close()
		return
	}

	d.Lock()
	d.DirectDB = directDB
	d.DirectBun = directBun
	d.Unlock()

	d.Logger.Info("direct (non-pooled) DB endpoint opened", "dsn", d.safeDirectDSN())
}

func (d *Database) cleanupAfterRun() {
	d.Lock()
	ticker := d.ticker
	db := d.DB
	bunDB := d.Bun
	directDB := d.DirectDB
	directBun := d.DirectBun
	gate := d.Gate
	d.ticker = nil
	d.DB = nil
	d.Bun = nil
	d.DirectDB = nil
	d.DirectBun = nil
	d.Gate = nil
	d.isRunning = false
	d.Unlock()

	if ticker != nil {
		ticker.Stop()
	}

	if bunDB != nil {
		_ = bunDB.Close()
	}

	if db != nil {
		_ = db.Close()
	}

	if directBun != nil {
		_ = directBun.Close()
	}

	if directDB != nil {
		_ = directDB.Close()
	}

	if gate != nil {
		_ = gate.Close()
	}
}

func (d *Database) currentTicker() *time.Ticker {
	d.Lock()
	defer d.Unlock()
	return d.ticker
}

func (d *Database) setTicker(t *time.Ticker) {
	d.Lock()
	if d.ticker != nil {
		d.ticker.Stop()
	}
	d.ticker = t
	d.Unlock()
}

// tryPing is a monitor tick's ping: it checks the pool, reconnects when it
// must, and runs migrations that are still pending.
func (d *Database) tryPing(ctx context.Context) bool {
	return d.ping(ctx, false)
}

// startupPing is the run loop's first ping, the last step before the database
// is marked ready, which the engine waits for under a fixed deadline
// (app/engine.go). It differs from a tick's in one thing: it runs no pending
// migration attempt when the attempt Start just ran reached the database.
//
// Why (2026-11-25). Start runs the first attempt synchronously, before that
// deadline begins, bounded by MIGRATION_TIMEOUT_MS (180 s on production's
// identity). When that attempt deferred, this ping used to run a second one
// before marking the database ready, and the engine's 30 s wait ended the
// process in the middle of it, with an index build running. An attempt that
// reached the database leaves the schema the engine can start on, as a
// replica refused by a sibling's lock always has. The next tick resumes the
// migrations, after ready, so the engine starts while they run.
//
// An attempt that never reached the database, because the node started before
// Postgres did, still runs here: there may be no schema at all (memql#3879).
func (d *Database) startupPing(ctx context.Context) bool {
	return d.ping(ctx, true)
}

// migrateAtThisPing reports whether a ping should run the migration attempt it
// would run: always on a tick, and at startup only when no attempt has reached
// the database yet (startupPing).
func (d *Database) migrateAtThisPing(startup bool) bool {
	if !startup {
		return true
	}
	d.Lock()
	defer d.Unlock()
	return !d.migrationReachedDB
}

func (d *Database) ping(ctx context.Context, startup bool) bool {
	ctx = ensureContext(ctx)

	if err := ctx.Err(); err != nil {
		return false
	}

	d.Lock()
	currentDB := d.DB
	d.Unlock()

	if currentDB != nil {
		// The monitor itself needs a request connection. A saturated pool used
		// to leave this goroutine waiting forever, preserving a stale healthy
		// verdict and preventing even pool-pressure logs from being emitted.
		pingCtx, cancelPing := context.WithTimeout(ctx, 5*time.Second)
		pingErr := currentDB.PingContext(pingCtx)
		cancelPing()
		if pingErr == nil {
			// A POOL THAT STARTS WORKING IS NOT A RECONNECT (memql#3879).
			//
			// Migrations are re-run below, on the reconnect path -- but only a
			// reconnect reaches it, and the cold-start race is not one. When a
			// node starts before the database is up, the startup attempt fails
			// at connect time and the node starts anyway ("monitoring will
			// retry"). pgdriver's pool then heals itself, so the very next tick
			// PINGS SUCCESSFULLY and returns here. The connection is fine
			// forever after; the schema was never created, and nothing ever
			// tries again.
			//
			// The result is a cluster where every pod is Running and Ready and
			// every write fails with `relation "MemoryNodes" does not exist`.
			// Observed on a machine slow enough that postgres spent ~98s waiting
			// on its PVC while the nodes came up around it -- the same condition
			// behind memql#3877 and memql#3873.
			//
			// So the retry belongs on connectivity RETURNING, which is what this
			// branch means, and not on the narrower event of a connection being
			// re-established. It is self-limiting: runMigrations clears the
			// error and sets migrationsRan on success, so this fires until it
			// works and then never again.
			if d.migrationsPending() && d.migrateAtThisPing(startup) {
				d.runMigrations(ctx, d.migrationBun())
			}
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		stats := currentDB.Stats()
		if errors.Is(pingErr, context.DeadlineExceeded) && stats.MaxOpenConnections > 0 && stats.InUse >= stats.MaxOpenConnections {
			// Replacing a busy pool adds connections and Close waits for its
			// borrowers. Leave it intact; report unhealthy and retry next tick.
			d.Logger.Warn("database monitor could not acquire a request connection", "inUse", stats.InUse, "maxOpen", stats.MaxOpenConnections)
			return false
		}

		d.Logger.Warn("ping failed, attempting reconnect", "dsn", d.safeDSN())
	}

	retries := max(d.config.maxConnRetries, 0)
	interval := time.Duration(d.config.maxConnRetriesIntervalMs) * time.Millisecond

	for attempt := range retries {
		if err := ctx.Err(); err != nil {
			return false
		}

		conn, err := d.openSQL()

		if err != nil {
			d.Logger.Error("failed to open SQL connection", "attempt", attempt+1, "error", err)

			continue
		}

		if err := conn.PingContext(ctx); err != nil {
			d.Logger.Error("failed to ping after reconnect", "attempt", attempt+1, "error", err)
			_ = conn.Close()
		} else {
			if err := d.runInitHooks(ctx, conn); err != nil {
				d.Logger.Error("failed to run init hooks", "attempt", attempt+1, "error", err)

				_ = conn.Close()

				continue
			}

			bunDB, err := d.openBun(conn)
			if err != nil {
				d.Logger.Error("failed to open Bun", "attempt", attempt+1, "error", err)

				_ = conn.Close()

				continue
			}

			// The migrations open a session of their own (migration_session.go);
			// the hooks after them run on the DIRECT pool when configured (it
			// persists across a main-pool reconnect), else on the freshly
			// reconnected main bun (epic memql#1925). At startup this is skipped
			// when Start's attempt already reached the database (startupPing).
			migrationBun := bunDB
			d.Lock()
			if d.DirectBun != nil {
				migrationBun = d.DirectBun
			}
			d.Unlock()
			if d.migrateAtThisPing(startup) {
				d.runMigrations(ctx, migrationBun)
			}

			d.Lock()
			oldDB := d.DB
			oldBun := d.Bun
			d.DB = conn
			d.Bun = bunDB
			d.Unlock()

			if oldDB != nil && oldDB != conn {
				_ = oldDB.Close()
			}

			if oldBun != nil && oldBun != bunDB {
				_ = oldBun.Close()
			}

			return true
		}

		if attempt < retries-1 && interval > 0 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return false
			}
		}
	}

	return false
}

func (d *Database) openSQL() (*sql.DB, error) {
	dsn := ""

	if d.config.dsn != nil {
		dsn = *d.config.dsn
	}

	db, err := d.config.sqlOpener(d.config.driverName, dsn)
	if err != nil {
		return nil, err
	}

	// Apply connection pool settings from config (overrides sqlOpener defaults if set)
	if d.config.maxOpenConns > 0 {
		db.SetMaxOpenConns(d.config.maxOpenConns)
	}
	if d.config.maxIdleConns > 0 {
		db.SetMaxIdleConns(d.config.maxIdleConns)
	}
	if d.config.connMaxLifetimeMs > 0 {
		db.SetConnMaxLifetime(time.Duration(d.config.connMaxLifetimeMs) * time.Millisecond)
	}
	if d.config.connMaxIdleTimeMs > 0 {
		db.SetConnMaxIdleTime(time.Duration(d.config.connMaxIdleTimeMs) * time.Millisecond)
	}

	return db, nil
}

// directMaxOpenConns / directMaxIdleConns bound the direct (non-pooled) pool.
// Coordination traffic is FEW and bounded -- a handful of leader-lock holders
// + migrations -- so the pool stays tiny to fit the limited direct Tiger
// budget (the whole point of the hybrid split: bulk traffic rides the pooler,
// only session-stateful work takes a direct slot). It deliberately ignores the
// MAX_OPEN_CONNS / MAX_IDLE_CONNS knobs, which govern only the main pool
// (epic memql#1925).
const (
	directMaxOpenConns = 4
	directMaxIdleConns = 1
)

// The connections a process can open against max_connections, pool by pool, as
// the deploy gate and the budget documentation count them
// (scripts/deploy/conn-headroom-check.sh, which conn_headroom_test.go holds to
// these; docs/public/operate/db-connection-budget.md). With the gate pool
// (DefaultGatePoolSize, gate_pool.go) they are a process's four pools.
const (
	// DefaultRequestPoolSize is the main (request) pool's limit when nothing
	// configures one (MAX_OPEN_CONNS under the database's env prefix).
	DefaultRequestPoolSize = defaultRequestMaxOpenConns
	// DirectPoolSize is the direct pool's fixed limit, opened only when a
	// direct DSN is configured.
	DirectPoolSize = directMaxOpenConns
	// MigrationPoolSize is the connection a startup migration opens.
	MigrationPoolSize = migrationMaxOpenConns
)

// defaultRequestMaxOpenConns and migrationMaxOpenConns are the request pool's
// default limit and the startup migration pool's.
const (
	defaultRequestMaxOpenConns = 10
	migrationMaxOpenConns      = 1
)

// openDirectSQL opens the direct (non-pooled) *sql.DB from DIRECT_DSN using the
// same configured opener as the main pool (so a test-injected opener applies
// to both), then clamps it to a small fixed pool. Caller guarantees
// d.config.directDSN is non-nil.
func (d *Database) openDirectSQL() (*sql.DB, error) {
	dsn := ""
	if d.config.directDSN != nil {
		dsn = *d.config.directDSN
	}

	db, err := d.config.sqlOpener(d.config.driverName, dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(directMaxOpenConns)
	db.SetMaxIdleConns(directMaxIdleConns)

	return db, nil
}

func (d *Database) openBun(sqlDB *sql.DB) (*bun.DB, error) {
	factory := d.config.bunFactory

	if factory == nil {
		return nil, fmt.Errorf("bun factory not configured")
	}

	bunDB, err := factory(sqlDB)

	if err != nil {
		return nil, err
	}

	for _, opt := range d.config.bunOptions {
		if opt != nil {
			opt(bunDB)
		}
	}

	return bunDB, nil
}

func (d *Database) runInitHooks(ctx context.Context, sqlDB *sql.DB) error {
	ctx = ensureContext(ctx)

	if len(d.config.initHooks) == 0 {
		return nil
	}

	if sqlDB == nil {
		return fmt.Errorf("nil database connection")
	}

	for idx, hook := range d.config.initHooks {
		if hook == nil {
			continue
		}
		if err := hook(ctx, sqlDB); err != nil {
			return fmt.Errorf("init hook %d: %w", idx, err)
		}
	}

	return nil
}

// runMigrations runs one migration attempt: the custom migrator when one is
// configured, then the bun migrations on a session of their own under the
// migration lock (runBunMigrations), then the post-migration hooks. bunDB is
// what the custom migrator and the hooks run on. The session is ended last, so
// the hooks run under the same lock as the migrations, as they always have.
func (d *Database) runMigrations(ctx context.Context, bunDB *bun.DB) {
	ctx = ensureContext(ctx)

	if !d.config.migrateOnStart {
		return
	}

	d.Lock()
	d.migrationReachedDB = false
	d.Unlock()

	if bunDB == nil {
		d.Logger.Warn("bun not initialized; skipping migrations")
		return
	}

	runCtx := ctx
	cancel := func() {}

	if timeoutMs := d.config.migrationTimeoutMs; timeoutMs > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	}

	defer cancel()

	// Start each attempt from a clean slate so a successful re-run (e.g. after
	// a reconnect) clears a previously recorded failure. MigrationError() then
	// reflects the outcome of the most recent attempt (#671).
	d.clearMigrationErr()

	if d.config.migrator != nil {
		err := d.config.migrator.Migrate(runCtx, bunDB)
		if err != nil {
			d.Logger.Error("failed to migrate", "error", err)
			d.recordMigrationErr(fmt.Errorf("migrator: %w", err))
		}
		// A custom migrator has no session to say whether it reached the
		// database. Anything but a lost connection says it did.
		if err == nil || !migrationMayStillBeRunning(err) {
			d.markMigrationReachedDB()
		}
	}

	if d.config.migrations != nil {
		if session := d.runBunMigrations(runCtx); session != nil {
			// After the hooks below: they run under the same lock.
			defer session.end()
		}
	}

	// Hooks consume the completed schema. Running them after a failed or
	// deferred migration can make recovery impossible: the Timescale hook may
	// compress rows an earlier migration still needs to rewrite. Keep the
	// attempt pending and let the normal retry finish migrations first.
	if d.MigrationError() != nil {
		return
	}

	if len(d.config.postMigrationHooks) > 0 {
		for idx, hook := range d.config.postMigrationHooks {
			if hook == nil {
				continue
			}

			if err := hook(runCtx, bunDB, d.Logger); err != nil {
				d.Logger.Error("post-migration hook failed", "error", err, "hook_index", idx)
			}
		}
	}

	// Last, so every stage above has had its chance to record a failure. This
	// is what makes the tryPing retry self-limiting: it fires until an attempt
	// completes cleanly, and never again after that.
	d.markMigrationsRan()
}

// runBunMigrations opens the attempt's session, takes the migration lock and
// writes the fence on it, and runs bun's migrations there, every statement on
// that one session (migration_session.go). It returns the session for the
// caller to end once the post-migration hooks have run, or nil when none
// opened. Every refusal and failure is recorded on the way.
func (d *Database) runBunMigrations(attempt context.Context) *migrationSession {
	session, err := d.openMigrationSession(attempt)
	if err != nil {
		d.Logger.Error("failed to open the migration session", "error", err)
		d.recordMigrationErr(fmt.Errorf("migration session: %w", err))
		return nil
	}
	d.markMigrationReachedDB()
	d.rememberMigrationBackend(session.backend)

	migrator := migrate.NewMigrator(session.db, migrationsForAttempt(d.config.migrations, attempt), migrate.WithMarkAppliedOnSuccess(true))
	if err := session.lock(attempt, d.ownMigrationBackend); err != nil {
		d.reportMigrationLockRefusal(attempt, migrator, err)
		d.recordMigrationErr(fmt.Errorf("migration lock: %w", err))
		return session
	}

	// Under the lock, so two engines creating bun's tables on a fresh database
	// cannot race each other.
	if err := migrator.Init(attempt); err != nil {
		d.heldLock.clear()
		d.Logger.Error("failed to initialize migrations", "error", err)
		d.recordMigrationErr(fmt.Errorf("migration init: %w", err))
		return session
	}
	// The stuck watch is cleared only once the fence is in: an engine that locks
	// by row can still refuse the attempt here, and that refusal must find the
	// watch's clock where the last one left it.
	if err := session.fence(attempt); err != nil {
		d.reportMigrationLockRefusal(attempt, migrator, err)
		d.recordMigrationErr(fmt.Errorf("migration lock: %w", err))
		return session
	}
	d.heldLock.clear()

	ledgerCtx, endLedger := migrationLedgerContext(attempt)
	defer endLedger()
	if _, err := migrator.Migrate(ledgerCtx); err != nil {
		if errors.Is(err, ErrMigrationDeferred) {
			// Progress, not a failure: an ERROR here would drown the one a
			// stuck lock logs.
			d.Logger.Warn("migration deferred to the next attempt", "error", err)
		} else {
			d.Logger.Error("failed to migrate", "error", err)
		}
		d.recordMigrationErr(fmt.Errorf("migrate: %w", err))
	}
	return session
}

// markMigrationReachedDB records that this attempt reached the database
// (startupPing reads it).
func (d *Database) markMigrationReachedDB() {
	d.Lock()
	defer d.Unlock()
	d.migrationReachedDB = true
}

// migrationBackendsKept bounds how many of this process's migration sessions
// it remembers. One cut-off statement at a time is what happens in practice.
const migrationBackendsKept = 16

// rememberMigrationBackend records a backend this process's migrations ran on.
func (d *Database) rememberMigrationBackend(b migrationBackend) {
	d.Lock()
	defer d.Unlock()
	d.migrationBackends = append(d.migrationBackends, b)
	if extra := len(d.migrationBackends) - migrationBackendsKept; extra > 0 {
		d.migrationBackends = append([]migrationBackend(nil), d.migrationBackends[extra:]...)
	}
}

// ownMigrationBackend reports whether b is a backend this process's migrations
// ran on: by pid AND start time, because a pid is reused.
func (d *Database) ownMigrationBackend(b migrationBackend) bool {
	d.Lock()
	defer d.Unlock()
	for _, mine := range d.migrationBackends {
		if mine.pid == b.pid && mine.started.Equal(b.started) {
			return true
		}
	}
	return false
}

// ErrMigrationLockHeld marks a migration attempt that ran nothing because
// another migrator holds the migration lock: another engine's migration
// session, or the row of an engine that locks by row (migration_session.go).
// The lock is a TRY-lock, so "held" says something about the world rather than
// about this attempt: another process is migrating, or is confirming there is
// nothing to migrate, or a statement it started is still running, right now.
// A node retries on its monitor tick (tryPing, via migrationsPending). A caller
// that must not proceed on an unmigrated schema and cannot wait for a tick --
// dbtest.EnsureSchema -- tests for this with errors.Is on MigrationError() and
// waits the holder out.
var ErrMigrationLockHeld = errors.New("migration lock held by another migrator")

// ErrMigrationDeferred marks a migration attempt that ended early as progress:
// the next attempt carries on from where this one stopped. These say so:
//
//   - The readiness history collapse (20260921000000), whose work can outlast
//     one attempt's MIGRATION_TIMEOUT_MS (memql#5604): it stopped with its
//     batches committed or rolled back whole.
//   - An index ensure (deferInterruptedIndexWork, latest_row_index.go): a build
//     may still be running, and its backend holds the build lock and, on the
//     migration session, the migration lock until it ends.
//   - The runner itself (migrationsForAttempt), when the attempt ended between
//     migrations: what finished is recorded and what follows has not started.
//   - The lock (migrationSession.lock), when the attempt ended while waiting
//     for this node's own earlier statement to finish.
//
// It decides nothing about the lock, which ends with the attempt's session: a
// session still connected releases it, and one whose connection was cut leaves
// it to the backend until its statement ends. It decides how loudly the runner
// logs and whether the migrate subcommand, which has no monitor tick, starts
// the next attempt at once (awaitMigrations, subcommand_migrate.go). A node
// starts it on its monitor tick (tryPing, via migrationsPending).
var ErrMigrationDeferred = errors.New("deferred to the next migration attempt")

// migrationLedgerGrace is how long bun's own statements may still run once the
// attempt has ended. A ledger insert takes milliseconds; the grace bounds the
// wait for a connection, and the ledger context's deadline bounds the read,
// since the migration session has no driver read deadline of its own.
const migrationLedgerGrace = 5 * time.Second

// migrationsForAttempt runs each migration under the attempt's context and
// leaves bun's own statements on the context Migrate is given
// (migrationLedgerContext), so an attempt that ends between migrations cannot
// read as a statement that may still be running.
//
// bun runs its ledger statements and every Up under the one context it is
// handed. So the deadline landing after an Up returned made bun's insert refuse
// on the expired context, and the runner, which cannot tell that from a
// statement cut off in flight, kept its lock with nothing running at all: that
// is how the September 26 rollout stranded it, one ledger row short. Here the
// insert still lands, and the next Up, finding the attempt over, defers instead
// of starting.
//
// A statement cut off INSIDE an Up is not this case. Nothing here knows what an
// arbitrary migration left running, and nothing has to: its backend is the
// migration session's, which holds the migration lock until the statement ends
// (migration_session.go).
func migrationsForAttempt(all *migrate.Migrations, attempt context.Context) *migrate.Migrations {
	out := migrate.NewMigrations()
	for _, migration := range all.Sorted() {
		if up := migration.Up; up != nil {
			migration.Up = func(_ context.Context, migrator *migrate.Migrator, m *migrate.Migration) error {
				if err := attempt.Err(); err != nil {
					return fmt.Errorf("the attempt ended (%v) before this migration started: %w", err, ErrMigrationDeferred)
				}
				return up(attempt, migrator, m)
			}
		}
		out.Add(migration)
	}
	return out
}

// migrationLedgerContext is the context bun's own statements run under: it
// outlives the attempt by migrationLedgerGrace, whether the attempt ran out or
// the node is stopping. Its deadline, when the attempt has one, is the only
// bound on a ledger statement's read: the migration session has no driver read
// deadline, and pgdriver bounds a read by deadlines, never by cancellation.
func migrationLedgerContext(attempt context.Context) (context.Context, func()) {
	base, endDeadline := context.WithoutCancel(attempt), context.CancelFunc(func() {})
	if deadline, ok := attempt.Deadline(); ok {
		base, endDeadline = context.WithDeadline(base, deadline.Add(migrationLedgerGrace))
	}
	ledger, cancel := context.WithCancel(base)
	stop := context.AfterFunc(attempt, func() { time.AfterFunc(migrationLedgerGrace, cancel) })
	return ledger, func() {
		stop()
		cancel()
		endDeadline()
	}
}

// migrationLockRunbook is where a stuck lock's report sends the operator.
const migrationLockRunbook = "docs/public/operate/env-vars.md#database-tuning"

// migrationLockStuckFloor is the shortest time one holder must stand in the way
// before it is reported as stuck, for a deployment whose attempts are short or
// have no deadline at all.
const migrationLockStuckFloor = 5 * time.Minute

// migrationLockStuckAfter is how long one holder must stand in the way before it
// cannot be an attempt: an attempt is bounded by its deadline and opens a new
// session (or, on an engine that locks by row, a new row) each time, so twice
// the deadline leaves room for a sibling configured the same way and its
// bookkeeping. A sibling with a longer deadline costs a premature report and
// nothing else, because the report releases nothing.
func migrationLockStuckAfter(timeoutMs int) time.Duration {
	return max(2*time.Duration(timeoutMs)*time.Millisecond, migrationLockStuckFloor)
}

// migrationLockWatch is what this node has seen of the holder in the way of its
// attempts. It is per process on purpose: a restart starts the clock again and
// reports again a few minutes later, rather than trusting anything stored.
type migrationLockWatch struct {
	sync.Mutex
	holder string
	since  time.Time
}

// observe records finding holder in the way and says how long this node has
// found it stalled. A different holder is another attempt's, however long the
// last one stood. A holder that is running a statement is not stalled: it
// releases the lock when the statement ends, as on 2026-11-25 when a dead
// replica's build finished eight minutes later. Seeing one starts the clock
// again, so the gauge and the stuck report keep meaning what the release gates
// read them as: migrations stopped until someone acts.
func (w *migrationLockWatch) observe(holder string, stalled bool, now time.Time) time.Duration {
	w.Lock()
	defer w.Unlock()
	if !stalled {
		w.holder, w.since = "", time.Time{}
		return 0
	}
	if w.since.IsZero() || w.holder != holder {
		w.holder, w.since = holder, now
	}
	return now.Sub(w.since)
}

func (w *migrationLockWatch) clear() {
	w.Lock()
	defer w.Unlock()
	w.holder, w.since = "", time.Time{}
}

// MigrationLockHeldFor is how long this node has found the same stalled holder
// of the migration lock in the way of its attempts: zero when nothing is, when
// the holder is running a statement, or once its own attempt got the lock. It
// reads no database, so a /metrics scrape can call it.
func (d *Database) MigrationLockHeldFor() time.Duration {
	d.heldLock.Lock()
	defer d.heldLock.Unlock()
	if d.heldLock.since.IsZero() {
		return 0
	}
	return time.Since(d.heldLock.since)
}

// reportMigrationLockRefusal says what a refused lock means. Moments of
// contention earn the WARN they always did, naming the holder. A holder that has
// stood stalled in the way for longer than any attempt is the installation's
// migrations stopped, and nothing else says so: every pod stays Ready and every
// read keeps its registered plan, just without the indexes it was promised.
// Production sat in that state for days answering the WARN line about 900 times
// a pod in eight hours.
//
// It reports and releases nothing. A row written by an engine that locks by row
// names no holder to ask, so whether it has stopped is the runbook's to
// establish. A session is released by its process ending, which is a restart,
// not a row to delete.
func (d *Database) reportMigrationLockRefusal(ctx context.Context, migrator *migrate.Migrator, err error) {
	var held migrationLockHeldError
	if !errors.As(err, &held) {
		d.Logger.Warn("failed to acquire migration lock; deferring migrations", "error", err)
		return
	}
	heldFor := d.heldLock.observe(held.holder, held.stalled, time.Now())
	if heldFor < migrationLockStuckAfter(d.config.migrationTimeoutMs) {
		d.Logger.Warn("failed to acquire migration lock; deferring migrations", "holder", held.describe)
		return
	}
	args := []any{"holder", held.describe, "heldFor", heldFor.Round(time.Second).String(), "runbook", migrationLockRunbook}
	if statuses, statusErr := migrator.MigrationsWithStatus(ctx); statusErr == nil {
		if pending := statuses.Unapplied(); len(pending) > 0 {
			args = append(args, "pendingMigrations", len(pending), "nextMigration", pending[0].Name)
		}
	}
	if held.row > 0 {
		d.Logger.Error("the migration lock has outlasted any attempt: no migration runs until an operator verifies "+
			"its holder has stopped and releases it", append(args, "lockId", held.row)...)
		return
	}
	d.Logger.Error("the migration lock has outlasted any attempt: its holder's session is connected and running no "+
		"statement, so no migration runs until the process holding it ends that session", args...)
}

// pgUniqueViolation is the SQLSTATE of the refusal an insert meets on the lock
// row's unique key while another row holds it.
const pgUniqueViolation = "23505"

// migrationLockHeldError is a refusal by another holder of the migration lock.
// It answers errors.Is for ErrMigrationLockHeld and for its cause, and carries
// what the stuck watch keys on: the holder, whether it can be stalled, and for
// an engine that locks by row, the row's id.
type migrationLockHeldError struct {
	cause    error
	holder   string
	stalled  bool
	row      int64
	describe string
}

func (e migrationLockHeldError) Error() string   { return e.cause.Error() }
func (e migrationLockHeldError) Unwrap() []error { return []error{ErrMigrationLockHeld, e.cause} }

// migrationMayStillBeRunning reports whether err leaves a statement possibly
// still running on the server: pgdriver sends no PostgreSQL cancel when its
// socket or context deadline expires, a lost connection says nothing about the
// backend behind it, and a migration session that ended had its connection
// lost. SQL errors are acknowledged by the server and end the statement.
//
// The runner no longer decides anything on it: the migration lock is the
// session, and a lost session's backend keeps it until its statement ends
// (migration_session.go). A migration that must say what it left running reads
// it -- an index ensure (deferInterruptedIndexWork) and the readiness collapse.
func migrationMayStillBeRunning(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, errMigrationSessionEnded)
}

// recordMigrationErr stores the first error seen during the current migration
// attempt. Logging stays as-is at each call site; this only makes the failure
// observable to callers via MigrationError() so the gated pre-deploy migrate
// Job can exit non-zero instead of shipping a broken schema behind a green
// gate (#671). Errors are kept first-wins so the originating failure isn't
// masked by a later cascading one.
func (d *Database) recordMigrationErr(err error) {
	if err == nil {
		return
	}

	d.Lock()
	defer d.Unlock()

	if d.migrationErr == nil {
		d.migrationErr = err
	}
}

// clearMigrationErr resets the recorded error at the start of an attempt so a
// successful re-run (e.g. after a reconnect) doesn't leave a stale failure.
func (d *Database) clearMigrationErr() {
	d.Lock()
	defer d.Unlock()

	d.migrationErr = nil
}

// markMigrationsRan records that an attempt finished without error, which is
// what stops the retry in tryPing. Called only after every stage of
// runMigrations has had its chance to record a failure.
func (d *Database) markMigrationsRan() {
	d.Lock()
	defer d.Unlock()

	if d.migrationErr == nil {
		d.migrationsRan = true
	}
}

// migrationsPending reports whether migrations are configured to run and have
// not yet completed successfully.
//
// The two conditions are NOT redundant (memql#3879). `migrationErr != nil` is
// the ordinary "it was tried and it failed" case. `!migrationsRan` additionally
// covers "it was never really tried": a startup attempt that dies at connect
// time can leave no error to find -- the bun handle is nil, runMigrations logs
// "bun not initialized; skipping migrations" and returns having recorded
// nothing -- and reading that silence as success is precisely how a database
// stays empty forever behind a healthy-looking node.
func (d *Database) migrationsPending() bool {
	if !d.config.migrateOnStart {
		return false
	}

	d.Lock()
	defer d.Unlock()

	return !d.migrationsRan || d.migrationErr != nil
}

// migrationBun picks the handle a migration attempt's custom migrator and
// post-migration hooks run on: the DIRECT (non-pooled) endpoint when
// configured, else the main pool. Same rule the reconnect path applies,
// factored out so the two callers cannot drift (epic memql#1925). The bun
// migrations themselves open a session of their own, dialled on the same
// endpoint, which holds the migration lock (migration_session.go).
func (d *Database) migrationBun() *bun.DB {
	d.Lock()
	defer d.Unlock()

	if d.DirectBun != nil {
		return d.DirectBun
	}
	return d.Bun
}

// MigrationError returns the error from the most recent migration attempt, or
// nil if migrations succeeded or were skipped (migrateOnStart=false). The
// gated pre-deploy migrate subcommand consults this to fail the deploy when a
// migration did not complete (#671).
func (d *Database) MigrationError() error {
	d.Lock()
	defer d.Unlock()

	return d.migrationErr
}

func (d *Database) safeDSN() string {
	if d.config.dsn == nil {
		return ""
	}
	return sanitizeDSN(*d.config.dsn)
}

// safeDirectDSN is the password-masked rendering of the direct (non-pooled)
// DSN for logs, or "" when DIRECT_DSN is unset.
func (d *Database) safeDirectDSN() string {
	if d.config.directDSN == nil {
		return ""
	}
	return sanitizeDSN(*d.config.directDSN)
}

// sanitizeDSN masks the password in a DSN so it is safe to log: it handles both
// the `password=...` keyword form and the `scheme://user:pass@host` URL form.
func sanitizeDSN(raw string) string {
	if raw == "" {
		return ""
	}

	if strings.Contains(raw, "password=") {
		return strings.ReplaceAll(raw, "password=", "password=********")
	}

	schemeIdx := strings.Index(raw, "://")

	if schemeIdx == -1 {
		return raw
	}

	rest := raw[schemeIdx+3:]
	atIdx := strings.Index(rest, "@")

	if atIdx == -1 {
		return raw
	}

	credentials := rest[:atIdx]
	colonIdx := strings.Index(credentials, ":")

	if colonIdx == -1 {
		return raw
	}

	sanitizedCreds := credentials[:colonIdx+1] + "********"

	return raw[:schemeIdx+3] + sanitizedCreds + rest[atIdx:]
}

// envDBTLSInsecureSkipVerify is the explicit operator opt-in to skip
// Postgres TLS certificate verification. Set to "1" to bypass; any
// other value (including unset) verifies against the system trust
// pool. Production deployments must leave this unset; a private CA
// can be loaded into the system trust pool instead.
const envDBTLSInsecureSkipVerify = "MEMQL_DB_TLS_INSECURE_SKIP_VERIFY"

// dbTLSInsecureSkipVerifyOptIn reports whether the operator opted
// into bypassing Postgres TLS certificate verification via env var.
// Returns false (verify) unless the env var is literally "1".
func dbTLSInsecureSkipVerifyOptIn() bool {
	return strings.TrimSpace(os.Getenv(envDBTLSInsecureSkipVerify)) == "1"
}

// Per-connection session safety net (memql#1817). pgdriver applies these as
// `SET <name> TO <value>` on every backend it opens, so they bound the
// lifetime of a wedged or orphaned session at the DB instead of letting it
// pin a connection slot until the process exits.
const (
	envDBIdleInTxTimeoutMs    = "MEMQL_DB_IDLE_IN_TX_TIMEOUT_MS"
	envDBIdleSessionTimeoutMs = "MEMQL_DB_IDLE_SESSION_TIMEOUT_MS"
	envDBStatementTimeoutMs   = "MEMQL_DB_STATEMENT_TIMEOUT_MS"
	envDBAppName              = "MEMQL_DB_APP_NAME"

	// 60s: reap a session left idle mid-transaction (a pod that died after
	// BEGIN). Safe default -- healthy code never sits idle-in-transaction.
	defaultIdleInTxTimeoutMs = 60000
	// 5min: reap ANY idle session, which clears orphaned backends left by an
	// ungracefully-killed pod (the "no self-recovery" mechanism in #1817).
	// Safe as a default because it is ABOVE the client-side idle reaping
	// (ConnMaxIdleTime 2min, MaxIdleConns 1), so a LIVE pod always closes its
	// own idle pool connection first -- the server timeout only bites a
	// connection the client can no longer close (i.e. a dead pod's orphan).
	// Long-lived holders stay busy (cron leader polls every 10s), so they are
	// never idle long enough to trip it. Set to 0 to disable.
	defaultIdleSessionTimeoutMs = 300000
	// Cancel serving queries at the server before pgdriver's default 10 s
	// socket read deadline. With a 60 s server limit, each abandoned request
	// kept scanning for another 50 s while its caller retried, amplifying disk
	// contention and starving authentication. Migrations use a separate pool
	// without this limit. Operators may override it for longer request budgets.
	defaultStatementTimeoutMs = 8000
)

// pgSQLOpener builds the default opener over the pgdriver connector. params
// is read at OPEN time, so the request pool and the migration pool differ
// only in the session parameters they SET on every backend.
func pgSQLOpener(params func() map[string]any) SQLOpener {
	return pgSQLOpenerWith(params)
}

// pgSQLOpenerWith is pgSQLOpener with further pgdriver options applied after
// the DSN's own, so they win over it. The maintenance session is the one user:
// it lifts pgdriver's 10 s client read deadline (maintenance_session.go).
func pgSQLOpenerWith(params func() map[string]any, extra ...pgdriver.Option) SQLOpener {
	connect := pgConnectorWith(params, extra...)
	return func(_ string, dsn string) (*sql.DB, error) {
		connector, err := connect(dsn)
		if err != nil {
			return nil, err
		}
		db := sql.OpenDB(connector)

		// Configure connection pool limits to prevent exhaustion.
		// Defaults are conservative so steady+rollout-surge demand across
		// every node stays under the instance's max_connections (memql#1076:
		// 25/node x ~20 pods at surge blew past the Tiger Cloud ceiling ->
		// 53300 storms). Override per env via MAX_OPEN_CONNS / MAX_IDLE_CONNS;
		// budget = (max_connections - reserved) / max_pods(steady+surge).
		db.SetMaxOpenConns(defaultRequestMaxOpenConns) // Max concurrent connections per node
		db.SetMaxIdleConns(1)                          // Keep ONE idle connection warm (memql#1817)
		db.SetConnMaxLifetime(1 * time.Hour)           // Rotate connections hourly
		db.SetConnMaxIdleTime(2 * time.Minute)         // Close idle connections after 2min (memql#1817)

		return db, nil
	}
}

// pgConnectorWith builds the connector every pool dials through: the DSN
// normalized, the session parameters, the application_name, TLS, and the
// retrying wrapper, with extra pgdriver options last so they win. The
// migration session takes it bare, because it decides itself how many
// connections the connector may make (migration_session.go).
func pgConnectorWith(params func() map[string]any, extra ...pgdriver.Option) ConnectorOpener {
	return func(dsn string) (driver.Connector, error) {
		normalized, serverName, err := normalizeDSN(dsn)
		if err != nil {
			return nil, err
		}

		// Per-connection session safety net (memql#1817). These SET
		// commands run on EVERY backend pgdriver opens (incl. reconnects),
		// so a session that gets wedged self-clears at the DB instead of
		// pinning a connection slot until the process exits -- the missing
		// "DB-side reaping" leg (epic #1778 child G), done in code so it
		// holds identically in every environment.
		opts := []pgdriver.Option{pgdriver.WithDSN(normalized)}
		if p := params(); len(p) > 0 {
			opts = append(opts, pgdriver.WithConnParams(p))
		}
		// Stamp application_name so pg_stat_activity attributes each
		// backend to a node type -- turns a future 53300 triage from a
		// guess into a GROUP BY.
		if appName := dbApplicationName(); appName != "" {
			opts = append(opts, pgdriver.WithApplicationName(appName))
		}
		opts = append(opts, extra...)

		connector := pgdriver.NewConnector(opts...)
		cfg := connector.Config()

		// Only configure TLS if sslmode is not explicitly disabled
		// Check if the DSN contains sslmode=disable
		if !strings.Contains(strings.ToLower(normalized), "sslmode=disable") {
			if cfg.TLSConfig == nil {
				// Verify against the system trust pool by default.
				// MinVersion: TLS 1.2 matches Go's modern default
				// and rules out known-broken handshakes. Earlier
				// versions of this file set
				// `InsecureSkipVerify: true`, which let a
				// MITM swap the Postgres certificate for an
				// attacker-controlled one without the client
				// noticing. An operator who genuinely needs to
				// bypass verification (e.g. dev cluster with a
				// self-signed CA they haven't loaded into the
				// system pool) can set
				// `MEMQL_DB_TLS_INSECURE_SKIP_VERIFY=1` and
				// accept the warning logged at startup.
				cfg.TLSConfig = &tls.Config{
					MinVersion:         tls.VersionTLS12,
					InsecureSkipVerify: dbTLSInsecureSkipVerifyOptIn(),
				}
			}
			if serverName != "" && cfg.TLSConfig != nil {
				cfg.TLSConfig.ServerName = serverName
			}
		}

		// Wrap the connector so Connect() retries transient Postgres
		// failures -- connection-slot exhaustion (SQLSTATE 53300) and a
		// dial i/o timeout -- with jittered backoff inside a ~15 s budget,
		// instead of failing the query outright (memql#1076; D7 of the
		// 2026-09-14 readiness-convergence record).
		return newRetryingConnector(connector, slog.Default()), nil
	}
}

// sessionConnParams builds the per-connection SET parameters applied to every
// backend (memql#1817). A value of 0 (or unset, for the off-by-default knob)
// omits the parameter so the server default stands.
func sessionConnParams() map[string]any {
	params := map[string]any{}

	if ms := envIntDefault(envDBIdleInTxTimeoutMs, defaultIdleInTxTimeoutMs); ms > 0 {
		params["idle_in_transaction_session_timeout"] = strconv.Itoa(ms)
	}
	if ms := envIntDefault(envDBIdleSessionTimeoutMs, defaultIdleSessionTimeoutMs); ms > 0 {
		params["idle_session_timeout"] = strconv.Itoa(ms)
	}
	if ms := envIntDefault(envDBStatementTimeoutMs, defaultStatementTimeoutMs); ms > 0 {
		params["statement_timeout"] = strconv.Itoa(ms)
	}

	return params
}

// migrationConnParams is sessionConnParams without the statement timeout: the
// migration runner gets its own single-connection pool built from these, so
// the SERVER never cancels a long index build or backfill at the request
// deadline while every request-serving backend still is.
//
// The CLIENT still stops waiting. The migration run's own context
// (MIGRATION_TIMEOUT_MS) ends a statement's read with an i/o timeout and sends
// no cancel, so the backend carries on server-side while the migration reports
// a failure. The migration session dials with no driver read deadline of its
// own, so that context is the only bound there; a pool that keeps pgdriver's
// 10 s default cuts a statement sooner, the same way. The cut-off backend is the
// migration session's, so it keeps the migration lock until the statement ends
// (migration_session.go). A statement that is meant to outlive an attempt has
// to be written for that: an index build takes a session lock its orphan keeps
// holding, and when the attempt ends first it defers, so the next attempt waits
// and verifies the catalog (deferInterruptedIndexWork, latest_row_index.go).
// Work that does not have to be one statement should not be: the readiness
// history collapse deletes in bounded batches under a server-side
// statement_timeout, keeps its cursor in a table, and stops before the run's
// deadline so the next attempt resumes
// (20260921000000_module_readiness_history_collapse.go).
//
// And without TimescaleDB's cap on DML decompression. "MemoryNodes" is meant to
// compress its 90-day-cold tail (memory_nodes_compression.go, memql#5421 --
// blocked today by a DELETE transition trigger), and once it does, a
// migration that UPDATEs or DELETEs rows in a compressed chunk decompresses
// them, capped per transaction at 100000 tuples by default -- measured to
// refuse a 150000-row, concept-scoped `payload - '<field>'` repair outright,
// which is exactly the shape a retired concept field's repair migration takes.
// The cap guards request traffic from an accidental mass decompression, so the
// request pool keeps it; a migration's bulk DML is deliberate. 0 is unlimited.
// TimescaleDB has carried the setting since 2.11 (the operand image ships 2.28
// and 2.29); a server without TimescaleDB takes it as an unused placeholder.
func migrationConnParams() map[string]any {
	params := sessionConnParams()
	delete(params, "statement_timeout")
	params[migrationDecompressionCapParam] = "0"
	return params
}

// migrationDecompressionCapParam is TimescaleDB's per-transaction cap on
// tuples decompressed by DML, lifted for the migration runner only.
const migrationDecompressionCapParam = "timescaledb.max_tuples_decompressed_per_dml_transaction"

// dbApplicationName derives the Postgres application_name so pg_stat_activity
// attributes each backend to a node type (memql#1817 -- turns a 53300 triage
// from a guess into a GROUP BY). Override with MEMQL_DB_APP_NAME.
func dbApplicationName() string {
	if v := strings.TrimSpace(os.Getenv(envDBAppName)); v != "" {
		return v
	}

	nodeType := strings.TrimSpace(os.Getenv("MEMQL_NODE_TYPE"))
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)

	switch {
	case nodeType != "" && host != "":
		return "memql-" + nodeType + "/" + host
	case nodeType != "":
		return "memql-" + nodeType
	case host != "":
		return "memql/" + host
	default:
		return "memql"
	}
}

// envIntDefault reads an integer env var, returning def when unset, blank, or
// unparseable.
func envIntDefault(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}

	return n
}
