# Mesh event delivery -- design record

Epic memql#5338. Date: 2026-09-22. Ships in ONE pull request with all five
tasks (memql#5339, #5340, #5341, #5342, #5343).

memql#5316's analysis (ruling D6 there) found the mesh island and deliberately
did not fix it: readiness got its own retry and safety net instead, and the
transport was left to this epic. The production evidence that opened it
(2026-09-13): the product bff and edge pods had logged 6 `event trigger fired`
lines since boot, against 430-1084 on the engine bff, agent and planner pods.
They were not quiet. They were deaf.

This record fixes the rulings, one per decision, in the order the work lands.

---

## The fact this epic changes

**Before.** A broadcast routing rule reached the nodes that DIALED the writer,
plus up to three relay hops along the receivers' own OUTBOUND dials. Nothing
ever sent an event down a stream a node had ACCEPTED:

- `EventBridge.forwardToPeers` sent only where `PeerManager.sendTarget`
  returned a connection, and that field was set only by the outbound dial path
  (`AttachConnection`, from the WorkerDialer and the ParentConnector).
- The server side of `NodeService.Stream` only ever answered an event with an
  `EventAck`. `NodeServerMessage_EventForward` was constructed nowhere, so the
  ParentConnector's handler for it was dead code, and the WorkerDialer did not
  have one at all.
- `childConns` / `AddChildConnection` in `peer.go` were a half-built answer
  with no caller.

So in the cloud's own topology the following heard NO mesh event, ever:

| Node | Dials | Dialed by | Heard before |
|---|---|---|---|
| product bff | identity, workbench, agents, planners | nobody (`bff-active` selects the engine bff) | nothing |
| edge | its parent, through `bff-active` | nobody | nothing |
| mcp | a discovered parent | nobody | nothing |
| the other engine bff | the same set as the first | children whose parent dial landed on it | only when round-robin favoured it |
| identity | nobody | every bff | nothing, by design |

Three consumers were broken by it on those nodes (memql#5316, section 1.3):
readiness rows, `providers.reload.*` (a federation change applied on one bff
never re-resolved providers on the others) and `v1:platform:site` cache
invalidation on the edge (kept correct only by its 30 s TTL backstop). Every
broadcast rule written to carry a row "to the replica the browser is attached
to" was silently narrower than its own comment on those nodes.

**After.** Every mesh participant that holds ANY stream to the mesh -- in
either direction -- hears every broadcast event exactly once, within 16 hops of
its origin. Identity sends its own events and hears none.

---

## D1 -- Every stream carries events both ways

**The ruling.** A node that ACCEPTS a `NodeService` stream pushes events down
it, exactly as the node that DIALED sends them up. Mechanically:

- `nodeService.Stream` wraps the stream in an `inboundStream` once the
  handshake completes: a bounded outbox (1024, the same capacity as a dialed
  connection's) drained by its own goroutine, which sends through the SAME
  `serializedStream` mutex the heartbeat and every forward reply already use.
  So a slow client can never block the event bus, and a push can never
  interleave with a heartbeat on the wire.
- The PeerManager records it under the peer's node id and releases it when the
  stream ends -- compare-and-delete, so a stream that ends late cannot release
  its replacement.
- The client side hands a pushed `EventForward` to the same arrival path as the
  server side (D6). The ParentConnector already had the case; the WorkerDialer
  gains it.

No new message: `NodeServerMessage.event_forward = 30` has been in the proto
since the mesh existed.

**Chosen over:**

- **Dialing every node from every node.** N squared connections, a second
  discovery path, and still no answer for a node whose address its peers
  cannot reach. The streams already exist; they were used in one direction.
- **Carrying broadcasts on the durable delivery substrate.** The substrate is
  keyed, polled, Postgres-backed delivery for traffic that must survive a
  replica switch (chat replies, streams, run lifecycle). Putting every
  `cache.invalidate.*` through it would turn every graph write into database
  load on every replica.
- **A broker.** A new piece of infrastructure, and a second deploy shape --
  which the environment-parity rule refuses.

---

## D2 -- One transport per peer per event

A node often holds TWO streams to the same peer: the bff dials an agent (its
WorkerDialer) and that agent also dials the bff (its ParentConnector); a static
seed (`workbench=workbench:50060`, a Service address) and a discovered pod
address can land on the same pod. Sending on every stream would deliver
duplicates that dedup then throws away.

**The ruling.** Per peer, per event, exactly one transport, chosen in this
order:

1. the dialed connection, when its stream is up right now;
2. the most recent accepted stream that is still open;
3. the dialed connection while it is reconnecting -- its outbox holds the event
   until the stream is back, which is what it did before this epic.

A peer with none of the three (a sibling learned only from gossip) is skipped
and reached by a relay (D3).

---

## D3 -- An event is published once and relayed once: flooding with suppression

**The problem it fixes before it can happen.** The old relay re-sent EVERY
arrival, duplicate or not, along outbound dials. With D1 every link carries
events both ways, so that rule would multiply copies by a node's degree at
every hop.

**The ruling.** Every arrival is checked against the dedup window FIRST. A
first sighting is published on the local bus and relayed; a repeat is dropped.
A relay goes to every event participant except the peer the copy came from and
the node the event originated on.

One refinement keeps the guarantee topology-independent: a repeat that has
travelled FEWER hops than any copy seen before it is relayed again (never
republished). Copies race; without this, a copy that arrived first the long
way round would carry a smaller remaining budget than the short-path copy that
lost the race, and the far side of the mesh could run out of hops. With it, a
node relays an event at most once per strictly-shorter path, which in practice
is once.

So every node relays each event a bounded number of times, the message count
per event is bounded by the number of stream directions, and there is no loop
to prevent: a node that has already relayed a copy at least as short as this
one does nothing with it.

**Chosen over:** a spanning tree (every node would have to agree on a topology
the mesh does not share), and relay-every-arrival (the amplifier described
above).

---

## D4 -- A hop count replaces the TTL, and the TTL field is retired

**Why the TTL could not simply be raised.** Three hops is shorter than paths
that exist in the cloud's own topology once links carry events both ways: an
edge parented on one bff to an edge parented on the other is four hops, and an
mcp whose discovered parent happens to be an edge is five. But the TTL is a
field a PRE-EPIC node reads and acts on, and a pre-epic node relays EVERY
arrival along its outbound dials. During a rolling update that makes the TTL a
multiplier: a bff fans out to about nine peers and each agent to two, so a TTL
of eight is on the order of 28,000 copies of every event while old and new
pods coexist.

**The ruling.** `EventForward.ttl` (field 7) is retired and reserved. A new
field, `hops` (9), counts the links a copy has travelled; the origin sends 0,
and a node does not relay a copy that has already travelled `meshMaxHops`
(16). Sixteen is not tuned to a topology: D3 makes every first-sighting path a
simple path, so in a mesh of seventeen nodes or fewer no copy can exhaust its
budget before every node has heard it, and the hop refinement covers the rest.

**The rollout consequence, stated rather than softened.** A pre-epic node
reads the absent `ttl` as zero -- expired -- and drops what an upgraded node
sends. For the minutes a rolling update takes, old pods are deaf to new pods;
new pods hear everything old pods send. That is the fail-safe direction: deaf,
never a storm. Every consumer that must converge already has its own floor
(the readiness safety net, the edge cache TTL, the WorkerDialer's 30 s ticker).
There is no compatibility shim that keeps old pods hearing -- CLAUDE.md's
pre-release rule, and a `ttl` written only for old readers would be exactly
that shim.

The substrate's fast-path hint is unaffected: it never used the TTL (it is not
relayed), and a pre-epic node checks the hint topic before it checks the TTL.

---

## D5 -- Identity sends and never receives

`meshEventParticipants` keeps excluding identity as a TARGET, for the reason it
was written: fanning every broadcast at the auth service would fire its
subscribers and automations on traffic it has never seen.

The other direction was never excluded -- it was simply unreachable, because
identity dials nobody. With D1 it becomes real: identity pushes its OWN events
down the streams the bffs opened to it. That is what the rules for
`graph.node.created.v1:identity:user`, `v1:identity:invitation`,
`v1:identity:account`, `v1:identity:auditEvent`, `v1:identity:group` and
`v1:identity:groupMembership` were written for -- the first one's comment says
"without this rule ... the event never leaves the identity node", and until
this epic it never did, rule or no rule.

**What that switches on, checked rather than assumed.** The per-user seed
runtime hook (`component/memql/seed_materializer.go`) now fires on the nodes
that hear a sign-up, instead of waiting for the next boot sweep. Per-user seed
ids are deterministic (`<seedName>-<userShortId>`, memql#273), so replicas
racing the same user collapse to versions of one row. Every other consumer of
those topics is a live OS surface, which is the reason each rule exists.

---

## D6 -- One entry point for an arriving event, held by the PeerManager

**The problem.** Three components receive events off the wire -- the
NodeServer, the ParentConnector and the WorkerDialer -- and each was wired to
the bridge by hand in five bootstraps and four `app/` call sites. The
WorkerDialer was simply never wired, which is part of why a push would have
been dropped even had anyone sent one.

**The ruling.** `NewEventBridge` registers the bridge as its PeerManager's
event sink, and all three components ask the PeerManager. One method,
`ReceiveForward(evt, fromNodeId)`, does D3 end to end: dedup, publish once,
relay. The per-component `SetEventInbound` setters are deleted, not left
beside it.

The same arrival path holds for every stream end, so "an event arrived at
this node" means one thing in the code and one thing in the counters (D7).

---

## D7 -- Every node says what it hears

An island was invisible: nothing on any screen, no metric and no log line said
"this node has heard nothing". The fold was right, the page was frozen, and the
evidence took an investigation to find.

**The ruling.** Three views of one set of counters, kept by the bridge:

1. **Prometheus** -- `memql_mesh_events_total{outcome}` (originated, heard,
   relayed, duplicate, hop_limited) and `memql_mesh_copies_total{transport,
   result}` (dialed / accepted; sent / dropped). Closed label sets.
2. **The node's own `v1:cluster:node` row** carries a `mesh` object, refreshed
   by the one-minute self heartbeat that already writes the row -- no new rows,
   no new writes:

   ```
   mesh: {
     receives:    true,                  # false only on identity (D5)
     since:       "<RFC3339>",           # when this process started counting
     links:       [{ node, type, via }], # via: dialed | accepted | both
     heard, duplicates, originated, relayed, dropped, hopLimited,
     lastHeardAt: "<RFC3339>"            # absent until the first event
   }
   ```

   `updateNodeHealth` gains the optional `mesh` argument; a peer-transition
   write (another node recording this one going offline) omits it, and the
   read-merge keeps the node's own last report.
3. **MemQL OS: Cluster > Mesh.** One row per live node, saying in words
   whether it hears the cluster (Hearing, Quiet, Not hearing, No links, Sends
   only, Not reported), and a page per node with its links -- which peers it
   dials and which dial it -- and its counts, each dated. A node that has not
   reported is "Not reported", never a zero. Identity reads "Sends only", not
   "Not hearing".

The section inherits the Cluster app's own floor: `v1:cluster:node` declares no
row tier, so every signed-in user can already read these rows, and a section
floor would be editorial rather than a mirror of a gate.

---

## D8 -- A discovered parent must be able to relay

`DiscoverPeerAddress` picks the first healthy `v1:cluster:node` row that is not
this node. For a node whose ONLY stream is that parent -- mcp in the cloud --
landing on identity is an island by construction: identity relays nothing
(D5). Discovery now skips identity rows. Every other node type relays.

---

## D9 -- The gates

- **The undialed-node gate** (`component/node/mesh_delivery_test.go`), written
  first and failing against the pre-epic transport with the island named:
  REAL gRPC streams over loopback -- NodeServer, ParentConnector and
  WorkerDialer, not a simulation -- in the cloud's dial topology plus a product
  bff and an mcp. A broadcast from every origin reaches every participant,
  identity hears none, each node publishes each event exactly once, and the
  copies on the wire stay within the bound D3 gives.
- **The three consumers end to end** (`test/meshdelivery/`, in the root module
  because it wires `component/edge` and `component/memql` onto the node
  transport): a registration change converges readiness everywhere, a
  providers reload re-resolves on the edge and the product bff, and a site
  write evicts every edge replica's resolver cache -- through the transport,
  with the hand-built bus bridge of the older hop tests gone from the path.
- **The readiness hop test's island list** shrinks to identity alone, as its
  own comment said this change would do. Its convergence assertion is
  unchanged.
- **Loop and dedup** unit tests: a cycle, a triangle, duplicate links between
  one pair, a race won the long way round, and the hop limit.

---

## What this does not change

- **The mesh is still best-effort.** A node whose every stream is down misses
  what is broadcast meanwhile, and nothing replays it. The durable substrate
  remains the guarantee for keyed delivery. The readiness safety net, the edge
  TTL and the dialer ticker stay: they are no longer the only thing that makes
  those nodes right, but a node mid-reconnect still needs them.
- **Routing rules.** No rule is added or removed. What a broadcast rule means
  is finally what its comments always said.
- **Automations.** A node that hears an event fires its triggers, as before;
  the cluster execution guard (#561) collapses the replicas to one execution.
  More replicas hear more events, so there are more claim attempts -- not more
  executions.

## Rejected

- **Queueing events for an absent peer** (the retired #1232 outbox): a mesh
  that remembers is the substrate's job, and doing it twice was retired for
  good reasons in memql#1267.
- **Keeping `ttl` alive for pre-epic readers**: D4.
- **Making identity a receiver**: D5.
