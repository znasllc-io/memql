package memql

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// forwarded_call_account_test.go -- the call's account on the AI forward
// (design record docs/superpowers/specs/2026-11-12-account-row-ownership-design.md,
// section C).
//
// forwardedPrincipal builds its assertion from the SESSION rather than
// re-asserting one off the context, so it is the one producer that
// auth.ForwardedAuthorityFromContext does not cover: an account the session's
// context states has to be read here or it does not leave this node. That
// matters on the second hop. A worker handling a forward runs a session over the
// bound context (HandleForwardedRequest), and when that session forwards again
// -- a transcription continuation, a deploy control call -- the far node must
// still record the account the first node stated.
//
// It drives the production producer and receiver halves over the same two
// adapters the router and the handler use, like the display-name parity test
// beside it.

// callAccountSession is a session over a stream whose context is ctx.
func callAccountSession(ctx context.Context) *streamSession {
	return &streamSession{
		stream:   &captureStream{ctx: ctx},
		identity: auth.UserIdentity{Subject: "v1:identity:user:alice", Email: "alice@example.com", Role: "writer"},
		access: &auth.AccessContext{
			UserId:       "v1:identity:user:alice",
			PrimaryEmail: "alice@example.com",
			Role:         auth.RoleWriter,
		},
		accessLoaded:    true,
		credentialClass: auth.ForwardedClassUser,
		closeChan:       make(chan struct{}),
	}
}

// callAccountForwardedContext sends a principal over the hop and returns the
// context the receiving node's handlers run on. context.Background(): the receiver holds
// nothing of the originating call but what the wire carried.
func callAccountForwardedContext(t *testing.T, principal auth.ForwardedPrincipal) context.Context {
	t.Helper()
	received := forwardedAuthorityFromProto(forwardedAuthorityToProto(principal.Authority, "agent-1", "agent"))
	access, err := auth.VerifyForwardedAuthority(received, time.Now())
	if err != nil {
		t.Fatalf("the receiver refused a well-formed assertion: %v", err)
	}
	return bindForwardedContext(context.Background(), principal.Claims, access, received)
}

func TestASessionForwardCarriesTheAccountItsContextStates(t *testing.T) {
	// The session of a worker that is itself handling a forward: its stream
	// context is the one BindForwardedContext produced, account included.
	sess := callAccountSession(auth.ContextWithCallAccount(context.Background(), "v1:accounts:account:acme"))

	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal: %v", err)
	}
	if principal.Authority.AccountId != "acme" {
		t.Fatalf("the session's assertion names account %q, want acme.\n\n"+
			"forwardedPrincipal builds its assertion from the session, not from the context, so an "+
			"account the context states is dropped unless it is read here. The node handling this "+
			"forward would then resolve an account from the actor instead of recording the one the "+
			"originating node stated.", principal.Authority.AccountId)
	}

	remote := callAccountForwardedContext(t, principal)
	if got, ok := auth.CallAccountFromContext(remote); !ok || got != "acme" {
		t.Errorf("the receiving node bound (%q, %v), want acme", got, ok)
	}
	// The actor is unchanged by it: the account is not part of who is calling.
	if ac, ok := auth.AccessFromContext(remote); !ok || ac.UserId != "v1:identity:user:alice" || ac.Role != auth.RoleWriter {
		t.Errorf("the bound actor = %+v, want alice as a writer", ac)
	}
	// Nor does it reach the attribution claims, which are what createdBy reads.
	if _, carried := principal.Claims["accountId"]; carried {
		t.Error("the account reached the attribution claims")
	}
}

// A session whose context states no account sends none, and the receiver binds
// none: a person's own session is resolved from the actor on whichever node
// handles it.
func TestASessionForwardWithNoStatedAccountSendsNone(t *testing.T) {
	sess := callAccountSession(context.Background())

	principal, err := sess.forwardedPrincipal()
	if err != nil {
		t.Fatalf("forwardedPrincipal: %v", err)
	}
	if principal.Authority.AccountId != "" {
		t.Fatalf("a session with no stated account asserted %q", principal.Authority.AccountId)
	}
	if got, ok := auth.CallAccountFromContext(callAccountForwardedContext(t, principal)); ok {
		t.Errorf("the receiving node bound %q from an assertion that named no account", got)
	}
}
