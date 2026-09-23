---
title: Mesh event delivery
audience: public
status: stable
area: operate
sinceVersion: 0.23.0
owner: znas
---

# Mesh event delivery

How the nodes of one cluster pass broadcast events to each other, what each
node reports about what it hears, and how to tell a node that hears the cluster
from one that does not. Design record:
[the mesh event delivery design](../../superpowers/specs/2026-09-22-mesh-event-delivery-design.md)
(epic memql#5338).

## What travels on the mesh

Every engine node holds `NodeService.Stream` gRPC streams to some of the
others: the bffs open streams to identity, the workbench, the agents and the
planners; agents, planners and the workbench open one to a parent bff; an edge
or an mcp opens one to its parent. A routing rule
(`node.RegisterRoutingRule`) decides which bus events a node broadcasts:
graph writes to the concepts other replicas read live, cache invalidations,
readiness registrations, `providers.reload.*` and so on. An event without a
rule stays on the node that raised it.

A broadcast reaches **every node that holds any stream to the mesh, in either
direction, exactly once**, within sixteen links of the node that raised it.
Identity is the one exception: it sends its own events and takes none.

The mesh is best-effort. A node whose every stream is down misses what is
broadcast meanwhile, and nothing replays it. Delivery that must survive a
replica switch (chat replies, streamed output, run lifecycle) rides the durable
delivery substrate instead, and every consumer that must converge after a
missed event has its own floor: the readiness safety net, the edge's site
cache TTL, and the worker dialer's thirty-second ticker.

## How a copy moves

- **Every stream carries events both ways.** A node that accepted a stream
  pushes events down it exactly as the node that opened it sends them up. Each
  accepted stream has its own bounded outbox (1024 copies) drained by its own
  goroutine, so a slow peer can never stall the event bus.
- **One transport per peer.** Two nodes often hold two streams between them
  (a bff dials an agent, and the agent dials the bff as its parent). Each copy
  goes out on exactly one of them: the stream this node opened, when it is up;
  otherwise the newest open stream the peer opened; otherwise the opened
  stream while it reconnects, whose outbox holds the copy until it is back.
- **Published once, relayed once.** A node checks every arriving copy against
  its dedup window first. A first sighting is published on the local bus and
  relayed to every other participant except the peer it came from and the node
  it started on; a repeat is set aside. A repeat that travelled a strictly
  SHORTER route than every copy before it is relayed again, never republished,
  so a copy that won a race the long way round cannot starve the far side of
  the mesh.
- **Sixteen links at most.** Every copy carries `hops`, the links it has
  travelled counting the one it arrived on. The origin sends 1, each relay adds
  one, and no node relays a copy that has already travelled sixteen. In a
  healthy cluster no copy ever reaches that limit.

## During a rolling update

The mesh wire changed in this release: `EventForward.ttl` is retired and
`hops` replaces it. A node on an older release reads the missing `ttl` as
expired and drops what an upgraded node sends, so for the minutes a rolling
update takes, **old pods do not hear new pods**. New pods hear everything old
pods send. That is the safe direction -- deaf for a few minutes, never a storm
of copies -- and the floors above cover it. Nothing needs doing: once every
pod runs the new release, every node hears every broadcast.

## What each node reports

Every node counts what it does with events from the moment its process starts,
and exposes the counts three ways.

### Prometheus

Written by every node type and scraped from each node's `/metrics`:

| Series | Labels | Meaning |
|---|---|---|
| `memql_mesh_events_total` | `outcome`: `originated`, `heard`, `relayed`, `duplicate`, `hop_limited` | Events at this node. `heard` is a first sighting delivered by a peer and published here |
| `memql_mesh_copies_total` | `transport`: `dialed`, `accepted`; `result`: `sent`, `dropped` | Copies this node put on a stream, by who opened the stream and whether the stream took it |

**Alert on `heard`.** A mesh node whose `heard` rate is flat zero while its
peers' is not is an island: it runs, it is healthy, and it hears nothing the
rest of the cluster does. Identity reads zero by design, so leave it out of
the alert. `duplicate` is normal and grows with the number of links.
`hop_limited` should stay at zero, and any sustained `dropped` rate means a
peer is not reading its stream fast enough and is missing events.

### The node's own row

Once a minute, the heartbeat that already writes a node's `v1:cluster:node`
row also writes a `mesh` object on it. No new row and no extra write:

```
mesh: {
  receives:    true,                   # false only on identity
  since:       "2026-09-22T09:00:00Z", # when this process started counting
  links:       [{ node, type, via }],  # via: dialed | accepted | both
  heard, duplicates, originated, relayed, dropped, hopLimited,
  lastHeardAt: "2026-09-22T11:59:58Z"  # absent until the first event heard
}
```

`links` lists every peer this node holds a stream with right now, and who
opened it: `dialed` (this node opened it), `accepted` (the peer did) or `both`.
When one node records another going offline, it leaves that node's last report
in place.

Read the rows with the `clusterNodes` query. `v1:cluster:node` declares no row
tier, so any signed-in user can read them, and they broadcast, so a live
subscription follows each report as it is written.

### MemQL OS: Cluster > Mesh

The Mesh section lists every node that has written its row in the last five
minutes, grouped by node type, and says in words what each one hears:

| State | Meaning |
|---|---|
| Hearing | Linked to the mesh, and heard an event within five minutes of its report |
| Not hearing | Linked to the mesh, and has heard nothing since it started. The island |
| No links | No stream links it to the mesh in either direction |
| Quiet | Heard events, then nothing for five minutes before its report |
| Starting | Started less than three minutes before its report, too early to judge |
| Sends only | Identity. It takes no events, by design |
| Not reported | Its row carries no `mesh` report: an older release, or its first heartbeat has not come yet |

Every verdict is judged against the time of the node's own report, not against
the browser's clock. A node that has not reported shows its figures as absent,
never as zeros. A band at the top counts the states, and one sentence names
any node that is not hearing or has gone quiet. Each name opens that node.

A node's page shows its counts, dated, and its links as two columns: the
streams it opened and the streams opened to it. A peer linked both ways
appears in both columns, because two streams exist. Every peer carries its own
state and opens its own page, so a node linked only to a peer that is itself
not hearing shows up without opening anything else.

## When a node is not hearing

1. **Open its page and read its links.** No links at all means it holds no
   stream: check that it can reach its parent or its seeds
   (`MEMQL_PARENT_ADDRESS`, `MEMQL_WORKER_PEERS`) and read its log for dial
   errors.
2. **Linked, and still not hearing.** Open each peer. A node linked only to
   peers that are not hearing either is part of a partition; look at the node
   those peers should reach.
3. **Copies dropped.** A non-zero `dropped` on a peer means that peer could not
   keep up with its stream and missed events unless another link carried
   them. Look at the slow peer's CPU and its event subscribers.
4. **Hop-limited copies.** Some path in this mesh is longer than sixteen links.
   The shipped topology has no such path, so look for a node whose parent is
   another edge or mcp in a long chain.
5. **During a rollout,** old pods not hearing new ones is expected (see
   above) and resolves when the rollout completes.

A node with no `MEMQL_PARENT_ADDRESS` discovers its parent from the node rows
(the mcp does, in the shipped topology), and discovery never picks identity:
identity relays nothing, so a node whose only stream led there would never
hear anything.

## Where it is tested

- `component/node/mesh_delivery_test.go` builds the cloud's own dial topology
  over real gRPC loopback streams, plus a product bff and an mcp, and asserts
  that a broadcast from every origin reaches every participant exactly once,
  with identity hearing none.
- `component/node/mesh_relay_test.go` covers the suppression rules: publish
  once, the shorter-route relay, the origin never taking its own event back,
  one transport per peer, a full outbox counted as dropped, 120 random meshes
  flooding exactly once, and the sixteen-link budget on a chain of twenty.
- `app/mesh_delivery_consumers_test.go` proves the three consumers end to end
  over the transport: readiness converges on every node, a site write evicts
  every edge's resolver cache, and a providers reload re-resolves on the
  product bff, the edge and the mcp.
