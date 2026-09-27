package database

// The MemoryNodes compression cases are db-gated and live in the EXTERNAL test
// package for the latest-row index's reason (latest_row_index_export_test.go):
// dbtest imports memory-nodes, which imports this package. A _test.go file, so
// nothing outside this package's tests can reach these.
var (
	EnsureMemoryNodesCompression = ensureMemoryNodesCompression
	MigrationConnParams          = migrationConnParams
	SessionConnParams            = sessionConnParams
	PgSQLOpener                  = pgSQLOpener
)

const (
	CompressionEnabledThisBoot     = compressionEnabledThisBoot
	CompressionAlreadyEnabled      = compressionAlreadyEnabled
	CompressionNotAHypertable      = compressionNotAHypertable
	CompressionNoTimescaleDB       = compressionNoTimescaleDB
	CompressionBlockedPrefix       = compressionBlockedPrefix
	MigrationDecompressionCapParam = migrationDecompressionCapParam
)
