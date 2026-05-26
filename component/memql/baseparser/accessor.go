package baseparser

import (
	"errors"
	"strings"
)

// AccessorKind classifies a DSL identifier that names one of the
// engine-managed value sources an author can reference (actor /
// args / ctx / payload). Returned by ClassifyAccessor.
//
// `caller.X` and bare `caller` are retired in memql#221; ClassifyAccessor
// returns KindNone + ErrCallerRetired for both, and both DSL parsers
// route their rejection of the retired form through here so the
// user-facing migration hint is identical regardless of which parser
// the author's expression flowed through. That single emit-point is
// the #244 / epic #218 acceptance criterion -- the previous
// implementation duplicated the dispatch (and the migration string)
// across `component/language/parser/parser.go` and
// `component/memql/parser.go`, and the divergence was what made #216
// land in the wrong parser first.
type AccessorKind int

const (
	// KindNone -- the identifier is not a recognised accessor. The
	// calling parser falls through to its own handling (function
	// name lookup, concept name lookup, literal-value handling).
	KindNone AccessorKind = iota
	// KindActor -- `actor` or `actor.<path>`. The auth-context
	// envelope (`userId`, `role`, `identityId`, `isClusterOwner`,
	// `primaryEmail`, `now`, `config.<key>`).
	KindActor
	// KindArgs -- `args` or `args.<path>`. Caller-passed arguments
	// declared in the construct's `args { ... }` block.
	KindArgs
	// KindCtx -- `ctx` or `ctx.<path>`. Legacy runtime-form
	// argument access still emitted by the struct-form rewriter
	// for non-Logic receivers; produces the same downstream
	// ArgReference as args.<path>.
	KindCtx
	// KindPayload -- `payload` or `payload.<path>`. The current
	// row's concept payload (only valid inside row contexts:
	// query filter clauses, shape body paths).
	KindPayload
)

// ErrCallerRetired is the canonical #221 migration hint emitted
// from both parsers' dispatch sites for any `caller.X` / bare
// `caller` accessor. Parsers wrap this in their own
// position-aware error type at the point of use; the inner
// message is identical so a cross-parser test can assert equality.
var ErrCallerRetired = errors.New("caller.X is retired (#221) -- use actor.X (the same auth-context envelope, one canonical spelling)")

// ClassifyAccessor inspects an identifier literal -- bare name or
// dotted form -- and returns the matching accessor kind + the
// dotted suffix path (empty for the bare form).
//
//	"actor"            -> KindActor,   "",         nil
//	"actor.userId"     -> KindActor,   "userId",   nil
//	"actor.config.k"   -> KindActor,   "config.k", nil
//	"args"             -> KindArgs,    "",         nil
//	"args.spaceId"     -> KindArgs,    "spaceId",  nil
//	"ctx"              -> KindCtx,     "",         nil
//	"ctx.spaceId"      -> KindCtx,     "spaceId",  nil
//	"payload"          -> KindPayload, "",         nil
//	"payload.userId"   -> KindPayload, "userId",   nil
//	"caller"           -> KindNone,    "",         ErrCallerRetired
//	"caller.userId"    -> KindNone,    "",         ErrCallerRetired
//	"someOther"        -> KindNone,    "",         nil
//
// Callers fall through (kind=KindNone, err=nil) for anything that
// is not an accessor -- function names, concept names, free-form
// identifiers. The error return is reserved for retired-form
// rejection.
func ClassifyAccessor(literal string) (AccessorKind, string, error) {
	if literal == "caller" || strings.HasPrefix(literal, "caller.") {
		return KindNone, "", ErrCallerRetired
	}
	if kind, path, ok := classify(literal, "actor", KindActor); ok {
		return kind, path, nil
	}
	if kind, path, ok := classify(literal, "args", KindArgs); ok {
		return kind, path, nil
	}
	if kind, path, ok := classify(literal, "ctx", KindCtx); ok {
		return kind, path, nil
	}
	if kind, path, ok := classify(literal, "payload", KindPayload); ok {
		return kind, path, nil
	}
	return KindNone, "", nil
}

// classify matches `bare` exactly or `bare.<path>` and returns the
// path (empty for the bare form). Returns ok=false for any other
// literal so the caller can fall through to the next prefix check.
func classify(literal, bare string, kind AccessorKind) (AccessorKind, string, bool) {
	if literal == bare {
		return kind, "", true
	}
	if path, ok := strings.CutPrefix(literal, bare+"."); ok {
		return kind, path, true
	}
	return KindNone, "", false
}
