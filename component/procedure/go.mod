// Epic memql#5402, design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md
// section 4 epic C, decision D6. The LEARNING half of the work spine, pure:
// functions over values that turn recorded steps into a parameterized
// template, spending no model. It must stay reachable from
// integrations/procedure with GOWORK=off, which is why it is a module rather
// than a root-module package.
module github.com/znasllc-io/memql/component/procedure

go 1.26.1

toolchain go1.27.1

require github.com/znasllc-io/memql/component/work v0.0.0

replace github.com/znasllc-io/memql/component/work => ../work
