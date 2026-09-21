package parser

// source_size.go -- the bounds on how much DSL source a WIRE surface accepts
// in one request.
//
// These are COST bounds, not safety ones. What keeps an oversized source from
// ending the process is the parser's own bounds on what it BUILDS -- the
// nesting bound (nesting_bound.go) and the chain bound (chain_bound.go) --
// which hold however many bytes arrive. What these bounds are for is the work
// a source costs before any of that can refuse it: tokenising costs memory in
// proportion to the TOKENS in a source rather than to its bytes, and the work
// after it is worse than linear. Diagnose over ordinary, well-formed source
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
// messages up to 32 MiB (raised for screenshots), which is where the bytes
// these bounds are about come from.
//
// # Two bounds, because the two payloads carry different things
//
// A Sense request's `source` is ONE FILE, the one an author has open. The
// largest .memql file in this repository is 135,297 bytes, so MaxSourceBytes
// (512 KiB) is nearly four times the largest source anyone edits, and the
// worst request it admits costs about a sixth of a second.
//
// An authoring payload's `sources` is a BUNDLE -- "one or more constructs
// concatenated" (component/grpc/memql.proto) -- so the file figure is the
// wrong one to size it against. The largest whole DOMAIN in this repository is
// dsl/identity/ at 444,924 bytes, which 512 KiB clears by only 1.18x: "stage
// this domain" is a plausible SDK or CI call that would start being refused
// after about 18% of growth. MaxBundleBytes (2 MiB) is 4.7 times that domain,
// and the worst request it admits costs about a second and 25 GB of
// allocation churn.
//
// That second is deliberately spent, and it is a COST decision rather than a
// safety one: with the nesting and chain bounds in place the residual risk on
// the bundle path is CPU, not a crash. The authoring payloads are also gated
// on the owner or developer role (auth.CanAuthor), where the Sense payloads
// are open to every signed-in client. The cost is written down here so the
// next reader does not have to measure it again.
//
// Source read from LOCAL files -- the language server, memqllint, a node
// loading its own tree -- is the operator's own and is held to neither bound.

import "fmt"

// MaxSourceBytes is the most DSL source, in bytes, a wire surface accepts for
// ONE FILE: every Sense request's `source`, and the MCP
// `run_inline_automation` tool's `source`.
const MaxSourceBytes = 512 << 10

// MaxBundleBytes is the most a wire surface accepts for a BUNDLE of
// constructs: the five authoring payloads' `sources`, and the MCP `define`
// tool's `bundle`.
const MaxBundleBytes = 2 << 20

// RuleSourceTooLarge is the stable code of the refusal of a source over the
// bound that applies to it: the last thing its message prints, in brackets.
const RuleSourceTooLarge = "source_too_large"

// SourceTooLargeError is the refusal of a source over its bound.
type SourceTooLargeError struct {
	// Bytes is the refused source's size.
	Bytes int
	// Limit is the bound it was held to: MaxSourceBytes or MaxBundleBytes.
	Limit int
}

// Error is the size, the bound and the remedy, and the code last, in brackets
// (D24).
func (e *SourceTooLargeError) Error() string {
	return fmt.Sprintf("source is %d bytes, over the %s limit: send one file at a time, or split it into smaller files [%s]",
		e.Bytes, byteLimitLabel(e.Limit), RuleSourceTooLarge)
}

// RuleCode is RuleSourceTooLarge (baseloader.CodedRefusal).
func (e *SourceTooLargeError) RuleCode() string { return RuleSourceTooLarge }

// byteLimitLabel writes a whole-power-of-two bound the way a person reads it.
func byteLimitLabel(limit int) string {
	if limit >= 1<<20 {
		return fmt.Sprintf("%d MiB", limit>>20)
	}
	return fmt.Sprintf("%d KiB", limit>>10)
}

// CheckSourceSize refuses ONE FILE of source over MaxSourceBytes. It reads
// only the length -- no copy, no allocation -- so a wire surface calls it
// before anything touches the source.
func CheckSourceSize(source string) error { return checkSize(source, MaxSourceBytes) }

// CheckBundleSize refuses a BUNDLE of constructs over MaxBundleBytes, and is
// CheckSourceSize in every other respect.
func CheckBundleSize(sources string) error { return checkSize(sources, MaxBundleBytes) }

func checkSize(source string, limit int) error {
	if len(source) > limit {
		return &SourceTooLargeError{Bytes: len(source), Limit: limit}
	}
	return nil
}
