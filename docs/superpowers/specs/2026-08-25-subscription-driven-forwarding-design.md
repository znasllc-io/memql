# Subscription-driven forwarding -- ending the routing-allowlist treadmill

**Date:** 2026-08-25
**Status:** designed, NOT built. Implementation is a future epic; this record is deliverable 1 of memql#4546 (epic memql#4541, decision D3).
**Owner:** `component/node` (the event bridge and the peer mesh), `component/grpc` (the subscription registry that would advertise)

Deliverable of epic memql#4541 (event reach and public reads). Its siblings
in that epic -- memql#4542's completed forward table and memql#4543's
conformance gate -- are the *static* answer to the problem below. This
record is how the static list stops being the mechanism.

---

## 1. Problem

Cross-node event routing is default-deny (`component/node/routing.go`). An
event forwards to peers only if a rule in `defaultRoutingRules()` matches its
topic; everything else stays on the replica that produced it. Two replicas
per mesh node is the default topology, so "written on the node that served
the write, read on the node serving the browser" is the ordinary case.

A concept with no forward rule is therefore **dark**: its events reach a
browser attached to the writing replica and no other, with no error, no log
line and no metric. The surface is correct when it loads and frozen
afterwards -- which the routing file itself calls "the worst of the three
possible behaviours because it looks like it is working".

**The list is a treadmill by construction, and the evidence is now four
entries long.**

| Found | By | Concepts |
|---|---|---|
| memql#4349 | a human, looking at a frozen page | `v1:worker:registration`, `v1:worker:routingPolicy`, `v1:workbench:workspace` |
| memql#4542 | a human, looking at a frozen page | `v1:portalviews:view`, `v1:agents:*`, `v1:library:artifact` + `file`, `v1:authoring:*`, `v1:identity:invitation`, `v1:identity:account`, `v1:identity:auditEvent`, every cognition DELETE, and the delete verbs memql#4349 did not cover |

Each fix was correct. None of them closed the class, because the class is not
"these concepts were missed" -- it is that **the engine has no idea what any
browser is subscribed to.** A page's subscription and the mesh's forwarding
policy are written in two different places, by two different people, at two
different times, and nothing relates them.

memql#4543's gate makes the relationship *checkable*: any concept a portal
surface subscribes to must have a forward rule or a recorded exclusion, or
the build fails. That is a real improvement and it has a real ceiling. The
gate reads `clients/portal/src`. It cannot see a product SPA in a downstream
repo, an SDK consumer, or a subscription composed at runtime. For those, the
list is still a list and the treadmill still turns.

---

## 2. The idea

**Replicas advertise what their connected streams are subscribed to. The
event bridge forwards a graph event to a peer only when that peer advertises
a subscriber match. The static rules remain as a floor.**

This inverts the question. Today the bridge asks "is this concept on the
list?" -- a question about a document somebody maintains. It would ask "does
anybody out there actually want this?" -- a question about the live system,
which cannot go stale because nothing has to remember to update it.

It also makes the mesh *quieter* rather than louder, which is worth stating
because the instinct runs the other way. Today `v1:planner:*` broadcasts
every plan and task event to every peer whether or not a single browser is
watching. Under advertisement, a cluster with nobody on the Nexus page
forwards none of it.

---

## 3. Design

### 3.1 What is advertised

A **subscription fingerprint**: the set of (concept, verb) pairs the
replica's currently-connected streams are subscribed to.

Concept **and verb**, not concept alone. The cognition-deletes hole
(memql#4542) was exactly a verb gap -- creates and updates crossed for the
mesh's whole life and deletes never did -- and a concept-granular
advertisement reproduces it in the new mechanism.

It is a **set of identities, not a set of predicates**. A subscription can
carry filters; the advertisement deliberately does not. Forwarding a few
events a peer will drop at fan-out is cheap and correct; encoding a
subscriber's filter into a routing decision means a routing bug can silently
withhold rows, and it moves authorization-adjacent logic into the transport.
The per-row admission gate (memql#4309) stays exactly where it is.

### 3.2 Transport

A new message pair on `NodeService.Stream`, beside the existing
`PeerIntroduction` gossip:

- `SubscriptionAdvertisement { node_id, generation, concepts[] }` -- the
  replica's full current set, not a delta. Full-state is what makes a
  reconnect self-healing: a peer that missed an update converges on the next
  advertisement rather than staying wrong until something else changes.
  `generation` is a monotonic counter per replica so a late-arriving stale
  advertisement is dropped rather than applied.
- Sent on: connect (the initial state), and thereafter on change, subject to
  the hysteresis below.

The receiving side stores it on the `PeerEntry` the `PeerManager` already
keeps. `evaluateRouting` gains a peer-aware form: the block rules and the
static forward rules answer first, unchanged; only when neither matches does
the advertisement decide, and it decides **per peer** rather than for the
event as a whole.

### 3.3 Hysteresis, and why it is the hard part

A page navigation tears down one subscription set and builds another. Without
damping, every navigation in every browser on the cluster is a mesh-wide
advertisement -- so the mechanism that removes a per-concept treadmill
introduces a per-navigation one, which is worse because it scales with
traffic rather than with development.

Three rules, all of which need measuring before they are believed:

1. **Debounce.** Coalesce changes over a short window (start at 2s) and send
   one advertisement.
2. **Additions are urgent, removals are lazy.** An addition that has not
   propagated means a page is dark -- the exact failure this design exists to
   remove -- so additions flush the debounce early. A removal that has not
   propagated means the mesh carries an event nobody wants, which is waste
   rather than breakage, so removals ride the next scheduled advertisement or
   a periodic reconcile.
3. **Sticky removal.** A concept stays advertised for a grace period (start
   at 60s) after its last subscriber leaves, so a navigation away and back
   does not cost two round-trips of mesh chatter.

### 3.4 Failure posture: degrade to the floor, never to silence

**An advertisement that is missing, stale or unparseable must widen
forwarding, never narrow it.** A peer with no advertisement on file is
treated as subscribed to nothing *by the dynamic rule* and is still served
every static rule -- so the worst case of a broken advertisement path is
today's behaviour, which is a known quantity.

This is the single most important property in the design, and it is the one a
plausible implementation gets backwards. The natural shape -- "forward to
peers whose advertisement matches" -- fails closed, and failing closed here
means a silently frozen page: exactly the bug being fixed, arriving through
the fix. The static floor is what makes the failure mode *cost* rather than
*breakage*.

### 3.5 The static rules stay, for three distinct reasons

Not as a migration crutch -- permanently, and it is worth being explicit
about which rule is which, because they are usually conflated:

1. **Engine-internal consumers that are not subscriptions.**
   `cache.invalidate.*`, `authoring.promote.*` / `demote.*`,
   `providers.reload.*`, `healing.#`, `automationrun.#`. Nobody subscribes to
   these through a client stream; they are consumed by Go code inside each
   node. There is nothing to advertise.
2. **Consumers that are subscriptions but whose absence breaks the system
   rather than a page.** `graph.node.created.v1:identity:user` drives per-user
   provisioning; `graph.node.created.v1:planner:plan` drives the planner loop.
   These must cross whether or not a browser is watching.
3. **Hard denials.** The volume exclusions
   (`node.RoutingExclusions()`: `v1:worker:invocation`,
   `v1:campaigns:delivery`, `v1:identity:authActivity`, the two
   `v1:observability:*` concepts) survive as **blocks that outrank
   advertisement**. A page that subscribes to an invocation feed must not be
   able to make the mesh carry it by asking. This is the one place the
   dynamic mechanism is deliberately not authoritative.

### 3.6 Migration, and what happens to memql#4543's gate

The gate stays and its rule widens by one clause: routed **or** excluded
**or** dynamically reachable. Concretely, a concept the portal subscribes to
passes when it has a static rule, a recorded exclusion, or a demonstration
that the advertisement path covers it.

The third arm needs care, because "the dynamic mechanism will handle it" is
the kind of claim that is true right up until the advertisement path has a
bug, at which point the gate has been talked out of its job. The proposal is
that the third arm requires an in-process hop test for the concept -- the
`TestSavedViewCrossesTwoBffReplicas` shape from memql#4542 -- rather than an
assertion about the mechanism in general.

Rollout is per-node and needs no flag day: a replica that does not send
advertisements is a peer with none on file, which by §3.4 is served the
static floor. Old and new replicas interoperate, in a mesh mid-rollout, with
the static behaviour as the floor throughout.

---

## 4. What this does not solve

Stated because the design is easy to over-sell:

- **It does not make an unrouted concept reachable to a client that never
  subscribes.** A page that reads on navigation and never subscribes is
  unaffected; that is a client design choice and belongs to epic
  memql#4535's LiveCollection.
- **It does not decide what a subscriber may SEE.** Row admission
  (memql#4309) does, unchanged, at fan-out. A forwarded event that the
  receiving stream may not read is dropped there, exactly as today.
- **It does not remove the volume exclusions**, which is the point of §3.5.3.
- **It does not help a cluster whose replicas cannot gossip.** If the mesh
  is partitioned, advertisement is partitioned with it -- and degrades to the
  static floor, which is the correct answer and not a good one.

---

## 5. Open questions

1. **What is the real advertisement volume?** The whole design rests on
   navigation churn being dampable to a low rate. Nobody has measured
   subscribe/unsubscribe frequency on a real cluster. If a busy portal
   produces advertisements at anything like event rates, the hysteresis
   constants in §3.3 are guesses and the design needs a different shape
   (perhaps: advertise a coarse concept set with a long TTL, and keep verbs
   static).
2. **Where does the fingerprint live -- `PeerManager` or a new registry?**
   `PeerEntry` is the obvious home and is already the thing that goes away
   when a peer does, which handles cleanup for free. The counter-argument is
   that `PeerManager` is liveness and this is subscription state.
3. **Does the bff's WS bridge complicate the count?** A browser's
   subscriptions terminate on the bff that holds its WebSocket, which is the
   node that also serves the page -- so the advertisement is mostly "the bff
   wants everything the portal shows". Whether that makes the mechanism
   valuable or merely equivalent to the static list *for the portal
   specifically* deserves an answer before building. Its value for product
   SPAs in downstream repos is not in doubt; its value for this repo's own
   client might be.
4. **Should the advertisement be authenticated?** Peers already authenticate
   to join the mesh, so a forged advertisement needs a compromised node,
   which has larger problems. But a forged one could suppress forwarding to
   a peer (advertise nothing) -- which §3.4's floor limits to the static set
   rather than to nothing, and that may be sufficient. Decide explicitly.
5. **Is there an observability requirement?** A dynamic mechanism whose
   decisions cannot be inspected replaces "a list you can read" with "a
   behaviour you have to reproduce". At minimum a per-peer advertised-set
   dump on the Fleet page or in `make status`.

---

## 6. Prior art in this repository

- `component/node/routing.go` -- the static table, its reasons, and the
  memql#4349 comment shape every added rule follows.
- `component/node/routing_reach.go` -- `RoutingExclusions()`, the recorded
  denials this design keeps as hard blocks.
- `portal_subscription_routing_test.go` -- the conformance gate whose rule
  §3.6 widens.
- `component/node/peer.go` -- `PeerEntry` and the gossip the advertisement
  would ride beside.
- `component/memql/rowauthz_subscription.go` -- the fan-out admission gate
  this design deliberately does not touch.
