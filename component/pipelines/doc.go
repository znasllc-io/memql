// Package pipelines is the pure core of MemQL's pipelines (epic memql#5477):
// a pipeline declared in a repository's memql-package.yaml, delivered by the
// GitHub App, compiled to a work goal and reported back as a check run.
//
// Everything here is a decision over values. The engine side -- reading rows,
// minting tokens, writing check runs, driving a run over the work spine --
// lives in component/pipelinerun; the place a step actually executes is an
// Executor, registered by the substrate (epic memql#5478).
//
// The contract in contract.go and executor.go is shared with that substrate,
// which lives in another module and another pull request. Its JSON field
// names are the wire: a request crosses NodeService as bytes, so renaming a
// tag is a protocol change on both sides at once.
package pipelines
