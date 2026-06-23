---
title: Connect to the memQL MCP server
audience: public
status: stable
area: operate
sinceVersion: 0.9.60
owner: znas
---

# Connect to the memQL MCP server

The `mcp` node exposes the memQL engine tool surface to MCP hosts (Claude Code,
and -- once OAuth lands, [#1556] -- Claude Desktop / claude.ai). It speaks two
transports:

- **stdio** (default) -- the MCP host launches `memql-mcp` as a local
  subprocess. Local only; there is no network listener and no bearer auth (the
  trust boundary is "you started the process").
- **Streamable HTTP** (`MEMQL_MCP_TRANSPORT=http`) -- a network listener
  (`POST/GET/DELETE /mcp`) so a host can reach a hosted deployment over the
  network. Bearer auth is **mandatory and default-deny**: no valid token -> 401
  before any engine work runs.

The capability tier (`MEMQL_MCP_MODE`) and acting role (`MEMQL_MCP_ROLE`) gates
are documented in [build-tags.md](../build/build-tags.md#mcp-node-configuration-epic-memql1529);
this page is about connecting.

## Connect Claude Code to staging (remote HTTP)

Staging serves the MCP head at `https://mcp.staging.copresent.ai/mcp` at the
`authoring` capability tier.

### 1. Mint a bearer token

The HTTP transport is wrapped with the per-node identity **verifier**, which
accepts only **JWKS-verifiable identity JWTs**. A **service-account JWT**
(`class="service_account"`, [#691]) is the right credential for a machine client
like Claude Code:

```bash
# On the identity binary (locally: docker exec memql-identity ...; on staging:
# an identity-side Job). Prints the JWT on stdout. 1-hour TTL.
memql service-account-token mint --label claude-mcp-staging
```

A user session JWT (from the magic-link login) also works. See
[service-account-jwt.md](auth/service-account-jwt.md) for the full mint +
rotation flow.

> **Not a PAT.** A Personal Access Token (`mql_pat_...`) is **rejected** by the
> per-node verifier -- PATs are validated only by the identity binary, and the
> `mcp` node is not the identity binary. Use a service-account or user JWT.

The role the token carries gates the tier (Gate B): the named read/exec tool
surface works for any authenticated role; `define` (authoring) and `query`
(inline) require the `owner` or `developer` role. Per-row authorization applies
to every tool on top of the tier.

### 2. Add the connector

```bash
claude mcp add --transport http memql-staging \
  https://mcp.staging.copresent.ai/mcp \
  --header "Authorization: Bearer <token from step 1>"
```

Verify the tools resolve:

```bash
claude mcp list                 # memql-staging should show "connected"
```

The token expires (1h for a service-account JWT); re-mint and re-add (or update
the header) when it does.

## Run + connect locally

### stdio (the simple local path)

Build the engine-node binary and let the MCP host run it as a subprocess. stdio
is the default transport, so no transport env is needed:

```bash
GOWORK=off make mcp            # -> bin/memql-mcp (engine node, no CoPresent DSL)

# Register it with Claude Code as a local subprocess. The binary runs the full
# engine, so it needs the engine env (DB DSN + genesis) the same way any node
# does -- the simplest path is to point it at your local k3d cluster database
# (via the postgres port-forward, make db).
claude mcp add memql-local -- /absolute/path/to/bin/memql-mcp
```

No bearer auth on stdio: the protocol owns stdout (the binary redirects its own
logs to stderr so the JSON-RPC wire stays clean), and the trust boundary is the
local process.

### HTTP locally (exercise the remote path)

To test the network transport locally you also need a reachable identity
verifier (HTTP mode refuses to start without one -- default-deny):

```bash
# Forward the identity service first: kubectl port-forward -n memql svc/identity 8085:8085
MEMQL_MCP_TRANSPORT=http \
MEMQL_MCP_HTTP_ADDR=:8090 \
MEMQL_MCP_MODE=authoring \
IDENTITY_VERIFIER_BASE_URL=http://localhost:8085 \
  ./bin/memql-mcp
# then: claude mcp add --transport http memql-local-http \
#   http://127.0.0.1:8090/mcp --header "Authorization: Bearer <local JWT>"
```

The blessed local environment that provides identity + a database is the k3d
parity cluster (`make up`); mint a local token against the identity service via
its port-forward (`localhost:8085`).

## Environment surface

| Variable | Default | Meaning |
|----------|---------|---------|
| `MEMQL_MCP_TRANSPORT` | `stdio` | `stdio` (local subprocess) or `http` (network listener). |
| `MEMQL_MCP_HTTP_ADDR` | `:8090` | Bind address for the HTTP transport. |
| `MEMQL_MCP_MODE` | `authoring` | Capability tier (Gate A): `sealed` / `authoring` / `inline`. See [build-tags.md](../build/build-tags.md#mcp-node-configuration-epic-memql1529). |
| `MEMQL_MCP_ROLE` | (token claim) | Acting role (Gate B). On HTTP, **empty = taken from the caller's verified token**; a non-empty value pins a conservative deployment role. |
| `MEMQL_MCP_USER` | (token claim) | Acting user that session-authored constructs are owner-scoped to. On HTTP, empty = taken from the caller's token. |

## Auth + capability posture

- **Default-deny.** The HTTP transport refuses to start without a verifier, and
  rejects any request without a valid identity JWT with `401`.
- **Per-caller identity.** On staging the role/user are taken from the verified
  token claims (the deployment does not pin `MEMQL_MCP_ROLE`/`USER`), so each
  caller acts as themselves under the normal role + per-row-authorization rules.
- **Conservative tier first.** Staging starts at `authoring`; `define`/`query`
  still require `owner`/`developer`. Raise or lower the tier via
  `MEMQL_MCP_MODE` as the posture is proven.
- **OAuth 2.1** for Claude Desktop / claude.ai custom connectors is a tracked
  follow-up ([#1556]); until then those hosts cannot use the bearer-header path
  that Claude Code uses.

[#691]: https://github.com/znasllc-io/memql/issues/691
[#1556]: https://github.com/znasllc-io/memql/issues/1556
