package packages

import "github.com/znasllc-io/memql/component/memql"

// sameShortId answers whether two spellings name the same row (memql#5291).
//
// A stored relationship field is CANONICAL -- `packageId` on
// v1:platform:packageDeployment is an outgoing relationship, so the engine
// canonicalizes it to `v1:platform:package:<id>` on write and an in-process
// read hands it back that way. A request carries the BARE id: the engine
// bare-ifies every id on egress, a client never composes the canonical form
// (docs/public/concepts/identifiers.md), and a builtin's arguments are not
// canonicalized on the way in. The read path already resolves a bare id in
// SQL, so a compare is the only place the two spellings meet -- and every
// site that compares a stored id against a request id goes through here.
//
// Both sides go through memql.BareShortId, which is TOTAL: a value it cannot
// decompose comes back unchanged, so a bare id compares as itself. It is not
// idempotent in general (see its header), which is fine here because a
// package id is a single canonical prefix over an opaque short id.
//
// An empty value on either side is a match for nothing: a blank request
// against a blank row is not evidence that they are the same package.
func sameShortId(a, b string) bool {
	a, b = memql.BareShortId(a), memql.BareShortId(b)
	return a != "" && a == b
}
