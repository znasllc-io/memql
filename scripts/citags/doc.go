// Package citags holds the CI drift gate for build-tagged test suites
// (memql#2903).
//
// It has no runtime code. The package exists so an UNTAGGED test can walk the
// repository and compare two sources of truth that used to be reconciled by
// hand in a ci.yml comment: the build constraints actually on *_test.go files,
// and the `go test -tags ...` steps actually in .github/workflows/.
//
// It lives under scripts/ rather than cmd/ because cmd/ means "a binary" --
// every other cmd/* has a main package. The precedent is
// scripts/lib/capability_contract_test.go, a test-only gate beside the thing it
// gates.
package citags
