# Fleet connection hardening -- design record

Epic memql#5327. Date: 2026-09-22.

The 2026-09-13 audit of the cockpit <-> cluster connection layer (the comment on
memql#5327) ranked eighteen findings across two repositories. This record fixes
the ENGINE-side rulings, one per decision, in the order the work lands. The
cockpit half is `znasllc-io/memql-cockpit#425`; the per-user / per-group sharing
model is memql#5344 and is deliberately NOT here.

What every ruling below has in common: the audit asked "why does the state lie?"
and the answer was almost never a missing check. It was a fact established once,
at stream open, and then believed for the life of a connection that can outlive
it by days.

---

## D1 -- Revocation and supersede reach the holding replica through the graph, not through a new RPC

**The problem.** A worker's token and registration are resolved ONCE, in
`admitRegistration`, and the recv loop never looks again
(`component/worker/server.go`). Revoking either changes no running stream. The
stream that must end is held by exactly one agent replica, which is usually not
the replica the revoke arrived on.

**The ruling.** `v1:worker:registration` already carries broadcast routing rules
(`component/node/routing.go`: `graph.node.created` / `graph.node.updated`), so
every agent replica already sees every registration write. One subscriber --
`component/worker`'s `RegistrationWatcher`, installed on the agent node --
reads each event and asks two questions of the row it names:

- is it revoked, and do we hold its stream? -> drain it, reason `revoked`.
- does `connectedNodeId` name a DIFFERENT node, and do we hold its stream? ->
  drain it, reason `superseded`.

That is one mechanism for two findings (H-4 and M-4). No new `NodeService`
message, no new proto field, and no fan-out the mesh does not already pay for.

**Why not a targeted RPC.** A supersede RPC has to name the replica to send to,
which is the very field that is wrong when a pod has died. The broadcast is
addressed to whoever is holding the stream, which is the only correct address.

**What it does NOT close.** A replica that has lost its mesh connection sees no
event. That is memql#5338 (`mesh-event-delivery`) and is why D3's heartbeat
re-check exists as an independent backstop rather than as belt-and-braces.

---

## D2 -- "Remove this machine" is ONE act with ONE name

**The problem.** MemQL OS's remove button called `revokeWorker` and nothing else,
so the registration was excluded from routing while the TOKEN stayed active: the
machine kept heartbeating, kept its stream, and could re-register the moment a
new row appeared. An operator told "removed" had removed half of it.

**The ruling.** A new engine builtin `fleetRevokeMachine(registrationId, reason)`
does both writes server-side, in one act, under the caller's own ownership:

1. resolve the registration through `workersForUser` -- the authorized path, so
   a machine that is not the caller's is not in the answer and the not-found and
   not-yours refusals are the same sentence;
2. revoke the registration row (`revokeWorker`);
3. revoke the bound `v1:identity:identity` worker-token row.

The client never learns the `identityId` and never composes two writes with a
window between them. D1's watcher then ends the stream.

**Order matters and it is row-first.** The row revoke is the one D1 watches, and
a token revoked first would leave a machine unable to reconnect while its row
still advertised it as routable -- the same split in the other direction.

---

## D3 -- Only a STATED refusal ends a live stream

**The problem.** D1 covers a revoke that reaches the mesh. It does not cover a
token revoked through a path that writes no registration row, and it does not
cover expiry, which no write announces at all.

**The ruling.** The heartbeat handler re-resolves the stream's worker identity at
most once per `IdentityRecheckInterval` (60s -- four beats). Three answers are
possible and only ONE of them ends the stream:

| Answer | Verdict |
|---|---|
| `active == false`, or a past `expiresAt` | end it, under a named reason |
| the lookup ERRORED | keep it, logged at warn |
| the identity was NOT FOUND | keep it, logged at debug |

**Why fail-open here when `component/node`'s stream gate fails closed.** That
gate runs at stream OPEN, where the cost of a wrong refusal is one retry a second
later. This runs on a live connection: failing closed on an unreadable row turns
a database hiccup into a cluster-wide disconnect of every paired machine, each of
which then reconnects into the same hiccup. The security property is not lost --
it is deferred by at most one interval, and the reconnect goes through the
open-time check, which DOES fail closed on all three.

**NOT FOUND is the row of that table worth arguing, and it goes the same way.**
Revocation here is a SOFT flag -- `revokeWorkerTokenIdentity` sets
`active=false` -- and nothing in the tree deletes an identity row. So absence is
not a decision anybody made; it is a read that did not see what is there, which
is an outage or a bug. Reading it as a revoke would disconnect every machine in
the cluster the first time a paginated query came back empty, which is the
failure this epic exists to stop rather than one to add.

---

## D4 -- Worker tokens expire, and the default is 90 days

**The problem.** `component/identity/http/pair.go` minted `time.Time{}`, which
`resolveWorkerToken` reads as non-expiring. Combined with H-4 a leaked token was
valid forever.

**The ruling.** Pairing mints `expiresAt = now + WorkerTokenDefaultTTL`, a
90-day constant in `component/identity/workertoken`. Ninety days is long enough
that a laptop that sleeps for a season still wakes up paired, and short enough
that a token copied out of a backup is worthless within one quarter.

**It is a constant, not an env var, and that is the ruling rather than an
omission.** A knob here has exactly one honest setting -- shorter -- and an
operator who wants shorter has rotation (D5), which renews without a person. A
knob whose only other position is "never expire" re-creates the finding.

**Rows minted before this stay non-expiring.** Nothing back-fills them: an expiry
stamped retroactively onto a token somebody is using is a disconnect nobody asked
for. They are renewed the first time they rotate.

---

## D5 -- Rotation renews the SAME identity row, and the previous hash survives a grace window

**The problem.** `handleRotationRequest` replied with an empty
`RotationResponse` (an MVP stub), so a cockpit that asked for rotation got an
acknowledgment and no token.

**The ruling.** A `RotationRequest` on a live stream:

1. mints a fresh plaintext + hash;
2. writes the new hash and a new `expiresAt` onto the SAME
   `v1:identity:identity` row, moving the CURRENT hash to
   `credentials.previousKeyHash` with a `previousKeyExpiresAt` of now + 15
   minutes;
3. replies with the plaintext and the new expiry.

**Same row, because the registration is bound to `identityId`.** Minting a new
row would unbind the registration and send the machine through machine-key
reclaim on its next connect -- which is the duplicate-row lineage `ef64aa3`
exists to prevent, triggered deliberately.

**The grace window is the whole reason this is safe.** The cockpit has to write
the new token to disk before it is useful, and a crash between the server's write
and that `fsync` would otherwise lock the machine out permanently with no
recovery but re-pairing. Fifteen minutes is a reconnect-and-retry, not a security
window: the previous hash is accepted ONLY until then and ONLY for the same row.

**Rotation is offered, never forced.** The server does not push. A cockpit that
never asks keeps its token until it expires, at which point it re-pairs -- which
is the honest outcome for a machine nobody is maintaining.

---

## D6 -- `lastSeenAt` is the server's clock; the cockpit's stamp becomes measured skew

**The problem.** `handleHeartbeat` took `hb.GetTs()` -- the cockpit's
`timestamppb.Now()` -- and persisted it, while `IsOnline` compares against the
SERVER's now. A cockpit 45s behind read offline while connected.

**The ruling.** `at := s.server.clock()`, always. The client's stamp is kept, and
it is kept as what it actually is: `heartbeat_skew_ms`, computed and logged (and
exposed on the row as `clockSkewMs`) so a machine whose clock is wrong is
diagnosable rather than merely absent.

**This also fixes the throttle**, which was comparing two client values and
therefore could be steered by a client that lied about its clock.

---

## D7 -- The stale-hold sweep clears by the ONLINE WINDOW alone, not by node liveness

**The problem.** `connectedNodeId` is cleared only by a graceful close. A
SIGKILLed agent pod -- every redeploy renames pods -- leaves the stamp forever,
and `StreamHeld` reads true for a machine nothing holds.

**The ruling.** A two-minute cron automation
(`workerStaleConnectionSweep`) clears `connectedNodeId` on every registration
whose `lastSeenAt` is older than the online window, run on the cron leader under
the maintenance principal.

**The audit proposed a second disjunct -- "or the node id is absent from live
`v1:cluster:node`" -- and this ruling drops it deliberately.** A heartbeat
arrives THROUGH the stream on the holding pod, so a pod that has gone cannot be
refreshing `lastSeenAt`; the window subsumes the liveness read. Keeping it would
add a cross-concept read that can itself be stale, and whose staleness would
clear the hold of a live pod the node registry had not caught up with. One
signal, and it is the one that cannot lie.

The window is `OnlineWindow` (30s) plus one `HeartbeatBatchInterval` (15s) of
slack, because the flush is throttled and a row can legitimately be one interval
behind the stream.

---

## D8 -- The conditional clear is a compare-and-swap in the store, and what remains is closed by D7

**The problem (M-13).** `clearConnectedNode` guards by read-then-write in the
session, but `clearWorkerConnectedNode` writes unconditionally, so a sibling
replica stamping between the read and the write is wiped.

**The ruling.** `EngineStore.ClearConnectedNode` takes the expected holder and
performs the compare-and-swap itself: it re-reads immediately before the write
and refuses when the persisted holder is no longer the expected one. Every caller
that knows which node it is passes it; the sweep passes empty, which means
unconditional and is the only caller entitled to that.

**The mutation gains no argument, and that is forced rather than chosen.** A
`.memql` mutation carries no filter, and an `args` field a body never references
is refused at load -- so an `expectedNodeId` argument could only be decoration.
The guard lives where it can be enforced, and `clearWorkerConnectedNode`'s doc
comment says so, in the house style that already warns against reading the
absence of a guard in a body as the absence of a guard.

**What remains.** The read->write window inside the store. Its consequence is
bounded in both directions and neither direction is permanent: a hold wrongly
wiped is re-stamped by the successor's next heartbeat flush (<=15s), and a hold
wrongly kept is cleared by D7's sweep (<=2min). Before this epic the second
direction had no closer at all.

---

## D9 -- A new token reclaiming a machine key revokes the token it displaced

**The problem (M-4, second half).** Machine-key reclaim rebinds the registration
row's `identityId` to the new token while the OLD token stays active -- so the
old cockpit reclaims the row straight back and the two flap.

**The ruling.** When `upsertRegistration` reclaims by machine key AND the
displaced row carried a different `identityId`, that identity is revoked as part
of the reclaim. One machine, one live credential.

**It is scoped to reclaim, never to an ordinary refresh.** A refresh under the
same `identityId` revokes nothing; the revoke fires only where the row's binding
actually MOVED, which is exactly the case where two credentials for one physical
machine are known to exist.

---

## D10 -- Shared inference is admitted across the replica hop by the same predicate that admitted it locally

**The problem (H-2).** The router picks a cluster-shared machine belonging to
owner O for acting user U; the forward receiver resolves the machine through
`WorkersForOwner(U)` and refuses it as not-owned. On the default two-replica
topology the feature worked only when the stream happened to be local.

**The ruling.** The receiver's `verifyRegistration` admits a registration when
EITHER the verified authority's subject owns it, OR the row is unrevoked and
`ServesCluster()` resolved through `SharedFleetStore` -- the same cross-owner
read the router used to pick it, and the same one that is deliberately on a
second interface so user-scoped paths cannot reach it.

**The registry owner-mismatch check moves with it.** `model_forward.go`'s
`w.OwnerUserId != owner` refusal is correct for an owned call and wrong for a
shared one; it now refuses only when the machine is neither the caller's nor
cluster-shared.

**The tool-dispatch half is NOT widened.** `WorkerForward` stays owner-only.
Sharing a machine lends its GPU, not its shell -- `dispatch.go` and
`app_inference.go` are owner-only by design and this ruling does not touch them.

---

## D11 -- The sharing ledger's promise is enforced by the query's reach, not only by the fold

**The problem (H-3).** `routerCallsOnMachine` is not `@serverOnly`, is generated
into both SDKs, and sits on `v1:router:call`, which declares
`@rowAuthz(public, requiresIdentity)`. Its shape projects `userId`. The
ownership gate lived only in the Go builtin -- so any signed-in caller could ask
the query directly, for any machine, and read who ran what and when.

**The ruling.** The query becomes `@serverOnly`; `fleetSharingLedger` stamps
internal origin for the one call that renders it, as a LOCAL context that is
never returned. The fold stays exactly where it is: the promise is now enforced
twice, and the second enforcement is the one that survives a rewrite of the
first.

**`userId` stays on the shape.** People are counted, and a count of people needs
their ids for exactly as long as the fold takes to turn them into a cardinality.
Dropping the field would make the ledger's "for how many people" unanswerable,
which is the figure the person who lent the machine actually asked for.

---

## D12 -- A refusal never enumerates another user's registration ids

**The problem (M-9).** `FleetUnavailable.consideredKeys()` aggregates foreign
private-share refusals to a count -- except when NOTHING actionable remains, in
which case it restores the raw ids on the grounds that they are the only repair
the operator has. With D11 open that was an enumeration primitive.

**The ruling.** Foreign entries are aggregated in BOTH branches. The
foreign-only case keeps its repair sentence, which is the same sentence for
every foreign machine and needs no id to be actionable: the machines exist and
their owners have not shared them.

**A machine the CALLER owns is still named.** That is their own id, it is the
one thing they can act on, and withholding it would make the common refusal
unreadable to fix a leak that only concerns other people's rows.

---

## D13 -- The LOOP BREAKER is keyed per acting user; the two ceilings are not

**The problem (L-8).** `FleetCallFingerprint` hashes model + messages and
nothing else, so two people asking the same question on the same replica shared
a loop-breaker key: one person's runaway tripped the breaker for the other, who
had made one call.

**The ruling.** The acting user leads the hash, in `FleetCallFingerprint` and in
`AppCallFingerprint` -- identically, because the two breakers share one guard and
a key shape that differed between them would make "why did this trip" depend on
which door answered. An EMPTY acting user is system work, which is a real caller
and gets its own key: a maintenance sweep repeating itself is exactly as much a
runaway as a person is.

**Only the breaker.** The RATE ceiling stays per-lane and the CUMULATIVE ceiling
stays per-process, and that is a ruling rather than an omission: those two are
the operator's valve on how fast this replica makes model calls at all, and a
per-user rate ceiling would let N users collectively exceed a figure an operator
set once. The audit's finding names `ai_guard_fleet.go:137-160`, which is this
function, and this function is the whole of it.

**This is an availability fix, not an isolation one.** No user could read
another's traffic through the guard; they could only stop it.

---

## D14 -- Worker connect and disconnect are audited on the agent, and `worker` is a `targetType`

**The problem (M-8).** `workerAuditorForApp` returns `NoopAuditor{}`, so every
`worker_registered` / `worker_disconnected` event on every agent node went
nowhere. Pairing was audited; connecting was not.

**The ruling.** The agent wires `worker.IdentityAuditor` over an
`identity.SlogAuditLogger` with an `identity.EngineAuditSink`, the same path
`component/packages` uses from a non-identity node. `v1:identity:auditEvent`'s
`targetType` enum gains `worker`, and `createAuditEvent` accepts it.

**`worker` is its own value rather than `identity`.** An `identity` target names
a credential that proves who somebody IS; a registration names a MACHINE that
somebody owns, and the audit question asked of it -- which machine connected,
from where, and when did it stop -- is not a question about a credential.

---

## D15 -- A shared call records whose hardware served it

**The problem (M-10).** `MachineOwnerUserId` on the decision record was
documented as "empty until shared team machines land". They landed; the field
stayed empty, so no row said "U's call ran on O's hardware".

**The ruling.** The fleet path stamps `MachineOwnerUserId` from the winning
candidate's `OwnerUserId` whenever that owner is NOT the acting user. A call on
one's own machine still leaves it empty rather than repeating `userId` -- the
field answers "whose machine, if not yours", and filling it in the self case
would make "is this a shared call" a comparison rather than a read.

---

## D16 -- The failure paths get lanes, and the worker lane is in-process

**The problem.** The audit's coverage table is a list of untested failure paths:
revocation mid-stream, two replicas, dead pods, the shared hop.

**The ruling.** Every ruling above lands with a test that FAILS against the code
it replaces. The cross-node ones are IN-PROCESS hop tests in
`integrations/agent/worker` and `component/worker`, following memql#4352's
precedent rather than a `clustere2e` lane -- a live-cluster gate is skipped on
every CI lane and every developer machine, and a gate skipped by default cannot
be what stands between a feature and the bug it prevents.

`test/clustere2e` gains a worker lane anyway, for the one thing an in-process
test cannot assert: that the registration broadcast actually reaches a sibling
replica through the real mesh. It is additive, and nothing above depends on it.

---

## What this epic found in the gates themselves

Two things, both discovered by a gate refusing work rather than by review, and
both fixed here because leaving them would make the next author pay the same
cost.

**`TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin` could not see
`RenderCall`.** It matches string literals shaped `mutation <name>(`, and its
own limitations note said "every call in the tree today is a literal or a
Sprintf format string". That stopped being true when memql#5004 introduced
`langparser.RenderCall` as the renderer every new write is supposed to use --
so the blind spot was GROWING. Measured, not theorised: D2 and M-2 made two
mutations `@serverOnly` and their builtins rendered them on an unstamped
context. Both would have been refused on every call, with one WARN and nothing
else, and the gate was green. It sees `RenderCall` now, and the one legitimate
caller it newly flags -- the nightly evidence fold, reached only from a
tree-loaded automation that has already stamped -- is a named exemption with a
staleness check of its own.

**`TestWorkerTokenListForUserIsAlwaysCallerScoped` refused the credential
re-check, correctly.** D3's first implementation resolved the identity through
`workertoken.ListForUser`, whose query is keyed on a caller-supplied userId and
projects `keyHash`; that gate pins every caller of it to the authenticated
caller's `Subject`, and a worker's subject is `worker:<identityId>` rather than
a user. The fix was not an exemption but a better shape:
`workerTokenIdentityById`, keyed on the CREDENTIAL'S OWN ID. There is then no
user id to supply, so there is nothing to enumerate -- the stronger form of the
same property rather than a waiver from it.

---

## Out of scope, and where it lives

| Finding | Where |
|---|---|
| C-1 unserialized `Send` | cockpit; engine SDK half shipped as memql#5351 |
| H-5 revoked token retried forever | cockpit memql-cockpit#425 |
| H-6 `inference.serve` needs a reconnect | cockpit memql-cockpit#425 |
| H-8 legacy `worker.yaml` mirror | cockpit memql-cockpit#425 |
| M-1 per-user / per-group sharing | memql#5344 |
| M-5 per-home model concurrency | cockpit memql-cockpit#425 |
| M-6 `machineId` path divergence | cockpit memql-cockpit#425 |
| M-12 one consent window across clusters | cockpit memql-cockpit#425 |
| L-1..L-7, L-9 | recorded, not scheduled |

M-2 (the cluster-owner share escape) IS in scope and is the one ruling with no
letter above, because it needs none: `setWorkerSharing` becomes `@serverOnly`
and routes through a builtin that resolves the row through the caller's OWN
machines, which is D2's shape applied to a second write. The mutation's comment
asserted the write guard refuses any actor but the owner; the guard grants the
cluster-owner escape on the `owner=..., clusterOwner` tier, so the assertion was
false and a cluster owner could share hardware they do not own.
