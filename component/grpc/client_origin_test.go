package memql

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/znasllc-io/memql/component/auth"
)

// Tests for the wire-boundary client-origin stamp (memql#2889).
//
// Before this, the stamp lived in one handler and NOTHING asserted it. The
// three tests that executed that line built a service with a nil engine, so
// executeQuery returned Unavailable before anything could observe the context:
// the line was covered and entirely unasserted, and deleting it broke no test
// in the repo. These are the tests that make deleting it fail.

// fakeStream is a grpc.ServerStream that carries a context of our choosing.
// Only Context() is exercised; the rest of the interface is never called.
type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeStream) Context() context.Context { return f.ctx }

// passthroughInterceptor stands in for whichever auth chain app/transport.go
// installs. It does what those chains do -- derive a context and hand the
// handler a stream carrying it -- without needing any of their machinery.
func passthroughInterceptor(derive func(context.Context) context.Context) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &fakeStream{ServerStream: ss, ctx: derive(ss.Context())})
	}
}

// TestWithClientOriginStampsTheWireBoundary: every handler beneath the
// interceptor sees a context explicitly marked client-origin.
func TestWithClientOriginStampsTheWireBoundary(t *testing.T) {
	var got auth.CallOrigin
	wrapped := withClientOrigin(passthroughInterceptor(func(ctx context.Context) context.Context { return ctx }))

	err := wrapped(nil, &fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{},
		func(_ any, ss grpc.ServerStream) error {
			got = auth.OriginFromContext(ss.Context())
			return nil
		})
	if err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	if got.IsInternal() {
		t.Error("a handler on the wire path saw INTERNAL origin; the stamp is not being applied")
	}
}

// TestWithClientOriginOverridesAnInheritedInternalMark is the one that matters.
//
// OriginClient is the zero value, so the test above would pass even with no
// stamp at all -- an unstamped context is already untrusted. The property the
// stamp actually provides is OVERRIDE: an inherited internal mark must not
// survive the wire boundary.
//
// Nothing on this path can supply such a mark today (component/grpc and
// component/node contain no ContextWithInternalOrigin, enforced by
// TestOnlyAllowlistedPackagesStampInternalOrigin). This drives the case
// directly so the guarantee holds the day that stops being true -- which is
// exactly how memql#2879's live hole arose one package over.
func TestWithClientOriginOverridesAnInheritedInternalMark(t *testing.T) {
	var got auth.CallOrigin
	wrapped := withClientOrigin(passthroughInterceptor(func(ctx context.Context) context.Context { return ctx }))

	// A stream whose context descends from server-side Go.
	poisoned := auth.ContextWithInternalOrigin(context.Background())
	if !auth.OriginFromContext(poisoned).IsInternal() {
		t.Fatal("fixture is wrong: the parent context is not internal, so this test would pass vacuously")
	}

	err := wrapped(nil, &fakeStream{ctx: poisoned}, &grpc.StreamServerInfo{},
		func(_ any, ss grpc.ServerStream) error {
			got = auth.OriginFromContext(ss.Context())
			return nil
		})
	if err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	if got.IsInternal() {
		t.Error("an inherited INTERNAL origin survived the wire boundary.\n" +
			"Internal origin is the only thing that opens the @serverOnly gate, so this would let a " +
			"wire caller read userByIdSystem / userByEmail / activeUsers -- full directory PII for any " +
			"user they name, and enumeration of the whole active-user table.")
	}
}

// TestWithClientOriginStampsAfterTheAuthChain: the stamp must land on the
// context the auth chain produced, not on one the chain then replaces.
//
// Wrapping in the other order -- stamping ss.Context() before calling next --
// looks equivalent and is not: an interceptor that derives its own context
// from the stream would discard the stamp entirely, and every test above would
// still pass because the zero value is client.
func TestWithClientOriginStampsAfterTheAuthChain(t *testing.T) {
	type key struct{}
	var (
		sawClaim any
		got      auth.CallOrigin
	)

	// A chain that both adds a value AND poisons the origin, so this test
	// fails if the stamp is applied before the chain rather than after.
	chain := passthroughInterceptor(func(ctx context.Context) context.Context {
		return auth.ContextWithInternalOrigin(context.WithValue(ctx, key{}, "claims"))
	})

	err := withClientOrigin(chain)(nil, &fakeStream{ctx: context.Background()}, &grpc.StreamServerInfo{},
		func(_ any, ss grpc.ServerStream) error {
			sawClaim = ss.Context().Value(key{})
			got = auth.OriginFromContext(ss.Context())
			return nil
		})
	if err != nil {
		t.Fatalf("interceptor returned %v", err)
	}
	if sawClaim != "claims" {
		t.Errorf("the auth chain's context did not reach the handler (got %v); the stamp must wrap the handler, not replace the chain's context", sawClaim)
	}
	if got.IsInternal() {
		t.Error("the stamp landed BEFORE the auth chain, so the chain's context overwrote it")
	}
}

// TestWithClientOriginPassesNilThrough: Server.Start refuses to run without an
// interceptor, so nil here means a caller wired something wrong. Returning a
// non-nil wrapper around nil would turn that into a nil-deref at the first RPC
// instead of the existing explicit error at startup.
func TestWithClientOriginPassesNilThrough(t *testing.T) {
	if withClientOrigin(nil) != nil {
		t.Error("withClientOrigin(nil) must stay nil so Server.Start's 'authentication interceptor not configured' check still fires")
	}
}
