// Package scan holds the CI drift gate for build-tagged test suites
// (memql#2903).
//
// It has no runtime code: the package exists so an UNTAGGED test can walk the
// repository and compare two sources of truth that used to be reconciled by
// hand in a ci.yml comment -- the build tags actually present on *_test.go
// files, and the `go test -tags ...` invocations actually present in
// .github/workflows/.
package scan
