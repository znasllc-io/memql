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

// ContextWithInternalOrigin marks ctx as trusted server-side Go.
//
// Call this ONLY from Go that is itself the caller of a @serverOnly construct,
// on a context that code controls. Never call it in a request handler on a
// context derived from an inbound request: that would launder client origin
// into internal for everything downstream.
//
// internal_origin_callers_test.go allowlists the packages that may reference
// this and fails on any other, so a new caller in a new package is a red test
// rather than a silent privilege grant (memql#2889). Adding yourself to that
// list is a security decision -- the bar is the paragraph above.
//
// Note the limit, stated so the guarantee is not over-read: the allowlist is
// per-PACKAGE. A new call added inside a package that is already listed does not
// fail it, and component/memql and app are large. Read that test's header for
// exactly what it does and does not cover.
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
