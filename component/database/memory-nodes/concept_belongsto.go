package memoryNodes

import "github.com/znasllc-io/memql/component/language/parser"

// The account-ownership accessors (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md).
//
// The Concept struct stores what the concept DECLARED (BelongsTo); every
// consumer that needs to know what the declaration MEANS comes through here, so
// "whose rows are these" is derived in one place (parser.BelongsToDecl) whoever
// asks. That is the reason concept_origin.go gives for the data-origins
// accessors, and it applies with more force here: the write path's stamp, the
// assignment of existing rows, the account inventory and, later, export and
// sign-in separation all ask the same four questions, and two of them reading
// the declaration differently would file one row under two accounts.
//
// EVERY ACCESSOR ANSWERS NO FOR AN UNDECLARED CONCEPT, ClusterWide included.
// Undeclared is not cluster-wide: it is a concept nobody has classified yet.
// The engine treats its rows as it always did (nothing is stamped, nothing is
// counted for an account), which happens to be what it does for a cluster-wide
// concept too -- but a caller that wants "treated as cluster-wide" asks
// !AccountScoped(), and a caller that wants "was declared cluster-wide" asks
// ClusterWide(). Collapsing the two would make an unclassified bundle concept
// indistinguishable from one its author deliberately kept out of every account,
// which is the distinction the load-time report of undeclared concepts exists
// to keep.
//
// All four are nil-safe on the *Concept, because the write path asks them of
// whatever the registry handed back.

// AccountScoped reports whether this concept's rows belong to accounts: it
// declared an account form of @belongsTo, of any kind. It is the question the
// READERS of the stamp ask (the inventory, export): is `accountId` on a row of
// this concept the row's account at all.
func (c *Concept) AccountScoped() bool {
	return c != nil && c.BelongsTo.AccountScoped()
}

// ClusterWide reports whether this concept DECLARED `@belongsTo(cluster)`: its
// rows are never inside an account. False for an undeclared concept -- see the
// file comment.
func (c *Concept) ClusterWide() bool {
	return c != nil && c.BelongsTo != nil && c.BelongsTo.Scope == parser.BelongsToCluster
}

// SharedRows reports whether this concept declared `@belongsTo(account,
// shared)`: beside its accounts' rows it holds rows that belong to no account
// and are provided by the cluster to every account (a seeded skill beside one a
// person wrote). On such a concept a row with no account is a statement, not a
// gap, so nothing may fill it in with a fallback.
func (c *Concept) SharedRows() bool {
	return c != nil && c.BelongsTo.AccountScoped() && c.BelongsTo.Shared
}

// AccountEngineManaged reports whether the ENGINE decides which account a row
// of this concept belongs to: the concept is account-scoped and did not declare
// `stated`. It is the one question the WRITERS of the stamp ask -- the write
// path's fill and the assignment of existing rows -- and it is narrower than
// AccountScoped on purpose: a `stated` concept's own mutations name the
// account, under rules that predate the declaration, and the engine neither
// fills the field there nor refuses a change to it.
func (c *Concept) AccountEngineManaged() bool {
	return c != nil && c.BelongsTo.EngineManaged()
}
