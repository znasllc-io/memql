---
title: Node service-account JWTs
audience: public
status: stable
area: operate
sinceVersion: 0.9.0
owner: znas
---

# Node service-account JWTs

memQL cluster nodes authenticate to `NodeService.Stream` using an
identity-issued `class="node"` JWT. Closes
[threat-model §5.1](../../../internal/design/auth-threat-model.md#51-inter-node-mesh-trust-f1) /
[#105](https://github.com/znasllc-io/memql/issues/105).

## Token shape

A node JWT is a regular identity-issued EdDSA-signed JWT plus three
extra claims:

| Claim | Value |
| --- | --- |
| `class` | `"node"` (the surface pin) |
| `node_id` | The `v1:cluster:node.id` the token binds to |
| `node_type` | The build-tag-derived role (`bff`, `voice`, `cognition`, `agent`, `planner`, `workbench`) |
| `sub` | The `v1:identity:identity.id` of the underlying credential row |

The token is signed with the same EdDSA key as user-class JWTs, so
the per-node verifier validates both via the same JWKS endpoint.

## Surface pinning

The `NodeService.Stream` interceptor admits a class=`node` JWT and
rejects every other shape:

- Non-JWT source (PATs) → rejected (PATs can't speak the mesh).
- `Class != "node"` → rejected (user-class JWTs can't speak the mesh).
- `NodeId == ""` or `NodeType == ""` → rejected (a class=node token with no binding is malformed).
- After admission, `NodeHello.NodeId` / `NodeType` must match the token's claims; mismatch returns `NodeShutdown` and disconnects.

When the per-node verifier isn't configured (single-node dev /
binaries with no identity service) the interceptor is a no-op
pass-through; the mesh runs unauthenticated and the BFF-only run
doesn't need a token.

## Provisioning

Each node binary needs one provisioned token, copied into its
`MEMQL_NODE_TOKEN` env var before startup.

1. **Reserve a `v1:cluster:node.id`** for the binary (e.g.
   `v1:cluster:node:cognition-1`). The token's `node_id` claim
   binds to it; rotation reuses the same id.
2. **Mint a `v1:identity:identity` row** with
   `identityType="node_token"` and the credential variant fields:
    - `nodeId` → the reserved cluster-node id
    - `nodeType` → the build-tag string
    - `keyHash` → SHA-256 of the plain token
    - `mintedBy` → admin user id (audit)
    - `expiresAt` → default `now + 30d`
3. **Sign a `class="node"` JWT** via
   `JWTIssuer.IssueNodeAccessToken(NodeIssueInput{...})`. The plain
   compact-form bearer is returned ONCE.
4. **Copy the bearer** into the target binary's `MEMQL_NODE_TOKEN`
   env var. The binary attaches `Authorization: Bearer
   ${MEMQL_NODE_TOKEN}` on every outbound `NodeService.Stream` dial.

## Rotation

Node tokens have a 30-day default TTL
(`DefaultNodeTokenTTLSeconds`) and no refresh path:

1. Mint a fresh node JWT for the same `node_id` + `node_type`.
2. Update the target binary's `MEMQL_NODE_TOKEN` env var.
3. Restart the binary. The outbound dialer presents the new bearer;
   the remote interceptor accepts it; the old token's remaining TTL
   drains harmlessly.

For "compromised token, kill it NOW" flows, soft-delete the identity
row (`active=false`). The verifier's per-stream revocation watcher
(#106) catches subsequent calls within
`IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS` (default 5 min).

## Out of scope

- **Automated provisioning CLI.** Two-call sequence for now.
- **TLS on `NodeService.Stream`.** The interceptor + token pin
  defends against forged peers; mTLS at the transport layer is a
  separate hardening item.
- **Per-token revocation epoch.** Node tokens piggyback on the
  existing user-row epoch (#106). A dedicated node-row epoch would
  let ops kill a specific compromised node token without touching
  every user's tokens.
