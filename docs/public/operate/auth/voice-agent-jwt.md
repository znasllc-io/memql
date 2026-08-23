---
title: Voice-agent service-account JWTs
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Voice-agent service-account JWTs

The Go voice-agent (`integrations/voice/agent/`) authenticates to
MemQL's `MemqlService.Stream` via an identity-issued service-account JWT.
Closes [threat-model §5.2](../../../internal/design/auth-threat-model.md#52-voice-agent-shared-secret-f4)
/ [#109](https://github.com/znasllc-io/memql/issues/109).

## Token shape

A voice-agent JWT is a regular identity-issued EdDSA-signed JWT with:

| Claim | Value |
| --- | --- |
| `class` | `"voice_agent"` (the surface pin) |
| `node_id` | The voice-agent **instance id** (e.g. `voice-agent-prod-us-east-1`); reused field slot so the JWT shape stays uniform across class types |
| `sub` | The `v1:identity:identity.id` of the underlying credential row |

The token is signed with the same EdDSA key as user-class JWTs, so
the per-node verifier validates both via the same JWKS endpoint.

## Surface pinning

The voice-agent interceptor admits a class=`voice_agent` JWT and
pins the call to `VoiceAgent*` payload types
(`VoiceAgentSessionStart`, `VoiceAgentSessionEnd`,
`VoiceAgentPartialTranscript`, `VoiceAgentFinalTranscript`,
`VoiceAgentTurnRequest`, plus the `ClientHello` / `Heartbeat` /
`Unsubscribe` / `CancelRequest` stream-level control frames). A
leaked voice-agent credential can't drive other RPCs.

User-class JWTs (the default identity mint) fall through to the
regular auth chain.

## Provisioning

Each running voice-agent process needs one provisioned token,
delivered into its `VOICE_AGENT_TOKEN` env var before startup.

### The mint subcommand

The identity binary ships a `voice-agent-token mint` subcommand that
runs both steps in one call. Because the identity service owns the
signing key, the subcommand must run as the identity binary itself
(typically `docker exec` into the live container):

```bash
# Dev (against the local cluster):
make voice-agent-token INSTANCE=voice-agent-local

# Equivalent direct invocation:
kubectl exec -n memql deploy/identity -- /app/memql voice-agent-token mint \
  --instance-id=voice-agent-local

# Optional flags:
#   --ttl=720h           Token lifetime (default 90d).
#   --out=/path/token    Write to file (0600) instead of stdout.
#   --minted-by=<userId> Audit attribution (default system:voice-agent-token-cli).
```

The subcommand prints the compact-form bearer to stdout. Diagnostic
output (identity id, instance id, expiry) goes to stderr so capture
patterns like `TOKEN=$(make voice-agent-token ...)` work.

### What the subcommand does

1. **Mints a `v1:identity:identity` row** with
   `identityType="voice_agent_token"` carrying `instanceId`,
   `keyHash` (SHA-256 of an auxiliary random bearer the subcommand
   generates and discards), `mintedBy`, and `expiresAt = now + TTL`.
2. **Signs a `class="voice_agent"` JWT** via
   `JWTIssuer.IssueVoiceAgentAccessToken(VoiceAgentIssueInput{...})`
   bound to the freshly minted identity row.
3. **Returns the JWT** -- this is what becomes
   `VOICE_AGENT_TOKEN`. The auxiliary bearer hashed into `keyHash`
   is never printed; the JWT is the only credential the voice-agent
   ever sees. The hash satisfies the schema's `@required` contract
   and gives operators a stable fingerprint for audit correlation.

The voice-agent attaches `Authorization: Bearer ${TOKEN}` on every
outbound `MemqlService.Stream` dial; the
voice-agent-stream-interceptor on the BFF accepts the JWT and pins
the call to the `VoiceAgent*` payload types.

## Bring-up injection (dev + prod)

`VOICE_AGENT_TOKEN` is an **injected runtime credential**, not a
stored secret. It is minted at bring-up and lives only in the
process environment of the voice-agent container -- the sealed
`memql-secrets` Secret (dev) and the deploy pipeline's secret store
(prod) do NOT carry it.

### Local cluster: self-bootstrap (default, memql#342)

The `voice` Deployment in the local k3d cluster ships the
self-bootstrap path wired by default so the voice pod comes up cleanly
without an out-of-band mint step. On startup, when `VOICE_AGENT_TOKEN`
is empty, the Go voice-agent's `ResolveVoiceAgentToken` posts to the
identity service's `POST /node/bootstrap` endpoint with
`tokenClass="voice_agent"` +
`instanceId="<MEMQL_VOICE_AGENT_INSTANCE_ID>"`, presenting the
`MEMQL_NODE_BOOTSTRAP_TOKEN` bootstrap secret. Identity returns a
minted `class="voice_agent"` JWT and the agent uses it for the
rest of the process's lifetime.

The local overlay wires all three knobs so bring-up needs no manual step
(`deploy/k8s/base/voice-agent.yaml`, secrets seeded by
`scripts/k3d/seed-secrets.sh`):

| Env var | Local default | Production posture |
| --- | --- | --- |
| `MEMQL_NODE_BOOTSTRAP_TOKEN` | Generated per cluster -- 64 hex characters (32 random bytes via `od -An -tx1 -N32 /dev/urandom`), seeded into `memql-secrets` by `seed-secrets.sh` and shared with node-class bootstrap | Leave unset on identity side -- endpoint stays dark |
| `MEMQL_IDENTITY_VERIFIER_BASE_URL` | `https://identity:8085` (cluster DNS) | Set to the deployed identity URL (HTTPS) |
| `MEMQL_VOICE_AGENT_INSTANCE_ID` | The pod's own name, via a Kubernetes `fieldRef: metadata.name` | Set to the deployed instance label (e.g. `voice-agent-prod-us-east-1`) |

The endpoint reuses the same secret + same bootstrap surface as
node-class JWTs (memql#338); operators have one secret to rotate
and one endpoint to audit. The node-class companion lives in
`component/node/bootstrap_token.go`; the voice-agent's companion
lives in `integrations/voice/agent/bootstrap.go`
(`maybeBootstrapVoiceAgentToken`).

### Out-of-band mint (operator-provisioned)

The out-of-band path stays the production-grade flow and also works
locally. Mint the token explicitly after the identity service is
healthy and inject it as the pod's `VOICE_AGENT_TOKEN`:

1. The voice pod self-bootstraps via the path above and starts cleanly.
2. Wait until the identity service reports `status=ok` with
   `memoryNodesDB` running (`kubectl logs -n memql deploy/identity`).
3. `make voice-agent-token INSTANCE=voice-agent-local` execs the
   identity binary's
   `voice-agent-token mint --instance-id=voice-agent-local`
   subcommand and captures the JWT.
4. Seed the JWT into the `memql-secrets` Secret (`make secrets`
   after updating `memql-secrets`) and roll the Deployment
   (`kubectl rollout restart -n memql deploy/voice`). Once
   `VOICE_AGENT_TOKEN` is set, the explicit token wins over the
   self-bootstrap path (operator-provisioned tokens always win).

The instance id is stable across remints (`voice-agent-local`),
so each remint produces a fresh JWT against a freshly inserted
`v1:identity:identity` row; old rows soft-expire via `expiresAt`.

### Which path runs?

`load_config` checks env vars in this order:

1. `VOICE_AGENT_TOKEN` non-empty -> use it directly (operator path).
2. `MEMQL_NODE_BOOTSTRAP_TOKEN` non-empty + `MEMQL_IDENTITY_VERIFIER_BASE_URL`
   + `MEMQL_VOICE_AGENT_INSTANCE_ID` -> self-bootstrap (dev path).
3. Otherwise -> raise `RuntimeError` with the canonical
   "VOICE_AGENT_TOKEN unset" message + provisioning pointers.

### Cloud install

The deploy pipeline does the same dance. ("Cloud" is a deploy TARGET, not a
tier: MemQL ships one installation shape, and a second environment is a second
install — epic memql#3943.)

1. Identity service comes up first (or is already up).
2. The pipeline runs `voice-agent-token mint --instance-id=<instance-id>`
   against the identity binary in that install's cluster.
3. The minted JWT lands in the deploy pipeline's secret store --
   Azure Key Vault, synced into the cluster via External Secrets
   Operator (ESO) -- and is injected as `VOICE_AGENT_TOKEN` into the
   voice-agent container at startup. MemQL's cloud target is Azure
   Kubernetes Service, not Cloud Run; see the "Deploy targets" table
   in the repo's top-level CLAUDE.md.
4. Rotation = re-mint + re-inject + restart on the same cadence
   (no in-place refresh path).

Operators who want to skip the live mint (air-gapped deploys, etc.)
can capture the bearer with `--out=/path/to/token` and feed it into
the secret store manually. The plain bearer is shown ONCE per mint.

## Rotation

Voice-agent JWTs default to a 90-day TTL
(`DefaultVoiceAgentTokenTTLSeconds`) and have no refresh path.
Rotate by minting fresh + restarting:

1. Mint a new JWT for the same instance id (the underlying identity
   row stays; only `expiresAt` advances).
2. Update `VOICE_AGENT_TOKEN` in the process's secret store.
3. Restart the voice-agent process.

### Killing a compromised token

**Soft-delete the credential's identity row.** Set `active=false` on the
`v1:identity:identity[voice_agent_token]` row whose id is the token's
`sub`. Every voice-agent stream open re-checks that row, so the
credential stops being admitted within the revocation cache TTL --
**5 seconds** (`DefaultVoiceAgentRevocationCacheTTL`), not the token's
90-day expiry.

Mechanics, for anyone auditing this:

- The gate is `component/grpc/voice_agent_revocation.go`, wired in
  `app/transport.go`. It runs AFTER the JWT verifies and the class is
  confirmed, and BEFORE the actor is stamped -- so a revoked credential
  never becomes an actor.
- It **fails closed.** A lookup error rejects the open with
  `Unauthenticated` rather than admitting on a half-answered check.
- A credential whose row does not exist is **not** treated as revoked.
  The operator-CLI mint path persists no row, so locking those out would
  break minting rather than close a leak. This matches the `node` class
  (memql#349).
- It reads row state, not a claim. `verifier.EpochCheck` (memql#106) is
  the user-class mechanism and does not apply here: it keys on
  `v1:identity:user.revocationEpoch`, and a voice-agent credential is a
  machine identity with no user row to bump.

> **This did not work before memql#4111.** The verify path was
> JWKS-only, this class creates no `v1:identity:authSession` row, so the
> session-revocation middleware never saw it, and no epoch check was
> wired. Soft-deleting the row recorded the operator's intent and
> changed nothing: the token stayed valid for its full 90-day TTL, and
> the only real mitigation was rotating the identity signing key --
> which invalidates every JWT of every class at once. If you are reading
> a copy of this page that promises a
> `IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS` window, that variable has
> never existed in this repo.

Signing-key rotation remains the answer when the SIGNING KEY itself is
compromised, rather than one credential.

## Out of scope

- **Automated token rotation / refresh.** Voice-agent JWTs have no
  refresh path; rotation is manual re-mint + restart. Periodic
  rotation could be wrapped by a cron or deploy-time helper, but
  the credential itself does not refresh in place.
- **Multi-tenant voice-agent topology.** The interceptor admits one
  class per call; multi-tenant routing (which tenant owns this
  voice-agent process?) would need extra claims.
- **Revoking a `service_account`-class token the same way.** That class
  is deliberately different and is not a gap to close here: its subject
  is not required to name a persisted row, so there is no row state to
  read. Its answer is the TTL -- 1 hour by default
  (`DefaultServiceAccountTokenTTLSeconds`) against this class's 90 days.
  See [service-account-jwt.md](service-account-jwt.md).
