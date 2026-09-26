package database

// The latest-row index's db-gated tests live in the EXTERNAL test package --
// dbtest imports memory-nodes, which imports this package, so an in-package
// test importing dbtest is an import cycle -- and reach the unexported ensurer
// through these aliases. A _test.go file, so nothing outside this package's
// tests can call them: the ensurer is a migration's primitive, and a caller at
// runtime would build indexes outside the migration it belongs to.
var (
	EnsureLatestRowIndex    = ensureLatestRowIndex
	EnsureWorkRecoveryIndex = ensureWorkRecoveryIndex
	InspectLatestRowIndex   = inspectLatestRowIndex
	LatestRowIndexLockKey   = latestRowIndexLockKey
)

const (
	LatestRowIndexValid      = latestRowIndexValid
	LatestRowIndexEquivalent = latestRowIndexEquivalent
	LatestRowIndexBuilt      = latestRowIndexBuilt
	LatestRowIndexRebuilt    = latestRowIndexRebuilt
)
