package auth

import (
	"context"
	"testing"
	"time"
)

// call_account_test.go -- the call's account as a context key (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C).
//
// Three properties, and each has a way of going wrong that nothing else would
// notice: the key has to SURVIVE the places that rebuild the actor (it is a key
// rather than an AccessContext field for exactly that reason), it has to hold
// ONE spelling of an account whatever a caller handed it, and it has to cross a
// mesh hop without becoming something the receiver decides access by.

// An unstamped context acts in no account that anybody stated. That is the
// answer the engine resolves from (component/memql's CallAccount works it out
// from the actor), so "no key" must never read as an account.
func TestAnUnstampedContextNamesNoCallAccount(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"nil":        nil,
		"background": context.Background(),
		"with an operator": ContextWithAccess(context.Background(), &AccessContext{
			UserId: "v1:identity:user:owner", Role: RoleOwner,
		}),
		"with a borrowed person":  ContextWithUserActor(context.Background(), "v1:identity:user:alice"),
		"with an unrelated value": context.WithValue(context.Background(), contextKey("unrelated"), "acme"),
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := CallAccountFromContext(ctx); ok || got != "" {
				t.Fatalf("CallAccountFromContext = (%q, %v), want no account", got, ok)
			}
		})
	}
}

// The value is the BARE id, whatever was handed in. Every reader of an account
// compares bare ids, and the two spellings of one account must not be two
// accounts on the far side of a hop.
func TestTheCallAccountIsStoredBare(t *testing.T) {
	for given, want := range map[string]string{
		"acme":                     "acme",
		"  acme\t":                 "acme",
		"v1:accounts:account:acme": "acme",
		"v1:accounts:account:self": "self",
		"self":                     "self",
		// The identity file's own `account` concept is a different concept
		// with the same short id; a field canonicalized against it still
		// names the same account.
		"v1:identity:account:acme": "acme",
		"acct-0f3a":                "acct-0f3a",
	} {
		got, ok := CallAccountFromContext(ContextWithCallAccount(context.Background(), given))
		if !ok || got != want {
			t.Errorf("ContextWithCallAccount(%q) stored (%q, %v), want %q", given, got, ok, want)
		}
	}
}

// An empty id states nothing, so it must change nothing: not clear an account an
// outer frame stated, and not mark the context as carrying one.
func TestAnEmptyCallAccountLeavesTheContextAlone(t *testing.T) {
	for _, empty := range []string{"", "   ", "\t\n"} {
		base := context.Background()
		if got := ContextWithCallAccount(base, empty); got != base {
			t.Errorf("ContextWithCallAccount(%q) returned a new context; an empty id must return ctx unchanged", empty)
		}
		outer := ContextWithCallAccount(base, "acme")
		inner := ContextWithCallAccount(outer, empty)
		if inner != outer {
			t.Errorf("ContextWithCallAccount(%q) over a stamped context returned a new context", empty)
		}
		if got, ok := CallAccountFromContext(inner); !ok || got != "acme" {
			t.Errorf("an empty id cleared the outer account: got (%q, %v)", got, ok)
		}
	}
	if got := ContextWithCallAccount(nil, ""); got != nil {
		t.Error("ContextWithCallAccount(nil, \"\") invented a context")
	}
	// A nil context with something to say must not panic and must still say it.
	if got, ok := CallAccountFromContext(ContextWithCallAccount(nil, "acme")); !ok || got != "acme" {
		t.Errorf("ContextWithCallAccount(nil, acme) stored (%q, %v)", got, ok)
	}
}

// The innermost statement wins: a run that a row of another account triggers
// acts in that row's account, whatever the frame around it was acting in.
func TestTheInnermostCallAccountWins(t *testing.T) {
	ctx := ContextWithCallAccount(context.Background(), "acme")
	ctx = ContextWithCallAccount(ctx, "v1:accounts:account:beta")
	if got, _ := CallAccountFromContext(ctx); got != "beta" {
		t.Fatalf("call account = %q, want the inner one (beta)", got)
	}
}

// THE REASON IT IS A KEY. About a hundred call sites rebuild the actor on the way
// down -- borrowed authority, a system actor, a restored owner, an origin stamp
// -- and each builds a fresh AccessContext. A field on that struct would be
// dropped at every one of them, silently, and the row written beneath would
// record no account. Each rebuild below is one of those sites' own helper.
func TestTheCallAccountSurvivesEveryRebuildOfTheActor(t *testing.T) {
	stamped := ContextWithCallAccount(context.Background(), "acme")

	persisted, err := ContextWithPersistedOwner(stamped, "v1:identity:user:alice", nil, nil)
	if err != nil {
		t.Fatalf("ContextWithPersistedOwner: %v", err)
	}
	system, err := ForwardedAuthorityForSystem("pipelines-executor:node-1", time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForSystem: %v", err)
	}
	systemAccess, err := VerifyForwardedAuthority(system, time.Now())
	if err != nil {
		t.Fatalf("VerifyForwardedAuthority: %v", err)
	}

	for name, ctx := range map[string]context.Context{
		"borrowed authority":        ContextWithUserActor(stamped, "u2"),
		"a replaced access context": ContextWithAccess(stamped, &AccessContext{UserId: "u3", Role: RoleReader}),
		"a system actor":            ContextWithSystemActor(stamped, "edge"),
		"a connector actor":         ContextWithConnectorActor(stamped, "shopify"),
		"the anonymous actor":       ContextWithAnonymousActor(stamped),
		"internal origin":           ContextWithInternalOrigin(stamped),
		"client origin":             ContextWithClientOrigin(stamped),
		"a restored owner":          persisted,
		"a blanked assertion":       ContextWithForwardedAuthority(stamped, ForwardedAuthority{}),
		"a bound system assertion":  BindForwardedContext(stamped, system.Principal().Claims, systemAccess, system),
	} {
		if got, ok := CallAccountFromContext(ctx); !ok || got != "acme" {
			t.Errorf("%s: call account = (%q, %v), want acme -- a rebuild of the actor dropped the account "+
				"the call acts in, and the row written beneath it would record none", name, got, ok)
		}
	}
}

// It states where a call acts and nothing about who is calling or how far they
// are trusted. A stamp that fabricated an actor or an origin would make "which
// account" and "may this caller" one bit.
func TestTheCallAccountIsNotAnActorOrAnOrigin(t *testing.T) {
	ctx := ContextWithCallAccount(context.Background(), "acme")
	if ac, ok := AccessFromContext(ctx); ok || ac != nil {
		t.Errorf("ContextWithCallAccount fabricated an AccessContext: %+v", ac)
	}
	if OriginFromContext(ctx).IsInternal() {
		t.Error("ContextWithCallAccount marked the context internal")
	}
	if _, ok := ForwardedAuthorityFromContext(ctx); ok {
		t.Error("ContextWithCallAccount bound a forwarded assertion")
	}
}

// --- across a hop -----------------------------------------------------------

func callAccountAuthority(t *testing.T) ForwardedAuthority {
	t.Helper()
	a, err := ForwardedAuthorityForUser(
		&AccessContext{UserId: "v1:identity:user:alice", PrimaryEmail: "alice@example.com", Role: RoleWriter},
		ForwardedClassUser, "", time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("ForwardedAuthorityForUser: %v", err)
	}
	return a
}

// The producer half: a node about to forward re-asserts what it accepted, and
// the account it carries is the one the call is acting in NOW -- stated by
// whichever frame stated it last, which may be after the assertion was bound.
func TestAReassertedAuthorityCarriesTheCallAccount(t *testing.T) {
	bound := callAccountAuthority(t)
	access, err := VerifyForwardedAuthority(bound, time.Now())
	if err != nil {
		t.Fatalf("VerifyForwardedAuthority: %v", err)
	}
	ctx := BindForwardedContext(context.Background(), bound.Principal().Claims, access, bound)

	if got, ok := ForwardedAuthorityFromContext(ctx); !ok || got.AccountId != "" {
		t.Fatalf("an assertion bound with no account re-asserts %q (ok=%v), want none", got.AccountId, ok)
	}

	// An automation's trigger, a shopper form or an access-code visit states
	// the account after the actor was bound.
	later := ContextWithCallAccount(ctx, "v1:accounts:account:acme")
	got, ok := ForwardedAuthorityFromContext(later)
	if !ok || got.AccountId != "acme" {
		t.Fatalf("re-asserted account = %q (ok=%v), want acme: the hop would leave the receiving node to "+
			"work the account out again", got.AccountId, ok)
	}
	// Nothing else about the assertion moves.
	got.AccountId = ""
	if got != bound {
		t.Errorf("filling the account changed another field:\n bound: %+v\n got:   %+v", bound, got)
	}

	// With no assertion bound there is nothing to re-assert, account or not.
	if _, ok := ForwardedAuthorityFromContext(ContextWithCallAccount(context.Background(), "acme")); ok {
		t.Error("a call account alone produced a forwarded assertion")
	}
}

// The receiver half: binding an assertion that names an account makes it the
// account every engine call on that node acts in, and an assertion that names
// none binds none.
func TestBindingAnAuthorityBindsItsCallAccount(t *testing.T) {
	withAccount := callAccountAuthority(t)
	withAccount.AccountId = "acme"
	access, err := VerifyForwardedAuthority(withAccount, time.Now())
	if err != nil {
		t.Fatalf("VerifyForwardedAuthority: %v", err)
	}
	ctx := BindForwardedContext(context.Background(), withAccount.Principal().Claims, access, withAccount)
	if got, ok := CallAccountFromContext(ctx); !ok || got != "acme" {
		t.Fatalf("bound call account = (%q, %v), want acme", got, ok)
	}
	// And a second hop out of this node carries it on.
	if again, ok := ForwardedAuthorityFromContext(ctx); !ok || again.AccountId != "acme" {
		t.Errorf("a second hop would carry %q (ok=%v), want acme", again.AccountId, ok)
	}

	none := callAccountAuthority(t)
	plain := BindForwardedContext(context.Background(), none.Principal().Claims, access, none)
	if got, ok := CallAccountFromContext(plain); ok {
		t.Errorf("an assertion with no account bound %q", got)
	}

	// A canonical spelling on the wire is bound bare, like every other entry.
	canonical := callAccountAuthority(t)
	canonical.AccountId = "v1:accounts:account:acme"
	spelled := BindForwardedContext(context.Background(), canonical.Principal().Claims, access, canonical)
	if got, _ := CallAccountFromContext(spelled); got != "acme" {
		t.Errorf("a canonical account on the wire was bound as %q, want acme", got)
	}
}

// THE GUARD RAIL. The account says which account a row this call writes is
// recorded in. It is not an input to who the caller is or what they may do, and
// the verifier must stay blind to it: the day it reads the field, "which
// account" has become part of "what this may do", decided by a value the
// sending node chose.
func TestTheVerifierIgnoresTheCallAccount(t *testing.T) {
	now := time.Now()
	without := callAccountAuthority(t)
	for _, account := range []string{"acme", "self", "v1:accounts:account:acme", "owner", "v1:identity:user:someone-else"} {
		with := without
		with.AccountId = account

		gotWith, errWith := VerifyForwardedAuthority(with, now)
		gotWithout, errWithout := VerifyForwardedAuthority(without, now)
		if errWith != nil || errWithout != nil {
			t.Fatalf("account %q: the verifier refused a well-formed assertion: with=%v without=%v", account, errWith, errWithout)
		}
		if *gotWith != *gotWithout {
			t.Errorf("account %q moved the bound AccessContext:\n with:    %+v\n without: %+v", account, *gotWith, *gotWithout)
		}
		if _, carried := with.Principal().Claims["accountId"]; carried {
			t.Errorf("account %q reached the attribution claims", account)
		}
	}

	// A system principal may act in an account too: an automation a row
	// triggered runs as the cluster and still acts in that row's account.
	system, err := ForwardedAuthorityForSystem("pipelines-executor:node-1", now)
	if err != nil {
		t.Fatalf("ForwardedAuthorityForSystem: %v", err)
	}
	system.AccountId = "acme"
	if _, err := VerifyForwardedAuthority(system, now); err != nil {
		t.Errorf("a system assertion carrying an account was refused: %v", err)
	}
}
