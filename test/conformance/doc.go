// Package conformance is the automated MCP conformance suite (memql#1715),
// the capstone regression gate for epic #1704.
//
// It enumerates the MCP tool surface from the same unified schema source the
// connector consumes (#1713), drives run_query / run_mutation / run_automation
// against a freshly-seeded engine, and asserts the per-dimension contracts the
// epic's ten fixes established:
//
//   - single MCP schema source of truth, no dropped args (#1713)
//   - canonical array envelope for 0 / 1 / many query results (#1710)
//   - partial-update-preserves-siblings for non-create mutations (#1709)
//   - inclusion/exclusion correctness for filtered queries (#1708)
//   - @pii fully scrubbed after a hard delete (#1711)
//   - event-trigger logic binds the triggering event into step-arg scope (#1706)
//   - LiteralValueNode / bare-literal AST evaluation (#1705)
//   - system-generated IDs pass the engine ID validator (#1712)
//   - entry-point logics are invokable via a wrapping automation (#1707)
//   - per-tool create -> read -> update -> delete lifecycle (the suite contract)
//
// The DB-backed dimensions skip cleanly when no Postgres is reachable (so a
// developer's `go test ./...` stays green without a database) and RUN in CI,
// where the `conformance` job provisions a TimescaleDB service and the suite
// migrates it on first connect. The suite publishes a pass-rate summary line
// per build (and a GitHub step-summary table) so the rate can be tracked as a
// release metric.
//
// This package intentionally contains only test files; this doc.go gives
// `go build ./...` a non-test file to compile.
package conformance
