package memoryNodes

import (
	"os"
	"strings"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/core/common"
)

type MemoryNodesDatabase struct {
	*database.TimescaleDBDatabase
}

const (
	ComponentName                = common.ComponentName("memoryNodesDB")
	memoryNodesDatabaseEnvPrefix = "MEMORY_NODES_DATABASE_"
)

func loadMemoryNodesEnvDatabaseOptions() (database.DatabaseEnvOptions, error) {
	opts, err := database.LoadDatabaseEnvOptions(database.DatabaseEnvLoader{Prefix: memoryNodesDatabaseEnvPrefix})
	if err != nil {
		return opts, err
	}
	// Epic 7.3 (memql#2106): the DSN var was renamed
	// MEMORY_NODES_DATABASE_DSN -> MEMQL_DATABASE_DSN. The prefix-composed
	// reader above still resolves the legacy MEMORY_NODES_DATABASE_DSN key
	// (back-compat); prefer the canonical new name when it is set. The
	// boot-time alias shim (genesis.ApplyLegacyEnvAliases) bridges the
	// other direction for an operator who only set the legacy name.
	if dsn := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN")); dsn != "" {
		opts.DSN = dsn
	}
	return opts, nil
}

func loadMemoryNodesDatabaseArgs() ([]database.DatabaseArg, error) {
	options, err := loadMemoryNodesEnvDatabaseOptions()

	if err != nil {
		return nil, err
	}

	return database.EnvOptionsToArgs(options)
}

func NewMemoryNodesDatabase(args ...database.DatabaseArg) (*MemoryNodesDatabase, error) {
	parsedArgs, err := loadMemoryNodesDatabaseArgs()

	if err != nil {
		return nil, err
	}

	combinedArgs := append(parsedArgs, args...)

	db, err := database.NewTimescaleDBDatabase(ComponentName, combinedArgs...)

	if err != nil {
		return nil, err
	}

	return &MemoryNodesDatabase{TimescaleDBDatabase: db}, nil
}
