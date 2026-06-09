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
memQL's `MemqlService.Stream` via an identity-issued service-account JWT.
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
docker exec memql-identity /app/memql voice-agent-token mint \
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
genesis envelope (dev) and the deploy pipeline's secret store
(prod) do NOT carry it.

### Dev (`make dev-refresh`)

### Dev: self-bootstrap (default, memql#342)

`docker-compose.polyphon.yml` ships with the self-bootstrap path
wired by default so a stock `docker compose up` brings the
voice-agent up cleanly without `make dev-refresh` running first.
On startup, when `VOICE_AGENT_TOKEN` is empty, the Go
voice-agent's `ResolveVoiceAgentToken` posts to the identity service's
`POST /node/bootstrap` endpoint with `tokenClass="voice_agent"` +
`instanceId="<MEMQL_VOICE_AGENT_INSTANCE_ID>"`, presenting the
`MEMQL_NODE_BOOTSTRAP_TOKEN` bootstrap secret. Identity returns a
minted `class="voice_agent"` JWT and the agent uses it for the
rest of the process's lifetime.

The compose file defaults all three knobs to dev sentinels:

| Env var | Dev default | Production posture |
| --- | --- | --- |
| `MEMQL_NODE_BOOTSTRAP_TOKEN` | `dev-bootstrap-do-not-use-in-production-memql338` | Leave unset on identity side -- endpoint stays dark |
| `IDENTITY_VERIFIER_BASE_URL` | `http://identity:8081` | Set to the deployed identity URL (HTTPS) |
| `MEMQL_VOICE_AGENT_INSTANCE_ID` | `voice-agent-local` | Set to the deployed instance label (e.g. `voice-agent-prod-us-east-1`) |

The endpoint reuses the same secret + same bootstrap surface as
node-class JWTs (memql#338); operators have one secret to rotate
and one endpoint to audit. The node-class companion lives in
`component/node/bootstrap_token.go`; the voice-agent's companion
lives in `integrations/voice/agent/bootstrap.go`
(`maybeBootstrapVoiceAgentToken`).

### Dev: out-of-band mint (`make dev-refresh`)

The pre-#342 out-of-band path still works and stays the
production-grade flow. `scripts/dev/refresh.sh` mints and injects
the token explicitly after the identity service is healthy:

1. Stack comes up via `docker compose up`. The voice-agent
   self-bootstraps via the path above and starts cleanly.
2. `wait_for_identity` polls `http://localhost:8081/healthz` until
   the identity service reports `status=ok` with `memoryNodesDB`
   running.
3. `mint_voice_agent_token` execs the identity binary's
   `voice-agent-token mint --instance-id=voice-agent-local`
   subcommand and captures the JWT.
4. The script exports `VOICE_AGENT_TOKEN` into its shell and runs
   `docker compose up -d --no-deps --force-recreate voice-agent`
   so compose re-evaluates `${VOICE_AGENT_TOKEN:-}` in
   `docker-compose.polyphon.yml`'s voice-agent `environment:` block.
   Once VOICE_AGENT_TOKEN is set, the explicit token wins over the
   self-bootstrap path (operator-provisioned tokens always win).

The instance id is stable across refreshes (`voice-agent-local`),
so each refresh mints a fresh JWT against a freshly inserted
`v1:identity:identity` row; old rows soft-expire via `expiresAt`.

### Which path runs?

`load_config` checks env vars in this order:

1. `VOICE_AGENT_TOKEN` non-empty -> use it directly (operator path).
2. `MEMQL_NODE_BOOTSTRAP_TOKEN` non-empty + `IDENTITY_VERIFIER_BASE_URL`
   + `MEMQL_VOICE_AGENT_INSTANCE_ID` -> self-bootstrap (dev path).
3. Otherwise -> raise `RuntimeError` with the canonical
   "VOICE_AGENT_TOKEN unset" message + provisioning pointers.

### Prod

The deploy pipeline does the same dance:

1. Identity service comes up first (or is already up).
2. The pipeline runs `voice-agent-token mint --instance-id=<env-instance-id>`
   against the identity binary in the production cluster.
3. The minted JWT lands in the deploy pipeline's secret store
   (Cloud Run env vars, Secret Manager, etc.) and is injected as
   `VOICE_AGENT_TOKEN` into the voice-agent container at startup.
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

For "compromised token, kill it NOW" flows, soft-delete the identity
row (`active=false`). The verifier's per-stream revocation watcher
(#106) catches the next periodic re-check within
`IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS` (default 5 min).

## Out of scope

- **Automated token rotation / refresh.** Voice-agent JWTs have no
  refresh path; rotation is manual re-mint + restart. Periodic
  rotation could be wrapped by a cron or deploy-time helper, but
  the credential itself does not refresh in place.
- **Multi-tenant voice-agent topology.** The interceptor admits one
  class per call; multi-tenant routing (which tenant owns this
  voice-agent process?) would need extra claims.
- **Per-instance revocation epoch.** Voice-agent tokens piggyback
  on the identity-row revocation surface; a dedicated per-instance
  epoch would let ops kill a single compromised instance without
  affecting peers.
