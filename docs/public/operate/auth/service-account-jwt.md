---
title: Service-account JWTs (machine identity)
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Service-account JWTs (machine identity)

A **service-account** JWT is the machine identity for automation and synthetic
checks — most importantly the in-cluster **deploy gate** (the Argo Rollouts
`AnalysisTemplate`, deployment-v2 Phase 3). It authenticates to memQL's
`MemqlService.Stream` on the **BFF / mesh** surface, where a PAT cannot.
Resolves [#691](https://github.com/znasllc-io/memql/issues/691); part of epic
[#697](https://github.com/znasllc-io/memql/issues/697).

## Why this exists (the #691 problem)

The deep-smoke gate needs to run a **real authenticated query** against the BFF
(`/memql/ws` → `MemqlService.Stream` → cognition/agent) to prove the
authenticated app path actually works. The obvious credential — a PAT
(`mql_pat_…`) — **does not work there**:

```
  PAT verification is wired ONLY on the identity node.
  Every other node's per-node verifier is built with a nil PATVerifier:

      app/config.go:  verifier.New(cfg, cache, /* PATVerifier */ nil, logger)
                                                 └── deliberate: keeps token
                                                     verification DB-free on
                                                     the hot path.

  So:  PAT ──► identity node      = OK (DB lookup of the PAT hash)
       PAT ──► BFF / cognition…   = 401  "PAT path not wired on this node"
```

A JWT, by contrast, is verified by the **JWKS path** that *every* node already
runs (signature + claims, no DB). So the fix is a JWT-shaped machine credential
— **not** wiring PAT verification onto every API node (which would add a DB
round-trip to the hot auth path and broaden where a long-lived static user
credential is honored). PATs stay the **human-CLI** credential; service-account
JWTs are the **machine** credential.

## The credential-class family

memQL stamps a `class` claim on identity-issued JWTs; each class is admitted by
a dedicated interceptor that **pins the surface** it may use. Service-account
joins `node` (#105) and `voice_agent` (#109) as a machine class.

```
                         identity service (Ed25519 signing key, JWKS published)
                                          │ mints
   ┌───────────────┬───────────────┬──────┴────────────┬────────────────────┐
   │ class=user    │ class=node    │ class=voice_agent │ class=service_account│
   │ (humans)      │ (#105)        │ (#109)            │ (#691, THIS doc)     │
   ▼               ▼               ▼                   ▼
 full app        NodeService.    MemqlService.Stream  MemqlService.Stream
 surface         Stream only     pinned to            pinned to the
 (per-row authz) (mesh)          VoiceAgent* msgs     read/query surface

   PAT (mql_pat_) ─ NOT a JWT ─► verifies ONLY on the identity node (DB lookup)

   Verify path for ALL JWT classes = the per-node JWKS verifier (NO DB):
        Authorization: Bearer <eyJ…>  ──►  verifier.VerifyBearer()  ──►  class
```

### Token shape

A service-account JWT is a regular identity-issued EdDSA JWT with:

| Claim | Value |
| --- | --- |
| `class` | `"service_account"` (the surface pin) |
| `sub` | A stable machine-principal id, e.g. `system:deploy-gate` (audit) |
| `node_id` | The instance **label**, e.g. `deploy-gate-staging` (reused field slot so the JWT shape stays uniform across classes; surfaces as `VerifiedClaims.NodeId`) |
| `exp` | Short by design — `DefaultServiceAccountTokenTTLSeconds` = **1 hour** |

Signed with the same EdDSA key as user JWTs, so the per-node verifier validates
it through the same JWKS endpoint. **No `v1:identity:identity` row is persisted**
(unlike `voice_agent`): the verify path is DB-free and the token is short-lived,
so *revoke = expiry / signing-key rotation*.

## Minting

`memql service-account-token mint` (on the **identity** binary,
`subcommand_service_account_token.go`, build tag `identity`).

```
  operator / CI                       identity binary (memql, -tags identity)
       │                                          │
       │  memql service-account-token mint \      │
       │     --label deploy-gate-staging \        │
       │     --subject system:deploy-gate \       │
       │     [--ttl 1h] [--out /path]             │
       ├─────────────────────────────────────────►│
       │                          1. ApplyLocalOverride(".")  (/.env if present)
       │                          2. app.Build + start config+db+engine+identity
       │                             (servers SKIPPED — no port bind; safe to
       │                              `docker exec` into the running identity pod)
       │                          3. svc.Issuer().IssueServiceAccountAccessToken(
       │                                 {Subject, Label, TTL})  — signs EdDSA
       │                          4. print JWT to stdout (or --out, mode 0600)
       │◄─────────────────────────────────────────┤
       │  eyJhbGciOiJFZERTQSIsImtpZCI6…            │  (stderr carries the logs;
       │                                          │   stdout is JUST the token so
       │                                          │   `TOKEN=$(… mint …)` is clean)
```

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--label` | *(required)* | Instance label, stamped on `node_id` for audit |
| `--subject` | `system:deploy-gate` | `sub` claim (machine principal) |
| `--ttl` | `1h` (`DefaultServiceAccountTokenTTLSeconds`) | Lifetime; no refresh path |
| `--out` | *(stdout)* | Write the JWT to a file at mode `0600` |

> The mint is unauthenticated by design — the operator is `docker exec`-ing the
> identity binary, which already holds the signing key. It binds no ports
> (`selectMintDependencies` drops the server deps), so it is safe to run inside
> the live identity pod.

## Verification + surface pinning (BFF / mesh)

On every API-serving node the interceptor chain (`app/transport.go`) now ends:

```
  base (JWKS verifier) → sessionRevocation → guestAware → operatorAware
       → voiceAgent → serviceAccount → panicRecovery → handler
```

The service-account interceptor
(`component/grpc/service_account_stream_interceptor.go`):

```
   Authorization: Bearer <jwt>
            │
            ▼
   ┌──────────────────────────────────────────────────────────┐
   │ reserved prefix (mql_pat_ / mql_wkr_)?  ── yes ──► base    │  (not a JWT)
   │ scheme != Bearer / empty / v==nil?      ── yes ──► base    │
   └──────────────────────────────┬───────────────────────────┘
                                   │ no
                                   ▼
                 v.VerifyBearer(token)   (JWKS — NO DB)
                                   │
              class == "service_account" && Source == JWT ?
                  │ no ──► base (user/voice-agent JWT for another surface)
                  │ yes
                  ▼
        admit + stamp system actor (role=system, class, sub, label)
                  │
                  ▼  per inbound MemqlClientMessage:
        ┌─────────────────────────────────────────────────────────┐
        │ isServiceAccountPayload(payload)?                         │
        │   ALLOW: ClientHello, Ack, Unsubscribe, CancelRequest,    │
        │          ExecuteQuery, Subscribe, ConceptsList,           │
        │          ConceptsSubscribe, MyAccess, EvaluatePolicy,     │
        │          AgentGenerateTurn                                │
        │   DENY (PermissionDenied): IdentityCreate/Update,         │
        │          CreateWorkerToken, DelegationCreate, RotateAuth, │
        │          SendGuestInvite, Revoke*, … every credential/    │
        │          admin mutation + other classes' message types    │
        └─────────────────────────────────────────────────────────┘
```

Two independent containments: (1) the **message-type allowlist** (a leaked
synthetic credential can run reads + one agent turn, never a credential
mutation), and (2) **per-row authz** still applies to whatever `ExecuteQuery`
runs. The allowlist — not the actor role — is the primary blast-radius bound.

## Use by the deploy gate (deployment-v2 Phase 3)

```
   Argo Rollout step ──► AnalysisTemplate (Job, IN-CLUSTER)
        env: MEMQL_SVC_JWT  ◄── k8s Secret (minted by an identity-side Job)
        │
        ▼  authenticated WS/gRPC, IN-CLUSTER (no public host, no firewall dep)
   bff:50058 (MemqlService.Stream)
        │  serviceAccount interceptor admits class=service_account, pins surface
        ▼
   ExecuteQuery / AgentGenerateTurn ──► cognition / agent   (proves the app path)
        │
        ▼  assert result + SLO metrics → pass/FAIL → Rollout promote/auto-abort
```

Because the check runs **inside the mesh** against service DNS, it is immune to
the public-host routing (#680) and rolling-convergence (#682) failure modes the
old shell smoke hit, and needs no firewall exception.

## Provisioning into the cluster

Deliver the JWT to the gate as a k8s Secret (consumed as `MEMQL_SVC_JWT`):

```bash
TOKEN="$(kubectl -n memql exec deploy/identity -- \
          memql service-account-token mint --label deploy-gate-staging)"
kubectl -n memql create secret generic deploy-gate-jwt \
  --from-literal=MEMQL_SVC_JWT="$TOKEN" --dry-run=client -o yaml | kubectl apply -f -
```

A short-lived identity-side `CronJob` re-mints on the TTL cadence (Phase 3
wiring); until then re-run the mint before a deploy. The token never lives in
git and never leaves the cluster.

## Rotation & revocation

- **Rotate:** mint a fresh token (and refresh the Secret). The 1-hour TTL means
  a stale token self-expires fast.
- **Revoke all at once:** rotate the identity **signing key** (re-seal the seed
  into the genesis envelope and roll identity, per
  [identity-service.md](identity-service.md)) — this invalidates *every*
  outstanding JWT of every class, so use it only for a true key-compromise.
- There is **no per-token revoke list** (no DB row by design). If you need
  individual revoke semantics, prefer a PAT on the identity surface instead.

## Class comparison

| | `user` | `node` (#105) | `voice_agent` (#109) | `service_account` (#691) | PAT |
| --- | --- | --- | --- | --- | --- |
| Shape | JWT | JWT | JWT | JWT | `mql_pat_…` opaque |
| Verifies on | all nodes (JWKS) | all nodes | all nodes | **all nodes** | **identity only** (DB) |
| Surface | full app | NodeService only | VoiceAgent* msgs | read/query + 1 agent turn | identity APIs |
| Persisted row | session | identity row | identity row | **none** | identity row |
| Default TTL | 15 min | 30 d | 90 d | **1 h** | long-lived |
| Revoke | session revoke | row + key | row + key | **expiry / key** | row |
| Use | humans | mesh nodes | voice-agent proc | **automation / deploy gate** | human CLI |

## Code map

| Piece | Location |
| --- | --- |
| Class const + issue fn | `component/identity/jwt.go` (`ClassServiceAccount`, `IssueServiceAccountAccessToken`) |
| TTL default | `component/identity/config.go` (`DefaultServiceAccountTokenTTLSeconds`) |
| Verifier class const | `component/identity/verifier/verifier.go` (`ClassServiceAccount`) |
| Interceptor + allowlist | `component/grpc/service_account_stream_interceptor.go` |
| Chain wiring | `app/transport.go` (`NewServiceAccountStreamInterceptor`) |
| Mint subcommand | `subcommand_service_account_token.go` (build tag `identity`) |
| Tests | `component/identity/jwt_node_test.go`, `component/grpc/service_account_stream_interceptor_test.go` |

See also [voice-agent-jwt.md](voice-agent-jwt.md) and [node-jwt.md](node-jwt.md)
(the sibling machine classes), [identity-service.md](identity-service.md)
(signing-key rotation), and [threat-model.md](../../../internal/design/auth-threat-model.md).
