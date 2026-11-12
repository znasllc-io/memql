package auth

import (
	"context"
	"strings"
)

// pack_caller.go -- WHICH PACK IS MAKING THIS CALL (ruling 57).
//
// Internal origin says the call is trusted server-side Go, and the automation
// executor stamps it on every step of every registered automation, a client
// package's deployed domain included. So internal origin alone cannot say
// "this is the wholesale pack's call": an automation of any package could name
// a builtin that trusts internal origin and pass it arguments it chose.
//
// The pack caller is a second mark, beside the origin: the name of the pack
// whose Go is making this one call. Nothing in DSL can set a context value, so
// an automation's own step never carries it. It is NOT AN ORIGIN and grants
// nothing by itself: a reader that trusts it also requires internal origin.
//
// WHO MAY SET IT IS GUARDED as who may stamp internal origin is:
// TestOnlyAllowlistedPackagesMarkAPackCaller at the repo root fails when
// ContextWithPackCaller is called from a package outside its allowlist, and
// when PackCallerFromContext is read anywhere but the one gate that trusts it.
// Set it INLINE on the one Execute that needs it, so the mark dies with that
// call.

type packCallerKey struct{}

// ContextWithPackCaller marks ctx as the named pack's own call.
func ContextWithPackCaller(ctx context.Context, pack string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, packCallerKey{}, strings.TrimSpace(pack))
}

// PackCallerFromContext names the pack that marked ctx, or "" when none did.
func PackCallerFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	pack, _ := ctx.Value(packCallerKey{}).(string)
	return pack
}
