// Part of the memql module split (memql#3228). Tier assignment and
// rationale: docs/internal/ops/ci-design.md, section D3.
//
// The account export's file format: the writer, the checker and the registry
// of hand-overs. It is a module, not a package of the root module, because
// integrations/accounts writes a file and the root module's own components
// hand their tables over to one, and a root-module package cannot be imported
// from a nested module with GOWORK=off. It requires nothing of MemQL's.
module github.com/znasllc-io/memql/component/accountexport

go 1.26.8

toolchain go1.27.4

require filippo.io/age v1.3.2

require (
	filippo.io/hpke v0.4.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)
