# Epic 4 — Telephony into core

Inbound + outbound PSTN calling for MemQL voice agents, via self-hosted
`livekit/sip` + a carrier-agnostic `CarrierProvider` abstraction (Telnyx
first), driven by the OpenAI Realtime voice path. **No Twilio.**
**Session: S4. Starts at G3.**

**Repo:** `memql`.

The detailed issue drafts live in
[`/telephony-issues.md`](../../../telephony-issues.md) (8 dependency-ordered
issues). This epic file records the **two amendments** required because it now
lands on the decoupled, renamed core.

## Amendment A — attach to a partition/room, NOT a CoPresent `space`
The original drafts referenced `polyphon-<spaceId>` rooms. After Epic 3, core
has no `space`. Telephony is **core**, so:
- Inbound dispatch maps a DID → a generic **partition/room**, resolved via
  `v1:telephony:number.partitionId` (not `spaceId`).
- Outbound `place_call` attaches the SIP participant to a partition-scoped room.
- The CoPresent product may *layer* its `space` on top (a space maps to a
  partition), but that binding lives in the CoPresent pack, never in the
  telephony core.

Update issues #2 (`v1:telephony:{number,trunk,call}` → `partitionId`), #4
(inbound routing → partition), #5 (outbound → partition-scoped room).

## Amendment B — AI naming
All telephony code uses the post-Epic-1 vocabulary (`ai()`, `*AIProvider`,
renamed wire names). No `si`-named identifiers introduced.

## Cost model (unchanged, validated)
Worst case ≈ $0.11/min outbound, $0.12/min inbound; OpenAI Realtime audio is
~90% of per-minute cost. Silence is ~free with VAD gating. Full model in
[`/telephony-sip-integration-plan.md`](../../../telephony-sip-integration-plan.md)
(if copied into the repo) or the program handoff.

## Why last
Depends on **G3**: a clean, partition-based core so telephony binds to the
generic primitive and inherits none of the CoPresent coupling.

---

## Issue list (from telephony-issues.md, with amendments)
1. CarrierProvider abstraction + Telnyx impl
2. `v1:telephony:{number,trunk,call}` concepts — **partitionId** (Amendment A)
3. Self-host `livekit/sip` (K8s)
4. Inbound: trunks, dispatch rules, **partition/room** routing (Amendment A)
5. Outbound + agent tools (`place_call`…), **partition-scoped room** (Amendment A)
6. Programmatic DID provisioning (owner/admin gated)
7. Cost controls & call observability (VAD silence-gating, caching)
8. Compliance guardrails (caller-ID/STIR-SHAKEN, E911, TCPA, KYC)
