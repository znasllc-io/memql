---
title: Outbound delivery -- staging rows, allowlists, and the drain worker
audience: public
status: stable
area: operate
sinceVersion: 0.12.2
owner: znas
---

# Outbound delivery

**Audience:** product authors staging outbound sends from pure DSL, and
operators configuring per-deployment egress.
**Design:** `docs/internal/design/outbound-delivery-adr.md` (memql#2521).

A pure-DSL product pack can now SEND things -- email and webhooks --
without any product Go. Products stage `v1:platform:outboundRequest`
rows; the engine-owned outbound worker drains them, delivers through
the deploy-configured transport, and stamps the delivery lifecycle back
onto the row, so delivery state is ordinary graph state.

## Staging a delivery (product DSL)

Call the platform mutation from any automation or logic body:

```
stageOutboundRequest(
  requestId: hash(concat("orderShipped:", args.orderId)),
  medium: "webhook",
  target: "https://hooks.internal.example/notify",
  body: payloadJson,
  dedupeKey: args.orderId,
  requestedBy: "myproduct.orderShipped"
)
```

- `requestId` is caller-supplied; derive it with `hash(concat(...))`
  for staging-time idempotency.
- `medium` is `"email"` or `"webhook"`. For email, `target` is the
  recipient address and `subject` applies; for webhook, `target` is an
  absolute URL and `body` is POSTed as JSON.
- Never put secret material in `body` or `target`: rows are graph
  state. Transport credentials live in deployment config.

Audit delivery state with `outboundRequestsByStatus(status: "failed")`
(or any other status). The lifecycle is
`pending -> sending -> sent | retrying | failed`, with `attempts`,
`lastError`, `nextAttemptAt`, and `sentAt` stamped by the worker.
Automations can react to the status transitions like any other
concept update.

## Operator configuration (deny-by-default)

The worker runs on every engine node but is **inert until an allowlist
admits targets** -- an unconfigured deployment never egresses. Set the
`MEMQL_OUTBOUND_*` vars on the node type that should own egress
(typically the bff):

| Var | Default | Meaning |
|---|---|---|
| `MEMQL_OUTBOUND_ENABLED` | `true` | worker on/off |
| `MEMQL_OUTBOUND_EMAIL_ALLOWLIST` | empty (disabled) | recipient domain suffixes, comma-separated; subdomains admitted |
| `MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST` | empty (disabled) | URL prefixes, comma-separated; https required unless the prefix itself is `http://` (cluster-internal opt-in) |
| `MEMQL_OUTBOUND_POLL_SECONDS` | `15` | safety-net drain interval |
| `MEMQL_OUTBOUND_STARTUP_DELAY_SECONDS` | `20` | first-drain delay after boot |
| `MEMQL_OUTBOUND_MAX_ATTEMPTS` | `5` | attempts before `failed` |
| `MEMQL_OUTBOUND_MAX_PAYLOAD_BYTES` | `262144` | body cap at drain time |
| `MEMQL_OUTBOUND_HTTP_TIMEOUT_SECONDS` | `10` | webhook request timeout |
| `MEMQL_OUTBOUND_CLAIM_TTL_SECONDS` | `300` | how long a won cross-replica claim blocks peers before a dead claimant's claim is re-winnable (crash recovery); keep it comfortably above `HTTP_TIMEOUT_SECONDS` |

Email transport selection and credentials come from the existing email
integration (`MEMQL_EMAIL_*`: Microsoft Graph, SMTP, or the dev log
sender) -- the same source the identity magic-link sender uses. A row
whose target misses an allowlist a node is configured for, or whose body
exceeds the cap, fails **fast and loud** (`status="failed"` with an
explicit `lastError`) on that node.

### The medium must be configured on some node

The worker runs on every node, but each node only owns egress for the
media it carries an allowlist for. A node with **no** allowlist for a
row's medium leaves the row `pending` and skips it, so a peer that does
carry the allowlist can pick it up (memql#2540). The consequence: if a
medium is configured on **no** node at all, its rows sit in `pending`
rather than failing loud. The fix is operator-side -- set the medium's
allowlist (`MEMQL_OUTBOUND_EMAIL_ALLOWLIST` /
`MEMQL_OUTBOUND_WEBHOOK_ALLOWLIST`) on the node type that should own that
egress. Audit with `outboundRequestsByStatus(status: "pending")`: rows
lingering there past the poll interval mean no node is configured for
their medium.

## Delivery semantics

- **At-least-once.** A crash between transport acceptance and the
  `sent` stamp redelivers on the next drain. `dedupeKey` rides to the
  receiver (webhook headers `X-Memql-Outbound-Id` /
  `X-Memql-Dedupe-Key`) so external systems can deduplicate.
- **Single-runner per attempt.** Multi-replica deployments claim each
  (row, attempt) through the automation cluster guard ledger; exactly
  one replica delivers a given attempt while its claim is live.
- **Crash recovery (no wedge).** If the replica that won a claim dies
  before stamping a terminal status, the row would otherwise sit `pending`
  with a persisted claim no peer can re-win until the guard's retention
  prune (1h). The claim carries a per-attempt lease
  (`MEMQL_OUTBOUND_CLAIM_TTL_SECONDS`, default 5m): once the orphaned claim
  ages past it, a peer re-claims the same attempt and recovers the row on
  the next drain. Set the lease above `MEMQL_OUTBOUND_HTTP_TIMEOUT_SECONDS`
  so a slow-but-alive delivery is never re-claimed underneath itself; a
  re-take under an extreme stall simply redelivers (at-least-once).
- **Bounded retries.** Retryable failures (timeouts, connect errors,
  HTTP 408/429/5xx) back off exponentially (30s base, x4, 1h cap,
  jittered) up to `MEMQL_OUTBOUND_MAX_ATTEMPTS`; other 4xx and policy
  refusals fail permanently. Operators can requeue a failed row by
  setting `status` back to `"pending"` via
  `updateOutboundRequestStatus`.
