package database

// The collapse's fixture test lives in the EXTERNAL test package for the
// latest-row index's reason (latest_row_index_export_test.go): dbtest imports
// memory-nodes, which imports this package. A _test.go file, so nothing outside
// this package's tests can reach these: the collapse is a migration's
// primitive, and a caller at runtime would delete readiness history outside
// the migration it belongs to.
var (
	CollapseModuleReadinessHistory       = collapseModuleReadinessHistory
	RevertModuleReadinessHistoryCollapse = revertModuleReadinessHistoryCollapse
)
