package database

import (
	"slices"

	"github.com/znasllc-io/memql/core/common"
)

type PostgresDatabase struct {
	*Database
}

func NewPostgresDatabase(componentName common.ComponentName, args ...DatabaseArg) (*PostgresDatabase, error) {
	db, err := NewDatabase(componentName, slices.Clone(args)...)

	if err != nil {
		return nil, err
	}

	return &PostgresDatabase{Database: db}, nil
}
