package auth

import (
	"context"
	"strings"
	"unicode"
)

// THE CALL'S ACCOUNT (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md, section C).
//
// Every call into the engine acts in at most one account, and a row the call
// writes records that account when nothing nearer says which it is: its parent
// row, or an account the write itself names. Most calls never state it. The
// engine works it out from the actor (component/memql's CallAccount): a
// cluster operator acts in the primary account, a person in the account they
// belong to, and the cluster acting for itself in none.
//
// The key below is for the entry points that KNOW the account and whose actor
// cannot say it:
//
//   - an automation a row triggered acts in that row's account, and it runs as
//     the cluster, which belongs to none;
//   - a shopper form and an access-code visit act in the site's account, under
//     the site owner's borrowed authority, and the owner's own account can be a
//     different one (an operator who owns a client's site, a person in several
//     accounts);
//   - a forwarded request acts in the account the node it came from stated, so
//     the node that handles it does not work one out again (BindForwardedContext).
//
// # Why a context key and not an AccessContext field
//
// The account is a fact about the CALL, and the AccessContext is rebuilt many
// times beneath one call: ContextWithUserActor, a system or connector actor, a
// restored owner (ContextWithPersistedOwner), the work journal's own actor.
// About a hundred call sites do one of those, and each builds a fresh struct. A
// field on it would be dropped at every one, silently, and the row written
// underneath would record no account with nothing to say why. A key on the
// context survives all of them, because none of them knows it is there.
//
// # What it is not
//
// IT IS NOT AN AUTHORIZATION INPUT, and it must not become one. It records which
// account a row belongs to and opens that row to nobody: who may read or write a
// row is the concept's @rowAuthz declaration, decided from the actor. Nothing in
// this package reads the key to admit or refuse anything, VerifyForwardedAuthority
// does not look at the field that carries it across a hop, and it is deliberately
// absent from the DSL actor envelope, so no filter or spec can branch on it.
//
// # Who may state it
//
// Server-side Go that holds the fact: a stored row (the triggering row of an
// automation, the site a request arrived on), or an assertion a peer node made
// and this node verified. NEVER a value a request supplied. An account a caller
// names is an explicit choice on the write, and the engine holds that choice to
// the accounts the caller may write to. This key is taken as stated, so only a
// fact the server already holds belongs in it.

// callAccountKey is its own key rather than a field on any struct already on the
// context, for the reason in the file note.
const callAccountKey contextKey = "callAccount"

// ContextWithCallAccount states the account the call on ctx acts in.
//
// The value is stored as the BARE id (`acme`, never `v1:accounts:account:acme`),
// whichever spelling it arrives in. A stored `accountId` is canonical on most
// concepts and bare on a few, a client speaks bare, and every reader of this key
// compares or writes it as one spelling; normalizing once, here, is what keeps
// two spellings of one account from being two accounts after a hop.
//
// AN EMPTY ID RETURNS ctx UNCHANGED. An entry point that could not learn the
// account has nothing to state, and stating nothing must not erase what an outer
// frame stated: a sub-automation inherits its caller's account, and a triggering
// row that has no account yet leaves the run where it was. There is therefore no
// way to clear the key, on purpose. The innermost statement wins.
func ContextWithCallAccount(ctx context.Context, accountId string) context.Context {
	bare := bareAccountId(accountId)
	if bare == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, callAccountKey, bare)
}

// CallAccountFromContext returns the account an entry point stated for this call,
// as the bare id, and false when none did.
//
// FALSE IS NOT "NO ACCOUNT". It means nobody stated one, and the engine then
// resolves it from the actor. Code that needs the account a call acts in asks
// component/memql's CallAccount, which reads this first; reading the key alone
// answers a narrower question, and is right only for a producer about to carry
// it somewhere else (ForwardedAuthorityFromContext).
func CallAccountFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	account, ok := ctx.Value(callAccountKey).(string)
	return account, ok && account != ""
}

// WithCallAccount returns the assertion carrying the account the call on ctx
// acts in, for a producer about to put it on the wire.
//
// Only a STATED account is carried. A context that names none leaves the field
// as it was, and the receiving node resolves the account from the actor the
// assertion carries, by the same rules over the same database this node would
// have used. Resolving it here instead would need the engine, which this
// package cannot reach.
//
// A value receiver, like WithDisplayName: a caller that drops the return value
// sends the assertion unchanged rather than mutating one another frame holds.
func (a ForwardedAuthority) WithCallAccount(ctx context.Context) ForwardedAuthority {
	if account, ok := CallAccountFromContext(ctx); ok {
		a.AccountId = account
	}
	return a
}

// bareAccountId strips the `{concept}:` prefix of a canonical id and returns the
// short id: `v1:accounts:account:acme` is `acme`.
//
// IT IS component/memql's BareShortId, WRITTEN OUT A SECOND TIME, and both
// obvious ways of not doing that are closed. This package cannot import
// component/memql, which imports it. And the primitive BareShortId stands on,
// core/id's ParseNodeId, lives in a package that also mints ids and so imports a
// UUID library: this module has no third-party dependency at all, and taking one
// on to split a string at its colons would be a poor trade. So the rule is
// restated here in full -- a concept is a version segment (`v1`, `v2`, ...) and
// the two segments after it, and the short id is whatever follows -- and
// TestTheCallAccountKeyAgreesWithBareShortId in component/memql holds the two
// to one answer, spelling by spelling.
//
// TOTAL, like the original: a value with no concept prefix is already bare and
// is returned as it is, and one that cannot be decomposed is returned unchanged
// rather than destroyed.
func bareAccountId(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ":")
	for i, part := range parts {
		if !isConceptVersionSegment(part) {
			continue
		}
		if end := i + 3; end < len(parts) {
			if short := strings.Join(parts[end:], ":"); short != "" {
				return short
			}
		}
		// A version segment with no short id after its concept: a bare concept
		// type, or a malformed id. Returned as it came.
		return value
	}
	return value
}

// isConceptVersionSegment reports whether a segment opens a concept id: `v`
// followed by digits and nothing else.
func isConceptVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
