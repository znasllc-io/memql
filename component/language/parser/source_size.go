package parser

// source_size.go -- the bound on how much DSL source a WIRE surface accepts in
// one request.
//
// The parser refuses a source nested past v1MaxDepth or chained past
// MaxExpressionChain (chain_bound.go), but only once it reads tokens, and the
// lexer has tokenised the whole source by then. Tokenising costs memory in
// proportion to the TOKENS in a source rather than to its bytes, and the work
// after it is worse than linear: Diagnose over ordinary, well-formed source
// measured on this tree at
//
//	128 KiB    29 ms     0.16 GB allocated
//	256 KiB    69 ms     0.50 GB
//	512 KiB   172 ms     1.77 GB
//	  1 MiB   415 ms     6.5 GB
//	  2 MiB   1.08 s      25 GB
//	  4 MiB   3.55 s      96 GB
//
// -- about four times the cost for twice the source. The gRPC stream accepts
// messages up to 32 MiB (raised for screenshots) and each Sense request runs
// in a goroutine of its own with no concurrency bound, so without a bound here
// one signed-in client decides how much CPU and memory a node spends.
//
// MaxSourceBytes is therefore 512 KiB: the largest .memql file in this
// repository is 135,297 bytes, so it is nearly four times the largest source
// anyone edits, and the worst request it admits costs about a sixth of a
// second rather than the four seconds 4 MiB costs. Source read from LOCAL
// files -- the language server, memqllint, a node loading its own tree -- is
// the operator's own and is not held to it.

import "fmt"

// MaxSourceBytes is the most DSL source, in bytes, a wire surface accepts in
// one request. See the file comment for the measurement behind the number.
const MaxSourceBytes = 512 << 10

// RuleSourceTooLarge is the stable code of the refusal of a source over
// MaxSourceBytes: the last thing its message prints, in brackets.
const RuleSourceTooLarge = "source_too_large"

// SourceTooLargeError is the refusal of a source over MaxSourceBytes.
type SourceTooLargeError struct {
	// Bytes is the refused source's size.
	Bytes int
}

// Error is the size, the bound and the remedy, and the code last, in brackets
// (D24).
func (e *SourceTooLargeError) Error() string {
	return fmt.Sprintf("source is %d bytes, over the %d KiB limit: send one file at a time, or split it into smaller files [%s]",
		e.Bytes, MaxSourceBytes>>10, RuleSourceTooLarge)
}

// RuleCode is RuleSourceTooLarge (baseloader.CodedRefusal).
func (e *SourceTooLargeError) RuleCode() string { return RuleSourceTooLarge }

// CheckSourceSize refuses a source over MaxSourceBytes. It reads only the
// length -- no copy, no allocation -- so a wire surface calls it before
// anything touches the source.
func CheckSourceSize(source string) error {
	if len(source) > MaxSourceBytes {
		return &SourceTooLargeError{Bytes: len(source)}
	}
	return nil
}
