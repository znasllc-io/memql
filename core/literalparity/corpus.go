// Package literalparity held the accept/reject/VALUE corpus shared by the
// near-duplicate payload-literal parsers -- component/memql's
// parsePayloadRawToTemplate (load time), component/language/compiler's
// parsePayloadRaw (compile time) and component/automations/steps'
// parseAndEvaluateObjectLiteral (runtime dispatch) -- whose copies drifted, so
// one literal meant different things at load, compile and dispatch
// (memql#2835). The edition-2026 flip (epic memql#5363) deleted all three with
// the string evaluators they fed: a payload's values are parsed by the one
// expression parser now, so there is no second copy left to disagree with, and
// the corpus went with them (memql#5367).
//
// What remains is the one value the copies' no-progress tests shared and
// component/memql's check of the edition-2026 parser still uses.
//
// No MODULE-INTERNAL imports, on purpose: every consumer must be able to
// import it, and they sit on either side of component/memql's own import
// graph. `time` is fine -- stdlib cannot create a cycle back into this
// module.
package literalparity

import "time"

// ParserDeadline is the watchdog window within which the edition-2026 parser
// must refuse a malformed literal (component/memql's no-progress check): a
// scanner that stops advancing is an unbounded allocation, and a bounded
// window turns it into a FAIL line instead of an OOM.
//
// A time.Duration, not a bare number of milliseconds: the untyped form made
// each call site remember `* time.Millisecond`, and
// `ParserDeadlineMillis * time.Second` compiles cleanly to 8m20s.
//
// 500ms, not 2s (memql#2835). A literal parses in well under a microsecond,
// so the margin is enormous either way; a shorter window gives a runaway
// allocation far less room to OOM the process before the FAIL line is
// printed. Measured worst case for a full round trip, under `-race` with a 5%
// CPU quota on one core, was 195ms; on an idle machine it is under 1ms.
const ParserDeadline = 500 * time.Millisecond
