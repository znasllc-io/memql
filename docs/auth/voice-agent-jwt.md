# Voice-agent service-account JWTs

The Python voice-agent process authenticates to memQL's
`MemqlService.Stream` via an identity-issued service-account JWT.
Closes [threat-model §5.2](threat-model.md#52-voice-agent-shared-secret-f4)
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

1. **Reserve an instance id** for the process (e.g.
   `voice-agent-prod-us-east-1`). Appears in audit logs.
2. **Mint a `v1:identity:identity` row** with
   `identityType="voice_agent_token"` and the credential variant
   fields: `instanceId`, `keyHash` (SHA-256 of the plain token),
   `mintedBy`, `expiresAt` (default `now + 90d`).
3. **Sign a `class="voice_agent"` JWT** via
   `JWTIssuer.IssueVoiceAgentAccessToken(VoiceAgentIssueInput{...})`.
   The plain compact-form bearer is returned ONCE.
4. **Copy the bearer** into the voice-agent's `VOICE_AGENT_TOKEN`
   env var. The voice-agent attaches `Authorization: Bearer
   ${TOKEN}` on every outbound `MemqlService.Stream` dial.

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

- **Automated provisioning CLI.** Two-call sequence for now.
- **Multi-tenant voice-agent topology.** The interceptor admits one
  class per call; multi-tenant routing (which tenant owns this
  voice-agent process?) would need extra claims.
- **Per-instance revocation epoch.** Voice-agent tokens piggyback
  on the identity-row revocation surface; a dedicated per-instance
  epoch would let ops kill a single compromised instance without
  affecting peers.
