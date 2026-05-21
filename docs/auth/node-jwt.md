# Node service-account JWTs

memQL cluster nodes authenticate to `NodeService.Stream` using a
dedicated `class="node"` JWT. This is the mechanism that closes
[threat-model §5.1](threat-model.md#51-inter-node-mesh-trust-f1) /
[#105](https://github.com/znasllc-io/memql/issues/105): without it, any
binary that can reach the inter-node port AND holds any
identity-issued JWT can drive the mesh; with it, only tokens that
carry `class="node"` plus a matching `(node_id, node_type)`
binding pass.

## Token shape

A node JWT is a regular identity-issued EdDSA-signed JWT plus three
extra claims:

| Claim | Value |
| --- | --- |
| `class` | `"node"` (constant; the surface pin) |
| `node_id` | The `v1:cluster:node.id` the token binds to |
| `node_type` | The build-tag-derived role (`bff`, `voice`, `cognition`, `agent`, `planner`, `workbench`) |
| `sub` | The `v1:identity:identity.id` of the underlying credential row (NOT a user id) |

The token is signed with the same EdDSA key as user-class JWTs, so the per-node verifier validates both via the same JWKS endpoint. The class pin is enforced at the `NodeService.Stream` interceptor:

- Source `!= SourceJWT` → rejected (PATs can't speak the mesh).
- `Class != "node"` → rejected (user-class JWTs can't speak the mesh).
- `NodeId == ""` or `NodeType == ""` → rejected (a `class="node"` token with no binding is malformed).
- After admission, `NodeHello.NodeId` / `NodeType` must match the token's claims; mismatch returns `NodeShutdown` and disconnects.

## Provisioning

Each node binary needs one provisioned token, copied into its `MEMQL_NODE_TOKEN` env var before startup. The provisioning flow is admin-driven (no self-service):

1. **Reserve a `v1:cluster:node.id`** for the binary. Choose a stable id (e.g. `v1:cluster:node:cognition-1`) -- the token's `node_id` claim binds to it and rotation reuses the same id.
2. **Mint a `v1:identity:identity` row** with `identityType="node_token"` and the credential variant fields:
    - `nodeId` → the reserved cluster-node id
    - `nodeType` → the build-tag string
    - `keyHash` → SHA-256 of the plain token (Go-side helper computes this; see #105 PR)
    - `mintedBy` → admin user id (audit)
    - `expiresAt` → default `now + 30d`
3. **Sign a `class="node"` JWT** via `JWTIssuer.IssueNodeAccessToken(NodeIssueInput{...})`. The plain compact-form bearer is returned ONCE.
4. **Copy the bearer** into the target binary's `MEMQL_NODE_TOKEN` env var. The binary reads it at startup (`node.NewIdentity`) and attaches `Authorization: Bearer ${MEMQL_NODE_TOKEN}` on every outbound `NodeService.Stream` dial.
5. **Optionally flip `MEMQL_NODE_REQUIRE_AUTH=1`** on every node in the fleet. Until this is set, the interceptor stays unwired and existing un-tokened streams still admit (rollout-safe default).

A future `memql-identity admin create-node-token` CLI will wrap steps 1-3 in a single command; today the flow is two engine calls and one CLI invocation.

## Rotation

Node tokens have a 30-day default TTL (`DefaultNodeTokenTTLSeconds`) and **no refresh path**. To rotate:

1. Mint a fresh node JWT for the same `node_id` + `node_type` (the underlying `v1:identity:identity` row stays; only its `expiresAt` advances).
2. Update the target binary's `MEMQL_NODE_TOKEN` env var.
3. Restart the binary. The outbound dialer presents the new bearer; the remote interceptor accepts it; the old token's remaining TTL drains harmlessly.

For "compromised token, kill it NOW" flows, soft-delete the identity row (`active=false`) -- the verifier's per-stream revocation watcher (#106) catches subsequent calls within `IDENTITY_VERIFIER_REVOCATION_CHECK_SECONDS` (default 5 min).

## Rollout sequence

The interceptor defaults to OFF (`MEMQL_NODE_REQUIRE_AUTH` unset) so existing clusters don't break on upgrade. Recommended sequence:

1. Deploy the binary version that ships the interceptor + outbound dialer support. Mesh continues unauthenticated.
2. Provision a node token for every binary in the fleet. Set `MEMQL_NODE_TOKEN` on each one.
3. Restart each binary one at a time. Outbound calls now carry the bearer; inbound calls still admit unauthenticated peers.
4. Flip `MEMQL_NODE_REQUIRE_AUTH=1` across the fleet. Inbound calls now require `class="node"` JWTs. Any binary still un-tokened gets rejected immediately.

The "still un-tokened" failure mode is the operator's signal that step 2 missed a binary. Surfaced as `codes.Unauthenticated` / `codes.PermissionDenied` with descriptive messages.

## Dev mode

Single-node binaries (the default BFF-only run) don't invoke `NodeService.Stream` at all -- the worker dialer doesn't start, the parent connector doesn't start. `MEMQL_NODE_TOKEN` and `MEMQL_NODE_REQUIRE_AUTH` stay unset; the interceptor stays unwired; nothing changes. Local cluster compose files (`docker/docker-compose.cluster.yml`) similarly stay unauthenticated until ops provisions tokens for the compose-managed nodes.

## What's still out of scope

- **Automated provisioning CLI.** The mint flow is a two-call sequence today; ops scripts it.
- **TLS on `NodeService.Stream`.** The interceptor + token pin defend against forged peers; mTLS at the transport layer is a separate gap tracked separately. The two compose cleanly -- a future PR can require both.
- **Per-token revocation epoch.** Node tokens piggyback on the existing user-row epoch (#106). A dedicated node-row epoch would let ops kill a specific compromised node token without touching every user's tokens; not needed for the MVP.
