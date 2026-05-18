# memQL Roadmap

Future-work tracker. Items here are deliberately deferred to keep landed
work shippable -- not "we'll never do this." Update entries as scope gets
clarified or work moves into a planning doc under `docs/planning/`.

---

## Partition model (current state, for context)

Two concept scopes exist today:

- **Partition-scoped (default)**: concepts without a scope annotation.
  Rows carry the envelope's partition; reads/writes/events are all
  stamped with it; the engine adds `WHERE partition = $envelope`
  automatically. This is the tenant isolation primitive.
- **Global (`@scope("global")`)**: concepts annotated as global.
  Rows live in the reserved `_system` partition regardless of
  envelope; queries against them ignore envelope partition. Used for
  infrastructure metadata that must be visible to every tenant:
  `v1:cluster:node`, `v1:cluster:nodeType`, `v1:cluster:spawnEvent`,
  `v1:cluster:cluster`, `v1:cluster:database`,
  `v1:cluster:identityProvider`, `v1:platform:partition`.

The items below build on this model.

---

## Multi-tenancy & Auth

### Partition-aware auth -- DONE (April 2026)

Shipped. See [docs/auth/access-model.md](auth/access-model.md) for the
model.

Summary of what landed:

- `v1:identity:user` (the person), `v1:identity:identity` (a credential
  set owned by a user), and `v1:identity:partitionAccess` (the grant)
  are all global-scoped concepts. Unified role spectrum
  (`owner / admin / writer / reader`).
- The gRPC middleware (`component/auth/access/`) loads the caller's
  `PartitionACL` on the first message of a stream (via
  `identityBySubject` -> `userById` -> `accessForUser`) and rejects any
  request whose envelope partition is not in the ACL. Cluster-wide
  owners bypass. `_system` is never addressable on the wire.
- `listPartitions` is server-filtered by ACL. The Cockpit Settings tab
  shows the caller's "My Access" block (fetched via a dedicated
  `MyAccessMsg` RPC).
- User + identity + default-partition rows are created inline by the
  identity service's magic-link verifier (`Store.CreateUserOnFirstLogin`)
  at first-link consumption. First user (no existing owners) becomes
  cluster owner.
- Audit: rejected requests log subject / user_id / partition / reason
  at `Info` level.

Deferred (not blockers for SaaS safety, but worth following up):

- Writer/reader distinction inside a partition (mutations require
  writer+; reader is read-only). Enforce after the parser tells us
  "this query resolves to a mutation".
- External-group -> partition-access sync (e.g. SCIM, SSO group
  claims). The model accommodates it via `partitionAccess.source`
  and `sourceRef`; the sync is future work.
- Admin grant UI in Cockpit. Today granting access goes through
  `grantPartitionAccess` from the API; Cockpit Settings only shows
  the caller's own grants (read-only).

---

## Partition tooling

### Hard-delete partition

The CLI's "delete" today is a soft delete -- it writes a `status:
"draining"` row and the partition disappears from the list. The
underlying `MemoryNodes` rows (whose composite PK includes the partition
string) are left untouched. We need an admin-only path that:

- Confirms zero active references for the partition.
- Tombstones or copies the data out (TimescaleDB compression / S3
  archive).
- Drops the rows in batches so the deletion doesn't lock the table.

This is admin tooling, not a user-facing CLI feature.

### Per-partition config

`v1:platform:partition.config` is a free-form `object` field today. We
should formalize per-partition settings:

- Default SI provider (a customer might prefer Anthropic over OpenAI).
- Rate limits (per-partition token budget).
- Retention windows (how long before timeseries data ages out).
- Allowed integrations (some customers can't use third-party storage).

Editable from the CLI partition form once the schema settles.

### Subscription partition scoping

`handleSubscribe` currently extracts the partition from the envelope and
scopes the subscription accordingly (TIER 2 in the partition research).
This works but isn't enforced -- a subscriber that omits the partition
sees everything for "default". Tighten:

- Reject subscribe requests that don't specify a partition (no
  fallback).
- Document the wire contract in `docs/core/events.md`.
- Surface the partition on every event payload so consumers can sanity
  check.

---

## Workbench production deployment

### Cut the workbench over to its own Cloud Run node

The workbench capability (the sandboxed per-Plan Linux environment
agents drive for headless work) ships in two modes. The MVP path
runs the workbench in-process on the agent node and is the active
default; the cluster-mode path runs the dedicated `workbench`
node-type binary (`make workbench`) and routes via
`NodeService.Stream` to a Cloud Run service backed by GCS-FUSE.

The cluster-mode code is committed and tested -- builds, proto,
routers, handlers, service yaml, docker-compose entries all in
place -- but is not yet active anywhere. Cross this bridge when
the production cutover lands. Step-by-step plan in
[docs/workbench/production.md](workbench/production.md): bucket
provisioning, image build, deploy, agent env flip, rollback.

Items inside this work that may need decisions when picked up
again:

- **Base image choice.** Alpine vs distroless+busybox vs
  Ubuntu-minimal. Preinstall set (`curl`, `git`, Python, Node)
  must match what the workbench knowledge corpus promises agents
  will find.
- **Per-Plan disk quota.** The current design has global size
  caps but no per-Plan quota; a runaway agent could fill the
  bucket.
- **`http_fetch` egress policy.** Today unrestricted; pre-prod
  decision needed on allowlist vs deny-by-default + agent opt-in.
- **Audit telemetry shape.** No equivalent of
  `v1:worker:invocation` for workbench calls; decide whether
  the sandbox needs the same per-call logging or just aggregate
  metrics.
- **Frontend visibility.** No UI for inspecting Plan workspaces
  today. Add a Plan-detail pane if user feedback demands it.

## Voice pipeline

### Replace Python voice-agent with a revived Go voice agent

The realtime voice + video channel is currently owned by the Python
`voice-agent/` process (LiveKit Agents 1.5). This is a deliberate
short-term choice; the long-term plan is to replace it with a Go
implementation owned by memQL core, eliminating the only non-Go
runtime in the tree and pulling the voice path back behind the same
build-tag model the other node types use.

**Revival source.** The previous Go voice agent (then called the
Polyphon Bridge Agent) was retired in commit `1af0c60` -- "voice:
Phase 11 -- retire the Go Bridge Agent (Initiative C)" -- when
the Python voice-agent shipped as the sole voice participant for
the General Assistant. The full implementation is recoverable from
that commit's parent:

- `cmd/bridge-agent/` -- the binary entry point + internal latency
  harness.
- `component/polyphon/bridge/` -- the `bridge.Bridge` implementation
  (room join, STT/TTS dispatch, sentence streaming).
- `docker/bridge-agent.Dockerfile` + bridge-agent service in
  `docker-compose.polyphon.yml`.
- `infra/polyphon/k8s/bridge-agent/` k8s manifests.
- Makefile targets `bridge-agent`, `voice-latency-harness`,
  `voice-loop-test-deepgram`, `voice-loop-test-openai`.
- `scripts/voice/{harness-run,loop-test-deepgram,loop-test-openai}.sh`.
- `POLYPHON_BRIDGE_AGENT_URL` env wiring + `MEMQL_VOICE_TRANSPORT`
  toggle in every binary that previously cared.
- `SetBridgeAgentURL` / `SetVoiceTransport` on the gRPC Server +
  `bridgeAgentURL` / `voiceTransport` fields.
- `notifyBridgeAgent` helper in `polyphon_handlers.go`.
- `CognitionIntegration.bridgeAgentURL` + `WithBridgeAgentURL` ctor
  option + `notifyBridgeAgentResponse` / `Sentence` cluster
  (`capabilities.go` `forwardToBridgeAgent` capability +
  `cognition_handler.go` TTS dispatch path +
  `voice_streaming.go` sentence-stream dispatch path).
- `queries/v1/common/builtin/builtinConductorForwardBridgeAgent.memql`.

**What changed since retirement that the revival must absorb:**

- LiveKit Agents 1.5 patterns the Python agent introduced (STT
  endpointing, token-streamed TTS, avatar gating via
  `audioControl` / `videoControl` overrides). The revived Go agent
  must match the avatar gating behavior and per-(space, agent)
  override resolution that ships today.
- The canonical voice catalog (`integrations/voice/voices.go`)
  and the `respondToUser` envelope (`integrations/agent/envelope.go`)
  -- both already in core, both consumed by the current voice path,
  both must continue to be the source of truth.
- Per-agent audio + video control + avatar persona fields on
  `v1:agents:agent` -- the revived Go agent reads these the
  same way the Python agent does today.

**Why we're not doing it now:** Python's LiveKit Agents 1.5 SDK
landed faster than rewriting the equivalent in Go, and the voice
path is product-critical. Defer until the surface stabilizes and
the Anam / Simli avatar plugin behavior is well-understood; revive
from `1af0c60`'s parent, port forward, delete `voice-agent/` in the
same commit.

**Repo split implication:** once the Go voice agent is back, the
voice node folds into the same module boundary as the other node
types (bff / cognition / agent / planner) -- no separate Python
repo, no separate Dockerfile family.

---

## Calendar & agenda

### User tasks / todos with due dates

Deferred from Calendar v1. Calendar events are time blocks ("I will
be at X from 2-3pm"); user tasks are commitments with deadlines
("I need to do X by Friday"). They render together in the UI but
have different schemas: tasks carry `status`, `completedAt`,
`priority`, optional dependencies, sometimes no time at all.
Folding them into `v1:calendar:event` via a `kind=task` discriminator
means every query/handler/prompt has to branch on kind, and the
naming collides with the existing `v1:planner:task` (Plan-execution
steps).

Land as its own concept tree -- name TBD but likely `v1:agenda:*`
or `v1:todo:*`; avoid `v1:tasks:*` to prevent the `v1:planner:task`
collision.

When it ships, the calendar agent tool grows a combined "agenda"
query that unions calendar events + due-today tasks at the tool
layer. Stored separately; presented unified.

---

## Closed notes (reference only)

The following carry-over notes from prior iterations are either shipped
in the current tree or superseded; kept here so the decision trail is
discoverable:

- **Server-side turn-continuation guard** -- the idea was to auto-inject
  a `[SYSTEM: continue the walkthrough without waiting for user input]`
  hint after the stream completes with an active session and no pending
  ask or release. Not implemented: the streaming loop instead uses a
  bounded iteration cap plus an explicit `WHEEL_CONTESTED` exit
  (`integrations/agent/streaming.go`), and the commit-confirmation
  pattern in the prompt keeps multi-step walkthroughs moving without
  the auto-continue. Reintroduce only if a real stall pattern emerges.
- **DSL arg drop for scalar int under additionalProperties=true** --
  fixed. The root cause was the DSL parser producing int64 while
  handlers only checked int/float64; accepting int/int64/int32/float64/
  float32 at the handler boundary closed it, and the replier's
  `chunks[:wantK]` client-side trim is gone. Retained as a defensive
  widening at the handler; no engine-side parser change was needed.
- **Engine reorders Bundle.Nodes from handlers** -- fixed via
  `IntegrationCapability.PreserveOrder`. The engine stamps monotonic
  `CreatedAt` on the returned slice so the default downstream sort
  reproduces handler order. Both agent-reply and delegate-takeover re-
  sorts are removed.
- **First-class pgvector DSL operator (`similarTo`)** -- shipped as
  `integrations/similarity`. Generalised over target concept; any
  vector-indexed concept is reachable. `knowledge.lookup` capability
  + `knowledgeLookup` builtin removed.

---

## Nice-to-have

- **CLI: cluster-level partition stats**. Show row counts and last
  activity per partition in the partition manager detail pane. Requires
  a cheap aggregate query.
- **CLI: bulk partition migration tool**. Move all rows from one
  partition to another (renames a customer, splits a noisy tenant).
  Pairs with hard-delete.
- **Auth: per-partition write-vs-read enforcement**. Per-partition
  grants already exist (`v1:identity:partitionAccess.role`), but the
  middleware today only checks "does the caller have *any* grant for
  this partition". Tighten it to block readers from running mutations
  and block writer+/admin-scoped mutations from reader grants.
