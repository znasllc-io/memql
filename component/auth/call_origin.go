package auth

import "context"

// CallOrigin records whether a DSL construct call arrived from an untrusted
// CLIENT over the wire or from trusted server-side Go.
//
// It exists because there was no way to tell (memql#2800). Client-submitted
// query text and server-side Go calls converge on the identical
// MemQLEngine.Execute, and nothing between the wire and the executor inspected
// which one it was -- so a construct the auth path needs to read cross-user
// (userByIdSystem resolving `sub` -> user BEFORE an actor exists) was equally
// reachable by any authenticated caller passing someone else's id.
//
// The annotations that looked like they solved this do not. `@internal` on a
// construct was RETIRED (#2620 / #2708) because it only hid the construct from
// discovery surfaces while leaving it callable; `@role` and `@permission` were
// retired as "documented but never enforced"; `@public` is a parse-only marker
// with no runtime semantics, read solely by a conformance test. This is the
// first origin signal that is actually consulted at execution time.
//
// # Why the zero value is OriginClient
//
// This is the whole safety property, and it is deliberately the inconvenient
// choice. An unstamped context is UNTRUSTED, so:
//
//   - a new client entry point that forgets to stamp gets the RESTRICTED
//     treatment -- it cannot reach a @serverOnly construct;
//   - a new server-side caller that forgets to stamp gets a loud, immediate
//     failure naming the construct, not a silent privilege grant.
//
// The reverse default would invert both: forgetting would mean "trusted", and
// the failure mode would be a silent hole rather than a broken call. Every
// retired annotation above failed in exactly that direction.
//
// Do NOT derive this from claims, a role, or anything the caller can influence.
// `Role == "system"` is a database fact about a user row, not a statement about
// which channel the call arrived on; conflating the two would make the gate
// spoofable by anyone who can get that role. Only Go code that is itself the
// trusted caller may stamp OriginInternal, and it must do so on a context it
// constructs -- never on one handed to it by a request handler.
type CallOrigin int

const (
	// OriginClient is untrusted: the call arrived over the wire, or its
	// origin was never established. Zero value on purpose -- see above.
	OriginClient CallOrigin = iota

	// OriginInternal is trusted server-side Go, stamped explicitly by the
	// calling package via ContextWithInternalOrigin.
	OriginInternal
)

// String renders the origin for error messages and logs.
func (o CallOrigin) String() string {
	if o == OriginInternal {
		return "internal"
	}
	return "client"
}

// IsInternal reports whether the call may reach @serverOnly constructs.
func (o CallOrigin) IsInternal() bool { return o == OriginInternal }

// callOriginKey is deliberately NOT a field on AccessContext. Server-side
// callers frequently have no AccessContext at all -- the identity resolver
// runs before one exists, since resolving the user IS how one gets built --
// so hanging the origin off it would make "trusted" and "authenticated"
// the same bit, which they are not.
const callOriginKey contextKey = "callOrigin"

// # WHAT TO DO WHEN YOU HIT THE BAN (memql#4769)
//
// A gRPC or node handler that needs a @serverOnly construct finds the door
// shut: the ban above is unconditional for component/grpc and component/node.
// That is correct, and it is NOT "there is no way to do this". There are two
// sanctioned answers and one forbidden one, in this order.
//
//  1. MAKE THE CONSTRUCT CALLER-SCOPED, and need no origin signal at all.
//     If the operation is about the CALLER's own rows, say so in the
//     construct: `filter subject==actor.userId` on a read,
//     `stamp { ownerUserId: actor.userId }` on a write. The caller-supplied
//     id disappears, and with it the reason @serverOnly was wanted.
//
//     This is the established answer, twice over. memql#2989 refuted the
//     @serverOnly route for two library mutations and fixed them with three
//     stamp lines (docs/public/operate/auth/per-row-authz-audit.md). memql#4768
//     did the read-side twin: authSessionsForSubject(subject:) became
//     authSessionsForSelfIncludingRevoked() with no argument at all. Reach for
//     this FIRST -- it removes the enumeration surface instead of gating it.
//
//  2. IF THE OPERATION IS GENUINELY ABOUT SOMEBODY ELSE, put it in a
//     purpose-built package, allowlist THAT package, and assert its
//     precondition with a test of its own.
//
//     component/identity/adminops is the worked example: request-derived and
//     allowlisted, with the precondition -- every path downstream of the
//     owner/admin gate in the same function -- verified by
//     component/identity/adminops/gate_test.go, which drives every operation
//     with every role against an engine that refuses everything and counts the
//     attempts. The allowlist entry is not the safety property; that test is.
//
//     Note what makes this different from allowlisting component/grpc itself:
//     the package is small, exists for one operation family, and every call
//     site in it is downstream of one gate that a test can enumerate. None of
//     those is true of component/grpc.
//
//  3. NEVER: stamp in the handler, or move one call into an allowlisted
//     package purely so the stamp compiles there. The second is the first with
//     an extra hop, and it defeats the "a reviewer sees it" property the
//     per-package allowlist is built on.
//
// ContextWithInternalOrigin marks ctx as trusted server-side Go.
//
// Call this ONLY from Go that is itself the caller of a @serverOnly construct,
// on a context that code controls. Never call it in a request handler on a
// context derived from an inbound request: that would launder client origin
// into internal for everything downstream.
//
// THAT RULE IS ENFORCED, not merely stated (memql#2889).
// TestOnlyAllowlistedPackagesStampInternalOrigin at the repo root parses every
// git-tracked non-test .go file, resolves import aliases, and fails if this
// function is called from a package outside a small allowlist -- and
// unconditionally if it is called from component/grpc or component/node, where
// every context descends from an inbound request. Adding a call in a new
// package is a deliberate act that a reviewer sees, rather than a line nobody
// notices.
//
// Prefer keeping the stamp INLINE as the argument to the one Execute that
// needs it, as almost every caller here does. Then the marked context dies at
// that call and its blast radius is one query. Binding it to a variable that
// flows on is what made memql#2879's hole reachable: a trusted frame stamped
// internal, and a later frame in the same tree ran caller-submitted source on
// the inherited context.
func ContextWithInternalOrigin(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callOriginKey, OriginInternal)
}

// ContextWithClientOrigin marks ctx as an untrusted client call.
//
// Stamping is not strictly required -- OriginClient is the zero value, so an
// unstamped context is already treated as client. It exists so the wire entry
// points can say so explicitly, which makes the intent greppable and lets a
// test assert that the client path never carries OriginInternal.
func ContextWithClientOrigin(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callOriginKey, OriginClient)
}

// OriginFromContext returns the call origin, defaulting to OriginClient for an
// unstamped or nil context.
func OriginFromContext(ctx context.Context) CallOrigin {
	if ctx == nil {
		return OriginClient
	}
	if o, ok := ctx.Value(callOriginKey).(CallOrigin); ok {
		return o
	}
	return OriginClient
}
