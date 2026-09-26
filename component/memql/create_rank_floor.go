package memql

import "context"

// CREATE RANK FLOORS: the create half of a cluster-owner tier, for the two
// concepts Connect Shopify leans on (design record, D13).
//
// A @rowAuthz(clusterOwner) concept's write guard judges the STORED row, and a
// create has none, so the guard answers nil (rowauthz_write_guard.go says so in
// as many words: "NOT creates"). For these two concepts that left the create
// open to every signed-in caller: anyone could register a Shopify store, or
// enable a pack nobody had flipped yet -- and enabling `wholesale` publishes a
// shopper write endpoint.
//
// The floor is OWNER, the tier's own answer. The design first set it at
// developer, and review found two exploits in a developer authoring these rows
// directly: a store row whose token reference names a secret it does not own,
// which the edge then publishes to every visitor, and a pack row under a fresh
// id that overrides the pack's switch. No developer journey needs a direct
// create: Connect Shopify writes the store, and the Modules flip writes the
// pack, both as server code.
//
// WHY AT THE WRITE SEAM AND NOT @requiresRank ON THE MUTATIONS. The raw
// insert() literal is public language and names no construct, so a floor
// declared on createStore never sees `insert("v1:shopify:store", ...)`.
// executeWrite is where the named mutation and the raw literal converge --
// the reason the write guard lives there too -- so one check here covers
// both, and a second one on the mutations would only be a second place for
// the rule to drift.
//
// The rank check itself is refuseBelowRequiredRank's, called the way the plan
// gate calls it: internal origin passes (server code stamped for one call is
// not a principal, so the Shopify seed, the connector, Connect Shopify and the
// Modules flip are unaffected), and a caller or floor that does not resolve on
// the ladder fails closed. The same hole on every other cluster-owner concept
// is memql#5624.
var conceptCreateRankFloors = map[string]string{
	"v1:shopify:store":      "owner",
	"v1:platform:packState": "owner",
}

// refuseCreateBelowRankFloor refuses a CREATE on a concept listed in
// conceptCreateRankFloors when the caller ranks below its floor. The caller
// decides that the write is a create; a write onto an existing row is the
// write guard's to judge.
func (e *MemQLEngine) refuseCreateBelowRankFloor(ctx context.Context, conceptName string) error {
	floor, ok := conceptCreateRankFloors[conceptName]
	if !ok {
		return nil
	}
	return e.refuseBelowRequiredRank(ctx, &Function{RequiresRank: floor}, "creating a "+conceptName+" row")
}
