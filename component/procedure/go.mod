// Epic memql#5402, design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md
// section 4 epic C, decision D6. The LEARNING half of the work spine, pure:
// functions over values that turn recorded steps into a parameterized
// template, spending no model. It must stay reachable from
// integrations/procedure with GOWORK=off, which is why it is a module rather
// than a root-module package.
// It has NO requires. The design record allows this module the standard
// library and component/work; it turned out to need only the first, because
// every goalSignature it groups by is read off a row by the wiring rather than
// derived here. A require nothing imports is one `go mod tidy -diff` removes,
// so the narrower truth is what ships -- see purity_test.go for how to widen
// it again if a function here ever needs work.GoalSignature.
module github.com/znasllc-io/memql/component/procedure

go 1.26.1

toolchain go1.27.1
