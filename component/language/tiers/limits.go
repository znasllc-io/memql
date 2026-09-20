package tiers

// limits.go -- the cost limits of the M tier (D11 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// An in-process expression is bounded twice. Statically, at load, by an
// estimate of how many node evaluations it can take (component/memql's
// EstimateCost): an expression whose estimate exceeds MaxStaticCost is refused
// before it can run. Dynamically, at run time, by a step budget: every node the
// evaluator visits counts one step, and a call that exceeds its budget stops
// with expression_budget_exceeded instead of running on.
//
// These are MANIFEST VALUES carrying the defaults the record names (section 5,
// "Values, not constants": the depth cap, the budgets, the deprecation window
// and the cost budget are values with the record's defaults). They are declared
// as Go constants today because nothing overrides them yet; a caller that needs
// a different budget passes it on the call (component/memql's
// EvalOptions.Budget) rather than editing these, and the day an operator-facing
// override exists it replaces the constant as the source of the default, not
// the call sites.

// MaxStaticCost is the largest static cost estimate an M-tier expression may
// carry. An expression whose estimate exceeds it is refused at load. The
// record's default is 1,000,000: the same number as the runtime budget, so an
// expression the loader admits is, on the estimate's assumptions, one the
// budget lets finish.
const MaxStaticCost = 1_000_000

// DefaultStepBudget is the number of node evaluations one EvalExpr call may
// perform when its caller names no budget. Exceeding it is the run-time error
// expression_budget_exceeded. The record's default is 1,000,000.
const DefaultStepBudget = 1_000_000

// DefaultCollectionSizeEstimate is the size the static estimate assumes for a
// collection it cannot size at load -- an argument, a step result, a row array
// field. A lambda over such a collection is estimated at this many evaluations
// of its body, and a lambda nested inside another multiplies again, which is
// the point: two nested scans of an unsized list are a million evaluations and
// refuse at MaxStaticCost, where one scan is a thousand and loads.
const DefaultCollectionSizeEstimate = 1_000

// DeprecationWindowMinorReleases is the fourth value in this file's list, and
// the one the comment at the top has named since that list was written: how
// long a public language form warns before it refuses (memql#5390, D22).
//
// D22's wording is "at least two minor releases", so this is a FLOOR rather
// than a default. A caller asking for a shorter window gets this one, because a
// window shorter than the record's minimum is the thing the record forbids, and
// a silently-honoured zero would let a form refuse in the very release that
// deprecated it.
//
// The mechanism is component/language/deprecation, which declares the same
// number as the constant its code reads. It deliberately does NOT import this
// package: the window is consulted from inside the loader, so every import it
// carries is an import taken at load time, and a leaf with no dependencies is
// the shape that cannot introduce a cycle there. The two are held equal by
// TestDeprecationWindowMatchesTheManifest, which is what stops a second copy
// becoming a second answer.
const DeprecationWindowMinorReleases = 2
