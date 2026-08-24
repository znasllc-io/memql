// Package datasync is the RUNTIME half of data origins (epic memql#4378):
// the outbox drain worker, the inbound dispatcher, and the backfill and
// reconciliation runners. It is what turns the declarations
// component/memql/sync defines into data actually moving.
//
// # Why this is not component/memql/sync
//
// The design record names `component/memql/sync/drain.go`, and the
// CONTRACT does live there -- the Connector interface, its value types,
// and the two-half registry, all of which are imported by integrations/
// and by the engine's boot check. What cannot live there is this: a
// runtime needs to ISSUE ENGINE CALLS and decode their results, which
// means importing component/memql, and component/memql/sync is a
// sub-package of it. That is an import cycle, not a preference.
//
// The split is the shape component/campaigns already has, for the same
// reason: a worker that drives the engine is a sibling of it, never a
// child. Keeping the contract in the leaf package is what lets
// integrations/ implement a Connector without pulling the runtime in.
//
// # What runs where
//
//   - The DRAIN WORKER is one goroutine per connector per node, gated by
//     the cluster execution claimer so exactly one replica drains a given
//     connector at a time. It delivers through Connector.Propagate with an
//     idempotency key, backs off on failure, dead-letters after a bounded
//     number of attempts, and audits every outcome.
//   - The INBOUND DISPATCHER routes a staged v1:platform:inboundRequest to
//     the connector its `source` names, applies the returned writes behind
//     a VERSION GUARD, and stamps the request row.
//   - The BACKFILL and RECONCILIATION runners drive Connector.Backfill and
//     Connector.Reconcile, persisting progress on v1:platform:syncState so
//     a restart resumes rather than restarts.
//
// # Two identities, deliberately
//
// The runtime reads and writes the engine's own clusterOwner-tier rows --
// the outbox queue, the health rows, the staged inbound requests -- under
// the OPERATOR identity, and it applies mirror writes under
// auth.ConnectorActor(name). Those are different authorities and the call
// sites say which is in scope, the way component/campaigns' store does:
// the operator identity is the deployment acting on its own bookkeeping,
// and the connector actor is the only identity a mirror will accept a
// write from.
package datasync
