// Package authoring holds the slice of the memQL authoring / dry-run suite
// that needs the AUTOMATION STEPS registered as well as the engine.
//
// It exists because of a module boundary, not because the tests changed. These
// eight files were `package memql_test` inside `component/memql`, and each one
// blank-imports `component/automations` (or `component/automations/steps`) for
// the init() that registers the Gate-1 compile hook and the Gate-2 dry-run
// runner. `component/automations` depends on `component/memql`; a test import
// is a module requirement, so leaving them there would have made the engine
// module require the automations tier that sits above it -- the exact upward
// edge the `GOWORK=off` lane exists to forbid (memql#3242).
//
// So they moved UP, to the root module, which can see both. The rest of the
// authoring suite -- everything that needs only the engine -- stays in
// `component/memql` where it belongs.
//
// The package is deliberately test-only; this file exists to carry the
// rationale and to give the directory a non-test package clause.
package authoring
