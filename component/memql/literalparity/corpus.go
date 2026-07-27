// Package literalparity holds the accept/reject corpus shared by the THREE
// near-duplicate payload-literal parsers:
//
//	component/memql            parsePayloadRawToTemplate      (load time)
//	component/language/compiler parsePayloadRaw               (compile time)
//	component/automations/steps parseAndEvaluateObjectLiteral (runtime dispatch)
//
// They are copies, so they drift, and a drift means the same literal is
// accepted at compile time and rejected at dispatch, or the reverse -- a bug
// class no single-copy test can see. Two live divergences were found that way:
// nested string-braces (only the compiler copy rejected) and unterminated
// strings (only the runtime copy accepted).
//
// WHY THIS IS A PACKAGE AND NOT THREE TABLES (memql#2835). The corpus used to
// be duplicated verbatim in three _test.go files under a comment reading "keep
// the three tables identical", with nothing enforcing it. The founding premise
// of this entire line of work is that duplicated copies drift -- so the
// protection against drift was itself three copies kept in step by a comment.
// A shared package makes the drift impossible rather than detectable, which is
// the same treatment #2852 gave DomainFromFilePath and #2872 gave
// BlankComments.
//
// Stdlib-only and dependency-free on purpose: all three consumers must be able
// to import it, and two of them sit on either side of component/memql's own
// import graph.
package literalparity

// Case is one shared expectation: every copy must agree on Accept.
type Case struct {
	Src    string
	Accept bool
}

// Cases is the agreed corpus. A copy that disagrees with any row here has
// drifted and its package-local parity test fails.
var Cases = []Case{
	{`{ k: "ok" }`, true},
	{`{ k: }`, true},
	{`{k:}`, true},
	{`{ k: [,] }`, true},
	{`{ k: ["a", "b"] }`, true},
	{`{ punct: "commas, braces } brackets ]" }`, true},
	{`{ a: { b: "}" } }`, true},
	{`{ a: { b: "a}b" }, c: 1 }`, true},
	{`{ k: "abc }`, false},
	{`{ k: [}{] }`, false},

	// Unbalanced NESTED literals. The runtime copy's nested-object and
	// nested-array arms returned their runoff text as a VALUE, so a single
	// missing brace produced a string where an object belongs. The third case
	// is the sharp one: an unterminated string ONE LEVEL DOWN, whose own
	// ok=false the enclosing arm swallowed -- the fix for unterminated strings
	// did not hold under nesting.
	{`{ a: { b: 1 }`, false},
	{`{ a: [ 1, 2 }`, false},
	{`{ a: { b: """ } }`, false},

	// ...and the balanced counterparts, which must keep parsing.
	{`{ a: { b: 1 } }`, true},
	{`{ a: [ 1, 2 ] }`, true},
	{`{ name: "x", nested: { deep: { deeper: 1 } } }`, true},
}

// Divergence records a literal the three copies do NOT agree on, with each
// copy's current answer.
//
// These are UNFIXED. The point is not that the behaviour is right -- it plainly
// is not, since one input yields three answers -- but that the disagreement is
// WRITTEN DOWN and pinned. An omitted divergence looks like agreement; a pinned
// one fails the moment any copy's answer changes, which forces whoever changed
// it to either fix the divergence or update the record deliberately.
//
// This is the honest form of "the parity table is a spot check": it is, and
// here is what the spot check misses.
type Divergence struct {
	Src string
	// Accept per copy, as MEASURED today.
	MemQL, Compiler, Steps bool
	// Why the case matters, and where it is tracked.
	Note string
}

// KnownDivergences are inputs the three copies answer differently TODAY.
//
// A broader sweep (7,421 inputs: 40 realistic literals plus exhaustive
// brace-wrapped bodies over "{}[]\",: a`" up to length 4) found ~6,300
// disagreements, nearly all on garbage input. This list carries the ones that
// are REALISTIC -- input a person would plausibly write -- because those are
// the ones that can bite. Growing it is expected; shrinking it means a
// divergence was actually fixed.
var KnownDivergences = []Divergence{
	{
		Src:      `{ event.payload.partitionId, name: "explicit" }`,
		MemQL:    false, // "invalid object literal payload"
		Compiler: true,  // {"name":"explicit", "partitionId":"event.payload.partitionId"}
		Steps:    true,  // {"event.payload.partitionId,":"explicit"} -- wrong key AND wrong value
		Note: "Bare-path shorthand, listed in the compiler's own " +
			"TestCompilerAcceptsRealisticPayloads. One input, three answers: the load copy " +
			"REJECTS, the compiler expands the path into a named field, and the runtime copy " +
			"invents a key from the path text (comma included) and steals the NEXT field's " +
			"value. So a payload the compiler accepts is refused at load and, if it got past " +
			"that, dispatched with different data. memql#2835.",
	},
}
