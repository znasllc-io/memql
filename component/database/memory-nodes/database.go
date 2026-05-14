package memoryNodes

import (
	"github.com/visionarys-io/memql/component/database"
	"github.com/visionarys-io/memql/core/common"
)

type MemoryNodesDatabase struct {
	*database.TimescaleDBDatabase
}

const (
	ComponentName                = common.ComponentName("memoryNodesDB")
	memoryNodesDatabaseEnvPrefix = "MEMORY_NODES_DATABASE_"
)

func loadMemoryNodesEnvDatabaseOptions() (database.DatabaseEnvOptions, error) {
	return database.LoadDatabaseEnvOptions(database.DatabaseEnvLoader{Prefix: memoryNodesDatabaseEnvPrefix})
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
