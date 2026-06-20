package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		ticker        *time.Ticker
		firstPing     bool
		isRunning     bool
		config        *config
		lifecycle     common.Lifecycle
		readyCh       chan struct{}
		migrationErr  error
	}

	DatabaseEnvOptions struct {
		DSN                      string
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
		driverName               string
		writer                   io.Writer
		level                    slog.Level
		tickerIntervalMs         int
		maxConnRetries           int
		maxConnRetriesIntervalMs int
		sqlOpener                SQLOpener
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
		sqlOpener: func(_ string, dsn string) (*sql.DB, error) {
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
			if params := sessionConnParams(); len(params) > 0 {
				opts = append(opts, pgdriver.WithConnParams(params))
			}
			// Stamp application_name so pg_stat_activity attributes each
			// backend to a node type -- turns a future 53300 triage from a
			// guess into a GROUP BY.
			if appName := dbApplicationName(); appName != "" {
				opts = append(opts, pgdriver.WithApplicationName(appName))
			}

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
			// connection-slot exhaustion (SQLSTATE 53300) with jittered
			// backoff instead of failing the query outright (memql#1076).
			db := sql.OpenDB(newRetryingConnector(connector, slog.Default()))

			// Configure connection pool limits to prevent exhaustion.
			// Defaults are conservative so steady+rollout-surge demand across
			// every node stays under the instance's max_connections (memql#1076:
			// 25/node x ~20 pods at surge blew past the Tiger Cloud ceiling ->
			// 53300 storms). Override per env via MAX_OPEN_CONNS / MAX_IDLE_CONNS;
			// budget = (max_connections - reserved) / max_pods(steady+surge).
			db.SetMaxOpenConns(10)                 // Max concurrent connections per node
			db.SetMaxIdleConns(1)                  // Keep ONE idle connection warm (memql#1817)
			db.SetConnMaxLifetime(1 * time.Hour)   // Rotate connections hourly
			db.SetConnMaxIdleTime(2 * time.Minute) // Close idle connections after 2min (memql#1817)

			return db, nil
		},
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

func (d *Database) run(ctx context.Context, markStarted func()) error {
	ctx = ensureContext(ctx)

	ticker := d.currentTicker()
	if ticker == nil {
		ticker = time.NewTicker(time.Duration(d.config.tickerIntervalMs) * time.Millisecond)
		d.setTicker(ticker)
	}

	startupPingSuccess := d.tryPing(ctx)

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
		}
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

	d.runMigrations(ctx, bunDB)

	runCtx, cancel := context.WithCancel(ctx)
	return runCtx, cancel, nil
}

func (d *Database) cleanupAfterRun() {
	d.Lock()
	ticker := d.ticker
	db := d.DB
	bunDB := d.Bun
	d.ticker = nil
	d.DB = nil
	d.Bun = nil
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

func (d *Database) tryPing(ctx context.Context) bool {
	ctx = ensureContext(ctx)

	if err := ctx.Err(); err != nil {
		return false
	}

	d.Lock()
	currentDB := d.DB
	d.Unlock()

	if currentDB != nil {
		if err := currentDB.PingContext(ctx); err == nil {
			return true
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

			d.runMigrations(ctx, bunDB)

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

func (d *Database) runMigrations(ctx context.Context, bunDB *bun.DB) {
	ctx = ensureContext(ctx)

	if !d.config.migrateOnStart {
		return
	}

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
		if err := d.config.migrator.Migrate(runCtx, bunDB); err != nil {
			d.Logger.Error("failed to migrate", "error", err)
			d.recordMigrationErr(fmt.Errorf("migrator: %w", err))
		}
	}

	if d.config.migrations != nil {
		migrator := migrate.NewMigrator(bunDB, d.config.migrations, migrate.WithMarkAppliedOnSuccess(true))
		if err := migrator.Init(runCtx); err != nil {
			d.Logger.Error("failed to initialize migrations", "error", err)
			d.recordMigrationErr(fmt.Errorf("migration init: %w", err))
		} else {
			locked := false
			if err := migrator.Lock(runCtx); err == nil {
				locked = true
			} else {
				d.Logger.Warn("failed to acquire migration lock", "error", err)
			}

			if locked {
				defer func() {
					if err := migrator.Unlock(runCtx); err != nil {
						d.Logger.Error("failed to unlock migrations", "error", err)
					}
				}()
			}

			if _, err := migrator.Migrate(runCtx); err != nil {
				d.Logger.Error("failed to migrate", "error", err)
				d.recordMigrationErr(fmt.Errorf("migrate: %w", err))
			}
		}
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

	raw := *d.config.dsn

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
)

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

	return params
}

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
