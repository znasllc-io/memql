package memql

import "context"

// fresh_read.go -- a read that must see what another replica committed a
// moment ago (memql#5431).
//
// Every node runs its OWN result cache, and a write evicts a sibling's cached
// reads through the cache.invalidate.* broadcast: fast, but asynchronous. For
// almost every read that is the right trade -- the broadcast and the TTL bound
// the staleness, and the cache buys a great deal.
//
// It is the wrong trade for one shape of caller: a READ-MODIFY-WRITE that
// another replica may have performed a moment ago. A caller that holds a
// cross-replica lock, reads a value and writes one derived from it has to read
// what the previous holder wrote. The previous holder released the lock the
// moment its write committed, which is before any broadcast can have reached
// this node -- so a read served from this node's cache hands back the value
// the previous holder already advanced, and the write that follows erases the
// previous holder's work. That is the lost increment memql#5431 measured.
//
// So freshness is a property of the CALL SITE, not of the query. The same
// named query can be exactly right cached elsewhere (emailRuleById is read once
// per recipient on the campaign send path), and a @cache(0) on it would pay a
// database round trip on every one of those reads to protect one caller.
// ContextWithFreshRead marks that one call instead: its reads are answered from
// the database, and they neither consult nor fill this node's result cache.

type freshReadKey struct{}

// ContextWithFreshRead marks every read made under ctx to be answered from the
// database, never from this node's result cache. Keep the marked context a
// local of the one call that needs it: each read under it costs a database
// round trip, and a context returned up the stack would quietly turn the cache
// off for every read after it.
func ContextWithFreshRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshReadKey{}, true)
}

// FreshReadFromContext reports whether ctx was marked by ContextWithFreshRead.
//
// Exported for the in-process callers' tests, the CursorFromContext precedent:
// a caller that promises a fresh read is tested against a fake engine, and a
// fake that cannot see the mark cannot tell the promise from its absence.
func FreshReadFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	fresh, _ := ctx.Value(freshReadKey{}).(bool)
	return fresh
}
