package memoryNodes

import (
	"context"

	"github.com/uptrace/bun"
)

// EnableAccountStampingWhenReady is the handshake for the switch that lets the
// engine record the account of a row: fill accountId on a write to a concept
// declared @belongsTo(account), and assign the rows written before
// (AccountStampingEnabled and AccountOwnershipRunner in component/memql).
//
// The switch waits for the fleet because the release that stamps the field is
// also the release that adds it to the concepts. While that release rolls out,
// nodes of the previous one still serve, their copy of those concepts does not
// declare the field, and a node refuses to update a row carrying a key its
// concept does not declare. A node of the previous release never advertises
// here, so it holds the switch until it has stopped.
//
// It is the reader handshake the observation stores use (liveness.go), on
// tables of its own (migration 20261112100000_account_ownership), and not a
// second mechanism. Its own tables because an advertisement says one thing: a
// node that reads memory_liveness has said nothing about accountId. A release
// recorded on the node's v1:cluster:node row would not do instead: nodes write
// each other's rows, and a node of the previous release would refuse a row
// carrying the new key.
//
// allowActivation is the operator's hold, MEMQL_ACCOUNT_STAMPING_ACTIVATE. A
// held node still advertises and never turns the switch on. The answer is the
// cluster's either way: once the switch is on a held node is told so, and what
// it does about that is its caller's decision (the runner neither stamps nor
// assigns). Nothing turns the switch off, because rows carry the field from
// then on.
//
// Regression tests: component/memql/account_stamping_db_test.go.
func EnableAccountStampingWhenReady(ctx context.Context, db *bun.DB, nodeID string, allowActivation bool) (bool, error) {
	return enableObservationsWhenReady(ctx, db, nodeID, "account_stamping_storage", "account_stamping_readers", allowActivation)
}
