# memQL auth threat model

**Status:** v1, written 2026-05-20 as part of the Wave 3 security audit (see #86). Companion to [access-model.md](access-model.md), [caller-envelope.md](caller-envelope.md), [identity-service.md](identity-service.md), and [per-row-authz-audit.md](per-row-authz-audit.md).

This document inventories the auth surfaces, the threats considered, the defenses in place, the trust assumptions, and the known limitations. It is intended to be the single place a reviewer can read to understand what memQL's auth model *is* — both its enforcement and its boundaries.

---

## 1. Inventory of auth surfaces

### 1.1 gRPC — `MemqlService.Stream`

Primary client surface. Multiplexed bidirectional stream carrying every product-facing message type (chat, speech, transcription, suggest, polyphon, voice-agent, concepts, guest invitations).

**Interceptor stack** (`app/transport.go`, applied innermost→outermost):

1. `verifier.StreamInterceptor()` — JWT signature validation via the per-node JWKS verifier.
2. `NewSessionRevocationStreamInterceptor()` — rejects tokens whose hash matches a revoked `v1:identity:authSession` row.
3. `NewGuestAwareStreamInterceptor()` — admits `Authorization: Guest <token>` against the invitation registry.
4. `NewOperatorAwareStreamInterceptor()` — admits `Authorization: Operator <MEMQL_MASTER_KEY>` for bootstrap.
5. `NewVoiceAgentStreamInterceptor()` — surface-pinned shared-secret auth (`mql_va_<secret>`) for the voice-agent process.
6. `NewWorkerAwareStreamInterceptor()` — surface-pinned worker-token auth for `WorkerService.Stream`.

Authorization inside handlers is per-row, enforced by the DSL query/mutation layer.

### 1.2 gRPC — `NodeService.Stream`

Inter-node mesh. Same interceptor stack as `MemqlService.Stream`. Used for `NodeHello`, `NodeHeartbeat`, `PeerIntroduction`, `EventForward`, `QueryForward`, `AiForwardRequest`, `WorkbenchForwardRequest`.

**Trust assumption:** any binary that holds a valid identity-issued JWT is treated as a peer. See §5.1.

### 1.3 gRPC — `WorkerService.Stream`

Computer-use surface. Surface-pinned to worker tokens (`mql_wkr_*`). A worker token presented on any non-`WorkerService` path returns `PermissionDenied`; a Bearer JWT on `WorkerService` returns `Unauthenticated`.

Worker tokens are revoked by flipping `active=false` on the identity row; expiry is enforced at admit time as of #97.

### 1.4 HTTP

| Path | Method | Auth | Notes |
|---|---|---|---|
| `/healthz` | GET | none | intentional public |
| `/.well-known/jwks.json` | GET | none | intentional public; key distribution |
| `/auth/login` | GET, POST | none (entry point) | magic-link request |
| `/auth/magic-link` | POST | none (entry point) | magic-link mint |
| `/auth/complete` | GET | none (link consume) | magic-link redeem |
| `/oauth/token` | POST | code | OAuth-style code exchange |
| `/auth/refresh` | POST | Bearer | refresh-token rotation |
| `/auth/logout` | POST | Bearer | session revocation |
| `/pair/codes` | POST | Bearer | worker-pair mint; **HTTPS-required** (F5) |
| `/pair/redeem` | POST | Pair code | worker-pair redeem; **HTTPS-required** (F5) |
| `/me/*` | GET | Bearer | user-self web UI |
| `/setup` | GET, POST | bootstrap gate | one-time owner-creation flow |
| `/spaces/{id}/attachments` | POST | Bearer + explicit space-owner check (F10) | file upload; ownership re-enforced by DSL mutation |
| `/api/concepts` | GET | Bearer | concept schema feed (intentionally readable to any authenticated caller) |

### 1.5 WebSocket

| Path | Auth | Notes |
|---|---|---|
| `/memql/ws` | Bearer JWT (header or `?token`); `Guest` (header or `?guest_token`); `Worker`; `Operator` | proxies to `MemqlService.Stream` |
| `/memql/audio` | Bearer JWT or Guest token | Polyphon audio |

**Origin policy (F3):** the WebSocket bridge consumes `MEMQL_WS_ORIGIN_PATTERNS` (comma-separated glob list) and passes it to `nhooyr.io/websocket`'s `AcceptOptions.OriginPatterns`. When unset, the handler falls back to the legacy wildcard and emits a WARN log every upgrade. Production deployments must populate this; dev environments can leave it for the wildcard fallback.

### 1.6 Identity primitives

- **JWT** — EdDSA pinned at both sign (`jwt.SigningMethodEdDSA`) and verify (`jwt.WithValidMethods(["EdDSA"])`). `alg: none` impossible. Keys generated at bootstrap, stored encrypted on `v1:identity:key` rows; public JWKS published at `/.well-known/jwks.json`. Per-node verifiers refresh JWKS on a 5-min background loop + on-demand for unknown `kid`.
- **PAT (`mql_pat_*`)** — SHA-256 hash persisted, plaintext shown once at mint. Revocable via `revoked_at`. Same claims shape as JWT.
- **Worker tokens (`mql_wkr_*`)** — SHA-256 hash persisted, surface-pinned, scoped to a single user, `Active=false` for revocation, `ExpiresAt` enforced at admit (#97).
- **Guest invitations** — token plaintext used to construct the `/join/<token>` URL emailed to the invitee. Server-side, the invitation row stores both the hash (used for fast lookup) and the plain token (used to reconstruct the URL on resend; see §5.4).
- **Cookies (`memql_auth`)** — `HttpOnly`, `Secure` (when `X-Forwarded-Proto: https`), `SameSite=Lax`.
- **Session revocation** — token-hash lookup on `v1:identity:authSession`. Stream-open check only; **fails open** on engine unavailability with a WARN log. See §5.3.

---

## 2. Threats considered

For each surface above, the audit walks this checklist. Findings tracked on #86.

| Threat | Defense |
|---|---|
| Unauthenticated request reaches an authenticated endpoint | Interceptor stack applied uniformly; per-row authz inside handlers |
| JWT downgrade (`alg: none`) | Explicit `WithValidMethods(["EdDSA"])` at both sign and verify |
| JWT forgery | Asymmetric EdDSA; private key never leaves the identity binary |
| Token replay after logout | Session revocation interceptor (see §5.3 for limits) |
| Cross-tenant read (IDOR) | Per-row authz classification, validator hard-fails on new unclassified |
| Cross-tenant write | Same per-row authz, enforced on `mutationX` |
| Worker-token escape to non-worker RPC | Surface pinning at the worker interceptor (rejected with `PermissionDenied`); #97 added regression tests |
| Voice-agent token used on non-voice RPC | Surface-pinned `VoiceAgent*` payload-type gating; `subtle.ConstantTimeCompare` on the secret (F4) |
| Magic-link replay | Single-use, hash-stored on `v1:identity:magiclink`, expiry enforced |
| Refresh-token reuse | One-time-use refresh tokens, rotated on every `/auth/refresh` |
| Cross-origin WebSocket smuggling | Origin allow-list (F3, configurable via `MEMQL_WS_ORIGIN_PATTERNS`) |
| Plaintext credential on the wire (pair codes) | `requireSecureRequest` on `/pair/codes` and `/pair/redeem` (F5) |
| Credential leak via claims-map logging | Guest interceptor stores `tokenHash`, not plain token (F9 in #98); worker identity claim mirrors the hash-only pattern |
| Privilege escalation through delegation | Delegation resolver is interface-only today; see §5.6 |

---

## 3. Authorization model

memQL's authorization model is **per-row, classified, and tested at load time.** Every query/mutation in the DSL falls into one of four buckets:

| Bucket | Filter shape | Example |
|---|---|---|
| **Owned** | `payload.ownerUserId == actor.userId` | `queryActiveSpaces`, `queryOwnedSpaceById` (F10) |
| **Granted** | relationship predicate gates on `actor.userId` | `querySpaceParticipants` |
| **Admin** | `spec("requiresClusterOwner")` | admin-only mutations |
| **Public** | `@public` annotation | `/.well-known/jwks.json`, `/api/concepts` schemas |

The conformance test `dsl.TestPerRowAuthzClassification` hard-fails on any new unclassified construct, so the classification doesn't drift.

---

## 4. Defenses landed by Wave 3

| # | Surface | Defense |
|---|---|---|
| **F8** | `WorkerService.Stream` | Reject tokens whose `ExpiresAt` is set and elapsed. Tests in `worker_stream_interceptor_test.go`. (#97) |
| **F9** | Guest interceptor | Stow `tokenHash` (sha-256 hex) on claims instead of the plain token; handler re-reads hash, not plain. (#98) |
| **F10** | `/spaces/{id}/attachments` | Explicit caller-owns-space check via `queryOwnedSpaceById` before any GCS upload. DSL mutation re-enforces; this is defense in depth. (Wave 3 closeout) |
| **F3** | `/memql/ws` WebSocket bridge | Configurable origin allow-list via `MEMQL_WS_ORIGIN_PATTERNS`; legacy `*` fallback emits a WARN log every upgrade so misconfigurations surface. (Wave 3 closeout) |
| **F5** | `/pair/codes`, `/pair/redeem` | `requireSecureRequest`: reject HTTP-only requests; dev escape hatch via `MEMQL_IDENTITY_ALLOW_INSECURE_PAIR=1` (logs WARN every use). (Wave 3 closeout) |

---

## 5. Trust assumptions and known limitations

Each of the items below is a place where the audit explicitly *chose* a tradeoff. They are surfaced here so future contributors understand the model rather than discovering it accidentally.

### 5.1 Inter-node mesh trust (F1)

`NodeService.Stream` validates the inbound JWT but does **not** require a per-node service-account credential. Any binary that holds a valid identity-issued JWT is treated as a peer and may call `NodeHello`, `PeerIntroduction`, `EventForward`, `QueryForward`, `AiForwardRequest`, or `WorkbenchForwardRequest`.

**Trust assumption:** the cluster runs inside a trusted network boundary (private VPC, Kubernetes namespace, mTLS-terminated load balancer). An attacker who can reach `NodeService.Stream` from outside the boundary AND who holds a valid JWT is by design out of model — the JWT alone shouldn't be enough to reach this surface.

**Upgrade path:** introduce a per-node service-account claim (`identity.node`) issued at provisioning time and required by the `NodeService.Stream` interceptor; existing user JWTs would be rejected at this surface. Pending; tracked separately.

### 5.2 Voice-agent shared secret (F4)

The Python voice-agent authenticates via `MEMQL_VOICE_AGENT_SHARED_TOKEN`. The interceptor uses `subtle.ConstantTimeCompare` for the comparison (timing-attack resistant). Rotation requires a coordinated restart of the voice-agent + the memQL binaries that hold the env var.

**Trust assumption:** voice-agent is a single-tenant, single-instance, internal service. Multi-tenant voice-agent deployments would need a service-account JWT path (same shape as §5.1) instead of a shared secret.

**Upgrade path:** mint a `voice_agent` identity type with a JWT lifecycle the cluster can rotate without a restart.

### 5.3 Session revocation on long-lived streams (F7)

Session revocation is checked at stream-open time only. A bearer token revoked mid-stream continues to work until the underlying JWT expires (default ~1 hour for browser flows, shorter for API clients).

**Trust assumption:** JWT short-expiry is the primary defense against compromised credentials; revocation is the secondary defense that closes the gap between "revoke" and "natural expiry."

**Trade-off:** per-message revocation would require a DB round-trip per RPC. Acceptable for low-rate calls; unacceptable for the streaming-transcription path where messages are sub-second.

**Upgrade path:** add a `revocation_epoch` claim to the JWT. The verifier checks the epoch on every stream-open AND on a periodic refresh; bumping the epoch invalidates every token issued before it. Pending; tracked separately.

**Session revocation lookup fails open** when the engine is unavailable, with a WARN log. JWT expiry is the underlying guarantee.

### 5.4 Plain guest token at rest (F11)

`v1:identity:invitation.token` persists the plain guest-invite token on the invitation row. The concept schema's own description notes the choice:

> Plain guest-invite token (guest kind only). Persisted so `resend email` sends the same `/join/<token>` URL without minting a new invitation. Acceptable because invites are short-lived and scope-limited.

**Trust assumption:** invitations are short-lived (TTL default 7 days, capped per `invitationTTLDays` config), scope-limited (read-only access to one space), and rotated when accepted. A DB read leak recovers an unaccepted invitation only.

**Upgrade path:** mint a fresh token + hash on each `ResendGuestInviteEmail`; the old `/join/<token>` URL becomes invalid. Has a UX cost (a recipient who clicks an older link after a resend gets a 404). Product decision before behaviour change.

### 5.5 Operator master key (F6 — operator path, not CSRF)

`MEMQL_MASTER_KEY` admits `Authorization: Operator <key>` for bootstrap before any users exist. The key is read from env at startup, so it lands in `/proc/self/environ` and any `env`-dumping diagnostic.

**Trust assumption:** the operator path is used only during initial cluster bootstrap and for break-glass admin tasks. Rotate the key after the first admin user is created; keep it injected via the secrets manager, never via plain env.

### 5.6 Delegation model (F2)

`component/auth/delegation_resolver.go` is interface-only today. `DelegationContext` (the resolved payload) is defined in the identity package; the resolver is invoked by the auth middleware after JWT validation. No tests exercise the delegation flow.

**Status:** the surface exists for an "agent acts on a user's behalf" pattern but no in-tree implementation populates a delegation. Until a concrete delegator ships, the surface is a no-op.

**When implementations land:** every delegation must carry a bounded role, scope, and lifetime. The resolver MUST reject delegations whose role exceeds the delegating user's role; the scope MUST narrow the delegating user's scope; the lifetime MUST be shorter than the delegating user's session.

### 5.7 CSRF on web form POSTs

`/auth/login` (POST), `/setup`, and `/auth/complete` are unauthenticated entry points (no Bearer required) and do not carry CSRF tokens. SameSite=Lax on `memql_auth` partially mitigates by blocking the cookie from cross-site POSTs, but the underlying forms accept any well-formed POST.

**Threat:** the impact of CSRF on these forms is limited because the side-effects are:
- `/auth/login` POST → mint a magic link → send to the *form-supplied* email address. The attacker who triggers this has no way to read the resulting email; the magic-link goes to a real user's mailbox.
- `/setup` POST → bootstrap-only; runs at most once.

**Trust assumption:** the unauthenticated entry-point surface does not warrant a full CSRF framework today. If authenticated form POSTs are added (admin console etc.), introduce a `gorilla/csrf`-style middleware first.

---

## 6. Test coverage

Each defense in §4 lands with negative tests:

- **F8** — `component/grpc/worker_stream_interceptor_test.go` (8 cases: active+unexpired admitted, revoked rejected, expired rejected, non-expiring admitted, surface pinning both ways, fallthrough, unknown-token).
- **F10** — Wave 3 closeout PR adds an integration-style test asserting cross-tenant attachment upload is rejected with 404 before any GCS write.
- **F3** — env loading test asserts CSV parsing; handler test asserts disallowed origins return 403 from `websocket.Accept`.
- **F5** — `pair_https_test.go` asserts plaintext requests are rejected with `errorCode: insecure_transport` and that `MEMQL_IDENTITY_ALLOW_INSECURE_PAIR=1` admits with a WARN.

**Not yet tested (gaps tracked for follow-up):**
- Guest interceptor negative pack — requires refactoring `lookupInvitationByTokenHash` to be stubbable.
- WebSocket lifetime / JWT-expiry-mid-session.
- Cross-node forged-peer scenarios (see §5.1).

---

## 7. Injection surfaces (Wave 4, #87)

The injection-surface audit (Wave 4, issue #87) walked the standard injection vectors against memQL. Most are **closed** by existing design; two are deferred as architectural follow-ups.

### 7.1 SQL injection — closed

Every runtime query goes through Bun ORM with parameterized binds. The DSL → SQL filter compiler (`component/memql/executor_filter.go`) compiles `payload.field == args.x` filter clauses to `?`-placeholder SQL with bound args. The audit found one `fmt.Sprintf` site, in `component/database/timescaledb.go:182-183`, but it interpolates the table name (a static, hardcoded constant — `memoryNodesTableName`) through `quoteIdentifier()` and never touches user input.

### 7.2 DSL execution sandboxing — strong

The MemQL parser (`component/language/parser/parser.go`) is strict: unknown receivers and unknown step types are rejected at parse time. The DSL has no `eval()` / reflection escape hatch — every external call goes through a registered integration, and integrations are Go code under operator control, not declared in `.memql` files.

`MEMQL_DSL_PATH` overrides let an operator load DSL from disk; the loader (`component/memql/dslfs/dslfs.go`) doesn't validate beyond what the parser does. **Trust assumption:** `MEMQL_DSL_PATH` is operator-controlled (env var injected at startup); the cluster owner can already replace any DSL on disk, so the override doesn't widen the trust boundary.

### 7.3 Prompt templates — low risk

`component/memql/si_prompts.go` uses Go's `text/template` with an empty `FuncMap` — no custom functions, no dangerous primitives like `call`/`env`. Templates themselves are operator-controlled (in `dsl/**/prompts/`); user data is passed as values, not template syntax. A malicious `.tmpl` loaded via `MEMQL_DSL_PATH` could exfiltrate fields from the data object, but `MEMQL_DSL_PATH` is operator-controlled (see §7.2).

The **BFF-side** prompt templates have their own injection surface (LLM data flowing into user-facing prompts); that's handled in `memql-bff-copresent` #15 and the `[[BEGIN UNTRUSTED * ]]` framing.

### 7.4 Path traversal — closed

`integrations/workbench/workspace.go:197-203` (`safeJoin`) does the canonical clean-and-prefix-check pattern. `safePlanId` whitelists allowed chars in the plan-id component. Every fs operation in the workbench goes through `safeJoin`. The DSL loader operates on operator-controlled directory trees only.

### 7.5 Web injection — closed

Templ auto-escapes `{ field }` interpolations. `templ.Raw(...)` is only used for markdown-rendered HTML where content was sanitized upstream. OAuth `redirect_uri` is whitelisted via `Cfg.AllowsRedirectURI` (`component/identity/web/handlers.go:711-712`); arbitrary destinations are rejected.

### 7.6 Tool-call argument validation — deferred (architectural)

`component/memql/si_tool_loop.go:149-158` looks up the tool by name and rejects unknown names, but **does not validate args against the tool's declared schema** before invoking the handler. Each tool handler is expected to validate its own args.

This is by design — every tool defines its own contract — but it leaves a class of bug where a handler that forgets to check a sensitive arg (e.g. `forUserId` or `actor.userId`) can be exploited via LLM-supplied values.

**Trust assumption today:** every tool handler validates its own args. The audit didn't find a concrete violation, but the surface is unprotected at the central dispatch point.

**Upgrade path:** generate a centralised arg-validator from the DSL tool schema (`tools.memql`) and intercept in `si_tool_loop` before dispatch. Rejects unknown arg names, wrong types, out-of-enum values, and (most importantly) auto-injected fields (like `ownerUserId`, `agentId`) that the LLM should never supply. Pending; tracked on #87.

### 7.7 Shell exec — intentional, mitigated, allowlist deferred

`integrations/workbench/dispatch.go:96` invokes `/bin/sh -c <cmd>` where `cmd` is supplied by the calling agent. This is the **workbench tool's reason for existing**: a sandboxed per-Plan workspace that an agent can drive via shell. Mitigations in place:

- Plan-scoped workspace root + `safeJoin` blocks fs escape.
- 60-second default execution timeout, 1 MiB output cap.
- Workspace runs as the host process user (no privilege escalation).

**Trust assumption:** the agent is the trust boundary. An agent compromised via prompt injection (see #15) or jailbroken via base-model manipulation can run arbitrary shell commands within the workspace. The kill-switch + plan-level token budgets bound the blast radius.

**Upgrade path:** per-command allowlist (`find`, `grep`, `cat`, `python`, `node`, etc.) or seccomp/AppArmor profile around the exec. Pending; tracked on #87.

---

## 8. Future hardening (ordered)

1. **Per-node service-account JWT for `NodeService.Stream`** — closes §5.1.
2. **Revocation epoch claim** — closes §5.3.
3. **Centralised tool-call arg validator** — closes §7.6.
4. **Rotate-on-resend for guest invitations** — closes §5.4.
5. **Voice-agent service-account path** — closes §5.2.
6. **Workbench exec allowlist or seccomp profile** — closes §7.7.
7. **CSRF framework if any authenticated form POSTs ship** — closes §5.7.

Each is tracked as a follow-up beyond Wave 3 / Wave 4 closeout.
