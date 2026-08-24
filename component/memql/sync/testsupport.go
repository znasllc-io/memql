package sync

// testsupport.go -- the exported half of the registry's test seam.
//
// resetForTest is unexported and serves this package's own tests. A test
// in ANOTHER package that installs a fake connector -- the inbound
// receiver's, which has to prove that a connector-owned source verifies
// with the store's own secret -- cannot reach it, and leaving a bound
// fake behind would leak into every test that runs after it in the same
// binary.
//
// Exported here rather than by widening resetForTest, because the two
// answer different questions: a test that installed ONE fake wants that
// one gone, not the process's whole registry, which in a binary that
// imports a real connector would also unbind the real one.

// UnbindForTest removes one bound implementation, leaving the
// declaration in place. Test-only.
func UnbindForTest(name string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	delete(registry.bound, name)
}
