// Package literalparity holds the accept/reject/VALUE corpus shared by the
// near-duplicate payload-literal parsers:
//
//	component/language/compiler parsePayloadRaw               (compile time)
//	component/automations/steps parseAndEvaluateObjectLiteral (runtime dispatch)
//
// They are copies, so they drift, and a drift means the same literal behaves
// differently at compile time and at dispatch -- a bug class no single-copy
// test can see. There were THREE until the edition-2026 flip (epic
// memql#5363): component/memql's parsePayloadRawToTemplate read mutation
// values at load, and was deleted with the string evaluator it fed -- a
// mutation's values are parsed by the one expression parser now -- so its
// column went with it.
//
// WHY THIS IS A PACKAGE AND NOT THREE TABLES (memql#2835). The
// corpus used to be duplicated verbatim in three _test.go files under a comment
// reading "keep the three tables identical", with nothing enforcing it. The
// founding premise of this line of work is that duplicated copies drift, so the
// protection against drift was itself three copies kept in step by a comment.
//
// WHY THE AXIS IS THE VALUE, NOT accept/reject (memql#2835 review). The first
// version of this package recorded only whether each copy ACCEPTED a literal,
// and declared the result "the agreed corpus". Measured, that was false on
// SEVEN of sixteen rows: all three accept `{ a: { b: 1 } }`, and they return
// {"b":1}, {"b":1} and {"b":"1"}. `{ port: 5432 }` -- verbatim in
// dsl/cluster/mutations.memql -- is 5432, 5432 and "5432". A bool cannot see
// any of that, so the table documented agreement that does not exist, which is
// exactly the failure the separate "known divergences" list was added to avoid.
//
// It also could not enforce what it documented: the divergent values lived in
// // comments, so changing the compiler's output left every pin green. That is
// the same shape one level down -- a comment saying "the compiler produces X",
// with nothing checking X.
//
// So every row carries each copy's value as canonical JSON, MEASURED, and
// each package asserts its own. A divergence is not a separate list: it is a
// row whose values differ, which Divergent() reports.
//
// No MODULE-INTERNAL imports, on purpose: every consumer must be able to
// import it, and they sit on either side of component/memql's own import
// graph. `time` is fine -- stdlib cannot create a cycle back into this
// module, and an earlier version of this doc claimed "zero imports, not even
// stdlib" as if that were the load-bearing property. It is not; the absence of
// module-internal imports is.
//
// A note on the cited precedent, since the first version overstated it:
// dslfs.DomainFromFilePath (#2852) and BlankComments (#2872) are PRODUCTION
// functions imported by production code. This is a test fixture promoted to a
// production-visible package -- the same mechanism for avoiding drift, but not
// the same justification. component/memql/internal/ would not work (Go's
// internal rule would block the other two packages) and a shared testdata file
// loses compile-time typing and needs brittle relative paths.
package literalparity

import "time"

// Case is one literal and what each copy does with it.
//
// The value strings are canonical JSON of the parsed result, or "ERR" when the
// copy rejects the input. They are MEASURED, not intended: several rows record
// behaviour that is plainly wrong (see Divergent), and recording it is the
// point -- an undocumented divergence is indistinguishable from agreement.
//
// Canonical JSON is BLIND TO NUMERIC KIND: int64(1), float64(1) and
// json.Number("1") all marshal to 1. Verified that no row hides a divergence
// that way -- every type difference across the copies also shows in the JSON
// -- but if one copy ever changes numeric kind alone, this corpus would report
// agreement. They are MEASURED, not intended: several rows record
// behaviour that is plainly wrong (see Divergent), and recording it is the
// point -- an undocumented divergence is indistinguishable from agreement.
type Case struct {
	Src             string
	Compiler, Steps string
}

// Accepted reports whether a copy's recorded outcome is an accept.
func Accepted(value string) bool { return value != "ERR" }

// Divergent reports whether the copies disagree on this row.
//
// Eight of the seventeen rows do, all of them the RUNTIME copy against the
// compiler: it string-types every scalar (1 -> "1"), renders an absent value
// as "" where the compiler gives null, and gives nil where the compiler gives
// []. The sharpest row is the bare-path shorthand, where one input produced
// three different answers including a wrong KEY. (With the load copy gone the
// count did not move: on every divergent row the runtime copy was the one that
// differed.)
func (c Case) Divergent() bool { return c.Compiler != c.Steps }

// Cases is the corpus. Every copy must reproduce its own column exactly.
var Cases = []Case{
	{
		Src:      `{ k: "ok" }`,
		Compiler: `{"k":"ok"}`,
		Steps:    `{"k":"ok"}`,
	},
	{
		Src:      `{ k: }`, // DIVERGENT
		Compiler: `{"k":null}`,
		Steps:    `{"k":""}`,
	},
	{
		Src:      `{k:}`, // DIVERGENT
		Compiler: `{"k":null}`,
		Steps:    `{"k":""}`,
	},
	{
		Src:      `{ k: [,] }`, // DIVERGENT
		Compiler: `{"k":[]}`,
		Steps:    `{"k":null}`,
	},
	{
		Src:      `{ k: ["a", "b"] }`,
		Compiler: `{"k":["a","b"]}`,
		Steps:    `{"k":["a","b"]}`,
	},
	{
		Src:      `{ punct: "commas, braces } brackets ]" }`,
		Compiler: `{"punct":"commas, braces } brackets ]"}`,
		Steps:    `{"punct":"commas, braces } brackets ]"}`,
	},
	{
		Src:      `{ a: { b: "}" } }`,
		Compiler: `{"a":{"b":"}"}}`,
		Steps:    `{"a":{"b":"}"}}`,
	},
	{
		Src:      `{ a: { b: "a}b" }, c: 1 }`, // DIVERGENT
		Compiler: `{"a":{"b":"a}b"},"c":1}`,
		Steps:    `{"a":{"b":"a}b"},"c":"1"}`,
	},
	{
		Src:      `{ k: "abc }`,
		Compiler: "ERR",
		Steps:    "ERR",
	},
	{
		Src:      `{ k: [}{] }`,
		Compiler: "ERR",
		Steps:    "ERR",
	},
	{
		Src:      `{ a: { b: 1 }`,
		Compiler: "ERR",
		Steps:    "ERR",
	},
	{
		Src:      `{ a: [ 1, 2 }`,
		Compiler: "ERR",
		Steps:    "ERR",
	},
	{
		Src:      `{ a: { b: """ } }`,
		Compiler: "ERR",
		Steps:    "ERR",
	},
	{
		Src:      `{ a: { b: 1 } }`, // DIVERGENT
		Compiler: `{"a":{"b":1}}`,
		Steps:    `{"a":{"b":"1"}}`,
	},
	{
		Src:      `{ a: [ 1, 2 ] }`, // DIVERGENT
		Compiler: `{"a":[1,2]}`,
		Steps:    `{"a":["1","2"]}`,
	},
	{
		Src:      `{ name: "x", nested: { deep: { deeper: 1 } } }`, // DIVERGENT
		Compiler: `{"name":"x","nested":{"deep":{"deeper":1}}}`,
		Steps:    `{"name":"x","nested":{"deep":{"deeper":"1"}}}`,
	},
	{
		Src:      `{ event.payload.partitionId, name: "explicit" }`, // DIVERGENT
		Compiler: `{"name":"explicit","partitionId":"event.payload.partitionId"}`,
		Steps:    `{"event.payload.partitionId,":"explicit"}`,
	},
}

// ParserDeadline is the watchdog window the no-progress tests share: the two
// copies' own, and component/memql's check that the edition-2026 parser
// refuses the same malformed literals in bounded time.
//
// A time.Duration, not a bare number of milliseconds. The untyped form made
// each of the three call sites remember `* time.Millisecond`, and
// `ParserDeadlineMillis * time.Second` compiles cleanly to 8m20s -- a
// mini-duplication of exactly the class this package exists to remove.
//
// 500ms, not 2s (memql#2835). These parse in well under a microsecond, so the
// margin is enormous either way; a shorter window gives a runaway allocation
// far less room to OOM the process before the FAIL line is printed. Measured
// worst case for a full mustTerminate round trip, under `-race` with a 5% CPU
// quota on one core -- an absurd configuration -- was 195ms; on an idle machine
// it is under 1ms.
//
// Shared here because the first version of this change DECLARED IT THREE TIMES,
// in a PR whose entire thesis is that duplicated copies drift. Nothing pinned
// the three values equal.
const ParserDeadline = 500 * time.Millisecond
