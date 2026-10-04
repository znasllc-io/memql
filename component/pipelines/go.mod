// Part of the memql module split (memql#3228). Pipelines' PURE half (epic
// memql#5477, design record
// docs/superpowers/specs/2026-09-16-pipelines-program-design.md, D4-D9): what
// a pipeline IS -- the manifest's pipeline: block, the compiled plan, the
// event-to-mode table, the affected set, the shard split, the check run's
// text and the executor contract -- as functions over values. No engine, no
// database, no network: the standard library only, asserted by
// purity_test.go.
//
// A module rather than a root package for one reason: the substrate's runner
// (epic memql#5478) lives in the integrations module, which does not require
// the root module, so a root-module package would be unreachable from it with
// GOWORK=off. The wiring that reaches the engine lives in component/pipelinerun.
module github.com/znasllc-io/memql/component/pipelines

go 1.26.1

toolchain go1.27.1
