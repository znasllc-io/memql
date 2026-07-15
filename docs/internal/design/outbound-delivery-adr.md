---
title: Outbound delivery -- engine-drained outbound requests (email / webhook)
audience: internal
status: accepted
area: internal
sinceVersion: 0.12.2
owner: znas
---

# ADR: Outbound delivery -- engine-drained outbound requests

> Design deliverable for memql#2521. A pure-DSL product pack can model
> outbound communication as graph state, but nothing in the engine can then
> PERFORM the delivery: the only route today is a product bff plugin via
> `@executor("integration.*")` -- bespoke Go, defeating the zero-Go product
> model for a need as generic as "send what my automation decided to send".

## 1. Context

Products authored purely in DSL react to graph events with automations and
stage the *decision* to notify someone as graph rows. The engine has three
email transports (`integrations/email`: SMTP, Microsoft Graph, log-only) but
the only consumer is the identity magic-link issuer, which sends
synchronously inside its own request path
(`component/identity/magiclink/issuer.go`). There is no product-agnostic
path from "an automation staged a row" to "a transport delivered it".

Every downstream product with notification or dispatch automations re-hits
this gap. The fix must not require product Go, must not put secret material
in graph rows, and must be safe by default (an engine that can POST
anywhere on the operator's behalf is an SSRF primitive unless egress is
allowlisted per deployment).

Engine precedents this design builds on:

- **Poll-loop worker:** `integrations/planner/stranded_plan_watchdog.go` --
  ticker poller over status-filtered rows, Go-side time comparisons (the
  MemQL comparison evaluator cannot do `createdAt`-vs-`now()` arithmetic),
  env-tunable interval/threshold, startup delay, default-on.
- **Cross-replica exactly-once claim:**
  `component/automations.ClusterExecutionGuard` -- DB-backed claim ledger so
  N replicas act once per key.
- **Transport + secret resolution:** `integrations/email` plugin
  (`NewSenderFromEnv` priority Graph -> SMTP -> Log; `LazySender` resolves
  credentials from `platform:globalSecret` rows / env at send time -- never
  from the staged row).
- **Event fast-path:** concept-row writes publish
  `graph.node.created.<concept>` on the in-process `events.Bus`
  (`component/memql/executor_mutation.go`), so a worker can react
  immediately and use its poll as the safety net -- the watchdog's
  "fast path = event, safety net = poll" at-least-once shape.
- **Delivery-contract vocabulary:** the mesh delivery substrate ADR
  (at-least-once + idempotent dedup by id). That substrate addresses
  *internal* consumers by logical key; outbound delivery addresses
  *external* systems, so it rides the same ideas, not the same backbone.

## 2. Shapes evaluated

### 2a. Core builtin: dispatch(medium, target, payload) from automations

A builtin callable from automation/logic bodies, delivering inline (or
enqueueing internally) with engine-owned transport config.

- Couples delivery latency and failure to automation execution; a slow SMTP
  relay stalls the automation runner.
- Delivery state is invisible: no row to inspect, retry, or audit; a failed
  send exists only in logs.
- Grants every automation author an egress primitive with no natural seam
  for per-deployment allowlisting or postmortem audit.

### 2b. Engine-drained outbound-request concept (CHOSEN)

Products stage rows of a platform-owned concept
(`platform:outboundRequest`: medium, target, payload, `status="pending"`);
a product-agnostic engine worker drains them, performs delivery, and stamps
status transitions (`sent` / `retrying` / `failed` + attempt metadata).

- Zero product Go: staging a row is an ordinary mutation from any
  automation or logic body.
- Delivery state IS graph state: products can query it, build UX on it,
  and automations can react to its transitions.
- The row is a durable outbox: at-least-once delivery with visible attempt
  metadata, independent of automation execution.
- One choke point for policy: allowlists, payload caps, and transport
  credentials live in the worker's deploy-layer config.

## 3. Decision

Implement 2b:

1. **Concept** `platform:outboundRequest` (in `dsl/platform`, modeled on
   `platform:missingCapability`'s lifecycle-status shape).
2. **Staging surface** in `dsl/platform`: mutation
   `stageOutboundRequest(medium, target, subject, payload, dedupeKey,
   requestedBy)` and query `outboundRequests(status)` so products never
   declare the concept themselves.
3. **Worker** `component/outbound` -- a lifecycle component on the bff node
   cloning the stranded-plan-watchdog loop: startup delay, ticker poll,
   `drainOnce(ctx)` as the synchronously testable unit, event fast-path
   subscription on `graph.node.created.*` for the concept, ClusterExecutionGuard
   claim per (row, attempt).
4. **Transports v1:** `email` (delegates to the existing
   `integrations/email` plugin -- Graph/SMTP/Log selection and secret
   resolution come free) and `webhook` (HTTPS POST, JSON body). A generic
   `http` medium (methods, custom headers) is deferred until a concrete
   consumer needs it; `webhook` covers the known cases.

## 4. Delivery contract

### 4.1 Status machine

```
pending  --claim-->  sending  --2xx/accepted-->  sent (sentAt stamped)
sending  --retryable failure, attempts < max-->  retrying
                                (attempts++, lastError, nextAttemptAt = backoff)
retrying --nextAttemptAt due, claim-->           sending
sending  --permanent failure or attempts >= max--> failed (lastError)
```

- Backoff: bounded exponential with jitter (base 30s, factor 4, cap 1h,
  default max 5 attempts). `retrying` rows become eligible when
  `nextAttemptAt <= now`, evaluated Go-side (watchdog precedent).
- Retryable: transport timeouts, connection errors, HTTP 408/429/5xx.
  Permanent: other 4xx, allowlist rejection, payload-cap rejection,
  malformed target, medium not configured.

### 4.2 At-least-once + dedup pass-through

Delivery is at-least-once: a crash between transport success and the `sent`
stamp re-delivers on the next claim. The optional `dedupeKey` is passed
through to the receiver (webhook: `X-Memql-Dedupe-Key` header, alongside
`X-Memql-Outbound-Id`; email: appended to the Message-ID-adjacent headers
where the transport allows) so external receivers can deduplicate. The
engine does not enforce staging-time uniqueness in v1.

### 4.3 Security invariants

- **No secrets in rows.** Rows carry target + payload + status only.
  Transport credentials resolve at send time from env /
  `platform:globalSecret` (email `LazySender` precedent).
- **Deny-by-default egress.** Each medium is enabled only by a non-empty
  per-deployment allowlist. `email`: case-insensitive recipient domain
  suffix list. `webhook`: URL prefix list, `https://` required (plain
  `http://` tolerated only for cluster-internal hosts the operator
  explicitly lists). A row whose target misses an allowlist a node IS
  configured for fails fast with an explicit `lastError` -- loud, never a
  silent backlog.
- **Per-node egress gate is pre-claim (memql#2540).** The worker is
  registered on every engine node, but the allowlist env is set only on the
  node types that own egress. The "is this medium enabled here" check
  (allowlist present for the row's medium) runs BEFORE the cluster-guard
  claim: a node with no allowlist for the medium SKIPS the row (no claim, no
  stamp, leaves it `pending`), so only configured nodes contend for the
  claim and the guard elects one that can actually deliver. Target-allowlist
  and payload-cap checks stay POST-claim so a configured node still fails a
  genuinely bad row loudly. Trade-off: a medium configured on NO node leaves
  its rows `pending` rather than failing loud; the operator resolves it by
  setting the allowlist. An unknown medium (no allowlist concept) falls
  through to the post-claim gate and one node stamps it `failed`.
- **Payload cap.** Rows whose body exceeds `MEMQL_OUTBOUND_MAX_PAYLOAD_BYTES`
  (default 262144) fail permanently.

### 4.4 Configuration (env, `MEMQL_OUTBOUND_*`)

| Var | Default | Meaning |
|---|---|---|
| `MEMQL_OUTBOUND_ENABLED` | `true` | worker on/off (inert unless an allowlist enables a medium) |
| `MEMQL_OUTBOUND_POLL_SECONDS` | `15` | safety-net poll interval |
| `MEMQL_OUTBOUND_STARTUP_DELAY_SECONDS` | `20` | first-poll delay after boot |
| `MEMQL_OUTBOUND_MAX_ATTEMPTS` | `5` | attempts before `failed` |
| `MEMQL_OUTBOUND_EMAIL_ALLOWLIST` | empty (disabled) | comma-separated recipient domain suffixes |
| `MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST` | empty (disabled) | comma-separated URL prefixes |
| `MEMQL_OUTBOUND_MAX_PAYLOAD_BYTES` | `262144` | staging payload cap |
| `MEMQL_OUTBOUND_HTTP_TIMEOUT_SECONDS` | `10` | webhook client timeout |

## 5. Concrete sketch

Concept (per-kind file layout of `dsl/platform`):

```
concept outboundRequest {
  medium        enum("email","webhook") @required
  target        string @required          // recipient address or webhook URL
  subject       string                    // email subject; ignored for webhook
  body          string @required          // email body / webhook JSON body
                                          // (named body, not payload: shape
                                          // paths reserve the payload prefix)
  status        enum("pending","sending","sent","retrying","failed") @default("pending")
  attempts      int @default("0")
  lastError     string                    // truncated to 4KB
  nextAttemptAt datetime
  sentAt        datetime
  dedupeKey     string                    // passed through to the receiver
  requestedBy   string                    // product/automation provenance
}
```

Worker skeleton: `Worker.drainOnce(ctx)` selects `pending` + due
`retrying` rows via `engine.Execute`, and per row: per-node egress gate
(is the medium's allowlist present on THIS node? if not, skip -- memql#2540)
-> guard-claim `outbound:<id>:<attempts>` -> admit (target allowlist, cap)
-> stamp `sending` -> `transport.Deliver(row)` -> stamp terminal/retry state. The transport is
an injected interface (`fakeTransport` in tests); the poll ticker and
startup delay are env config resolved at construction; tests call
`drainOnce` directly (cron-leader / drain-hook test precedent).

## 6. Consequences

- Products get delivery with zero Go, and delivery state as graph state --
  automations can chain on `status` transitions (e.g. escalate on
  `failed`).
- Operators must set allowlists per deployment before anything egresses;
  the default posture is inert. A target that misses an allowlist a node is
  configured for is loud (the row fails with an explicit error). A medium
  configured on NO node is the one silent case (rows stay `pending`): the
  pre-claim egress gate (memql#2540) trades that against the worse failure
  it prevents -- an unconfigured node claiming and killing a row a
  configured peer could deliver.
- **Known gap (not yet closed): claim/stamp wedge.** The claim is keyed per
  (row, attempts) and persists in `automation_execution_claims` until the
  guard prune TTL (retention, default 1h). A replica that dies AFTER
  claiming an attempt but BEFORE stamping `sending`/`sent`/`failed` leaves
  the row `pending` while peers re-claim-and-lose the persisted key, so the
  row is wedged until the TTL elapses. Closing it (a claim TTL scoped to the
  attempt, or stamping `sending` in the same tx as the claim plus a
  `sending` reaper) touches the shared `ClusterExecutionGuard` semantics and
  is deferred to a follow-up (tracked on memql#2540).
- At-least-once means receivers may see duplicates after a crash;
  `dedupeKey` pass-through is the mitigation, matching the mesh substrate's
  dedup-by-id stance.
- The in-process event fast-path fires on the replica that executed the
  mutation; other replicas rely on the poll. The guard makes the race
  harmless.
- The identity magic-link path stays synchronous (its UX is
  latency-sensitive and it predates this worker); converging it onto the
  outbox is possible later but out of scope.

## 7. References

- memql#2521 (capability ask), memql#1259 / mesh-delivery-substrate-adr.md
  (delivery-contract vocabulary)
- `integrations/planner/stranded_plan_watchdog.go` (loop shape),
  `component/automations` ClusterExecutionGuard (cross-replica claim),
  `integrations/email` (transports + secret resolution),
  `component/memql/executor_mutation.go` (graph.node.created fast-path)
