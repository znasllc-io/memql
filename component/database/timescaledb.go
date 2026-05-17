package database

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

type TimescaleDBDatabase struct {
	*PostgresDatabase
}

const (
	memoryNodesTableName       = "MemoryNodes"
	secretMemoryNodesTableName = "SecretMemoryNodes"
)

var (
	//go:embed memory-nodes/migrations/*.sql
	timescaleMigrationsFS embed.FS
)

func NewTimescaleDBDatabase(componentName common.ComponentName, args ...DatabaseArg) (*TimescaleDBDatabase, error) {
	logger := logger.New(componentName, os.Stdout, slog.LevelInfo)
	options := append([]DatabaseArg{withTimescaleMigrations(logger)}, slices.Clone(args)...)
	db, err := NewPostgresDatabase(componentName, options...)

	if err != nil {
		return nil, err
	}

	return &TimescaleDBDatabase{PostgresDatabase: db}, nil
}

func withTimescaleMigrations(logger *slog.Logger) DatabaseArg {
	return OptionalCtorArg("timescaledb_migrations", func(cfg *config) {
		if cfg.migrations == nil {
			cfg.migrations = migrate.NewMigrations()
		}

		registerTimescaleMigrations(cfg.migrations, logger)
		cfg.initHooks = append(cfg.initHooks, timescaleExtensionInitHook(logger))
		cfg.postMigrationHooks = append(cfg.postMigrationHooks, timescaleExtensionPostHook(logger))
	})
}

func registerTimescaleMigrations(m *migrate.Migrations, logger *slog.Logger) {
	if m == nil {
		return
	}

	if err := m.Discover(timescaleMigrationsFS); err != nil {
		if logger != nil {
			logger.Error("failed to discover timescale migrations", "error", err)
		}
		panic(err)
	}
}

func timescaleExtensionPostHook(fallbackLogger *slog.Logger) PostMigrationHook {
	return func(ctx context.Context, bunDB *bun.DB, componentLogger *slog.Logger) error {
		if bunDB == nil {
			if componentLogger != nil {
				componentLogger.Warn("skipping TimescaleDB verification; Bun database is nil")
			} else if fallbackLogger != nil {
				fallbackLogger.Warn("skipping TimescaleDB verification; Bun database is nil")
			}
			return nil
		}

		logger := componentLogger
		if logger == nil {
			logger = fallbackLogger
		}

		if logger == nil {
			return nil
		}

		ctx = ensureContext(ctx)

		const (
			extensionName    = "timescaledb"
			statusId         = "system.timescaledb.status"
			statusCreatedBy  = "system"
			statusPartition  = "default"
		)

		statusCreatedAt := time.Unix(0, 0).UTC()

		var available bool
		if err := bunDB.NewSelect().
			ColumnExpr("COUNT(*) > 0").
			TableExpr("pg_available_extensions").
			Where("name = ?", extensionName).
			Scan(ctx, &available); err != nil {
			logger.Warn("failed to check TimescaleDB availability", "error", err)
		}

		var activationErr error
		if available {
			if _, err := bunDB.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
				activationErr = err
				logger.Error("failed to create TimescaleDB extension", "error", err)
			}
		} else {
			activationErr = errors.New("extension not available on server")
			logger.Error("TimescaleDB extension not available on this server; hypertable features unavailable")
		}

		type extensionInfo struct {
			Version sql.NullString `bun:"version"`
			Schema  sql.NullString `bun:"schema"`
		}

		var info extensionInfo
		err := bunDB.NewSelect().
			TableExpr("pg_extension AS e").
			ColumnExpr("e.extversion AS version").
			ColumnExpr("ns.nspname AS schema").
			Join("JOIN pg_namespace AS ns ON ns.oid = e.extnamespace").
			Where("e.extname = ?", extensionName).
			Limit(1).
			Scan(ctx, &info)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			logger.Error("TimescaleDB extension not enabled; hypertable features unavailable")
		case err != nil:
			logger.Error("failed to verify TimescaleDB extension", "error", err)
		}

		var hasCreateHypertable bool
		if err := bunDB.NewSelect().
			ColumnExpr("COUNT(*) > 0").
			TableExpr("pg_proc").
			Where("proname = ?", "create_hypertable").
			Scan(ctx, &hasCreateHypertable); err != nil {
			logger.Warn("unable to confirm presence of create_hypertable function", "error", err)
		}

		hypertableResults := map[string]bool{}

		if hasCreateHypertable {
			existing := map[string]struct{}{}
			var hypertableNames []string

			if err := bunDB.NewRaw("SELECT hypertable_name FROM timescaledb_information.hypertable").
				Scan(ctx, &hypertableNames); err != nil {
				errMsg := err.Error()
				if strings.Contains(errMsg, "timescaledb_information") || strings.Contains(errMsg, "42P01") {
					logger.Info("timescaledb_information.hypertable view not available yet; relying on create_hypertable")
				} else {
					logger.Warn("failed to list existing hypertables", "error", err)
				}
			} else {
				for _, name := range hypertableNames {
					existing[name] = struct{}{}
				}
			}

			tables := []string{memoryNodesTableName, secretMemoryNodesTableName}

			for _, table := range tables {
				if _, ok := existing[table]; ok {
					hypertableResults[table] = true
					continue
				}

				query := fmt.Sprintf("SELECT create_hypertable('%s'::regclass, 'createdAt', migrate_data => TRUE, if_not_exists => TRUE)", quoteIdentifier(table))
				if _, err := bunDB.ExecContext(ctx, query); err != nil {
					if isUndefinedRelationError(err) {
						logger.Warn("failed to ensure hypertable; table missing (run migrations?)", "table", table, "error", err)
					} else {
						logger.Warn("failed to ensure hypertable", "table", table, "error", err)
					}
					hypertableResults[table] = false
				} else {
					hypertableResults[table] = true
				}
			}
		}

		installed := info.Version.Valid
		version := ""

		if installed {
			version = info.Version.String
		}

		schema := ""
		if info.Schema.Valid {
			schema = info.Schema.String
		}

		payload := map[string]any{
			"installed":           installed,
			"version":             version,
			"schema":              schema,
			"hasCreateHypertable": hasCreateHypertable,
			"available":           available,
			"checked_at":          time.Now().UTC().Format(time.RFC3339Nano),
		}

		if activationErr != nil {
			payload["activation_error"] = activationErr.Error()
		}

		if len(hypertableResults) > 0 {
			payload["hypertables"] = hypertableResults
		}

		payloadJSON, payloadErr := json.Marshal(payload)

		if payloadErr != nil {
			logger.Warn("failed to marshal TimescaleDB status payload", "error", payloadErr)
		}

		schemaJSON, schemaErr := json.Marshal(map[string]any{
			"type":   "timescaledb_extension_status",
			"source": "runtime",
		})

		if schemaErr != nil {
			logger.Warn("failed to marshal TimescaleDB status schema", "error", schemaErr)
		}

		if payloadErr == nil && schemaErr == nil {
			type statusRecord struct {
				bun.BaseModel `bun:"table:MemoryNodes"`
				Partition     string          `bun:",pk,notnull,default:'default'"`
				ID            string          `bun:",pk"`
				CreatedAt     time.Time       `bun:"\"createdAt\",pk"`
				CreatedBy     string          `bun:"\"createdBy\",notnull"`
				Concept       string          `bun:",notnull"`
				Schema        json.RawMessage `bun:"type:JSONB,notnull"`
				Payload       json.RawMessage `bun:"type:JSONB,notnull"`
			}

			node := &statusRecord{
				Partition: statusPartition,
				ID:        statusId,
				CreatedAt: statusCreatedAt,
				CreatedBy: statusCreatedBy,
				Concept:   "system.migration",
				Schema:    schemaJSON,
				Payload:   payloadJSON,
			}

			if _, err := bunDB.NewInsert().
				Model(node).
				On(`CONFLICT (partition, id, "createdAt") DO UPDATE`).
				Set("schema = EXCLUDED.schema, payload = EXCLUDED.payload").
				Exec(ctx); err != nil {
				logger.Warn("failed to upsert TimescaleDB status node", "error", err)
			}
		}

		if installed {
			logger.Info(
				"timescaledb extension enabled",
				"version", version,
				"schema", schema,
				"hasCreateHypertable", hasCreateHypertable,
				"hypertables", hypertableResults,
			)
		} else if activationErr == nil {
			logger.Error("TimescaleDB extension not enabled after attempted activation")
		}
		return nil
	}
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isUndefinedRelationError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "42P01")
}

func timescaleExtensionInitHook(fallbackLogger *slog.Logger) InitHook {
	return func(ctx context.Context, sqlDB *sql.DB) error {
		if sqlDB == nil {
			return nil
		}

		logger := fallbackLogger
		ctx = ensureContext(ctx)

		if _, err := sqlDB.ExecContext(ctx, "SELECT 1"); err != nil {
			logger.Warn("TimescaleDB availability check failed", "error", err)
			return nil
		}

		if _, err := sqlDB.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
			logger.Error("failed to create TimescaleDB extension during init", "error", err)
		}

		return nil
	}
}

func (db *TimescaleDBDatabase) Migrator() (*migrate.Migrator, error) {
	if db == nil {
		return nil, fmt.Errorf("timescaledb database is nil")
	}

	if db.PostgresDatabase == nil || db.Database == nil {
		return nil, fmt.Errorf("database is not initialized")
	}

	db.Database.Lock()
	bunDB := db.Database.Bun
	cfg := db.Database.config
	migrations := db.Database.config.migrations
	db.Database.Unlock()

	if bunDB == nil {
		return nil, fmt.Errorf("bun database is not initialized")
	}

	if cfg == nil {
		return nil, fmt.Errorf("database configuration is not initialized")
	}

	if migrations == nil {
		return nil, fmt.Errorf("no migrations registered")
	}

	return migrate.NewMigrator(bunDB, migrations, migrate.WithMarkAppliedOnSuccess(true)), nil
}
