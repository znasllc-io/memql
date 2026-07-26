package auth

import (
	"context"
	"testing"
)

// TestUnstampedContextIsClient is the safety property the whole gate rests on
// (memql#2800). If an unstamped context ever reported internal, every caller
// that has not been taught about origin -- including any client entry point
// added later -- would silently gain access to @serverOnly constructs.
//
// The three annotations this replaces all failed in exactly that direction:
// @internal hid constructs from discovery while leaving them callable
// (#2620 / #2708), and @role / @permission were retired as "documented but
// never enforced". A default of "trusted" is how a marker becomes decorative.
func TestUnstampedContextIsClient(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"nil":                  nil,
		"background":           context.Background(),
		"todo":                 context.TODO(),
		"with unrelated value": context.WithValue(context.Background(), contextKey("unrelated"), "x"),
		"with access context": ContextWithAccess(context.Background(), &AccessContext{
			UserId: "v1:identity:user:someone", Role: RoleOwner,
		}),
	} {
		t.Run(name, func(t *testing.T) {
			if got := OriginFromContext(ctx); got != OriginClient {
				t.Errorf("OriginFromContext(%s) = %v, want %v -- an unstamped "+
					"context MUST be untrusted", name, got, OriginClient)
			}
			if OriginFromContext(ctx).IsInternal() {
				t.Errorf("%s: IsInternal() = true, want false", name)
			}
		})
	}
}

// TestAuthenticationDoesNotImplyInternal pins the distinction that makes this
// a channel fact rather than a permission. A cluster owner calling over the
// wire is fully authenticated and maximally privileged, and still must not
// reach a @serverOnly construct -- otherwise the gate is just another role
// check, and any path that can obtain the role can bypass it.
func TestAuthenticationDoesNotImplyInternal(t *testing.T) {
	ctx := ContextWithAccess(context.Background(), &AccessContext{
		UserId:     "v1:identity:user:owner",
		Role:       RoleOwner,
		IdentityId: "v1:identity:identity:abc",
	})
	if OriginFromContext(ctx).IsInternal() {
		t.Fatal("a cluster owner's authenticated context reported internal origin; " +
			"origin is which channel the call arrived on, not how privileged the caller is")
	}
	// And the reverse: internal origin carries no authorization by itself.
	internal := ContextWithInternalOrigin(context.Background())
	if ac, ok := AccessFromContext(internal); ok && ac != nil {
		t.Errorf("ContextWithInternalOrigin fabricated an AccessContext (%+v); "+
			"it must only mark origin", ac)
	}
}

// TestInternalOriginRoundTrips covers the positive path and the explicit
// client stamp.
func TestInternalOriginRoundTrips(t *testing.T) {
	internal := ContextWithInternalOrigin(context.Background())
	if got := OriginFromContext(internal); got != OriginInternal {
		t.Errorf("OriginFromContext(internal) = %v, want %v", got, OriginInternal)
	}
	if !OriginFromContext(internal).IsInternal() {
		t.Error("IsInternal() = false on an internally-stamped context")
	}

	// An explicit client stamp must not be readable as internal, and must
	// override an inherited internal stamp -- otherwise a handler deriving a
	// request context from an internal parent would launder the origin.
	client := ContextWithClientOrigin(internal)
	if OriginFromContext(client).IsInternal() {
		t.Error("an explicit client stamp did not override an inherited internal stamp; " +
			"origin would leak from a server-side parent into a request context")
	}

	// ContextWithInternalOrigin(nil) must not panic and must still mark.
	if !OriginFromContext(ContextWithInternalOrigin(nil)).IsInternal() {
		t.Error("ContextWithInternalOrigin(nil) did not mark the context")
	}
}

// TestOriginStringsAreStable guards the strings that appear in the rejection
// error a caller will see.
func TestOriginStringsAreStable(t *testing.T) {
	if OriginClient.String() != "client" {
		t.Errorf("OriginClient.String() = %q, want %q", OriginClient.String(), "client")
	}
	if OriginInternal.String() != "internal" {
		t.Errorf("OriginInternal.String() = %q, want %q", OriginInternal.String(), "internal")
	}
	// An out-of-range value must read as the SAFE label, not as internal.
	if CallOrigin(99).String() == "internal" || CallOrigin(99).IsInternal() {
		t.Error("an unknown CallOrigin reported internal; unknown must fail closed")
	}
}
