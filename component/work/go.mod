// Part of the memql module split (memql#3228). The work spine's PURE half
// (design record docs/superpowers/specs/2026-09-05-work-spine-design.md,
// sections B, D, E): the symptom rules table, the compile order, the
// postcondition derivation, the footprint union, the replay-serving
// decision and the per-run budget. Everything here is a decision over
// values -- no engine, no database, no provider -- which is what lets the
// headline tests of spec section J run with no database and prove that a
// catalogued goal and a rules-classified symptom make ZERO provider calls.
// The wiring that reaches the engine lives in integrations/work.
module github.com/znasllc-io/memql/component/work

go 1.26.1

toolchain go1.27.1
