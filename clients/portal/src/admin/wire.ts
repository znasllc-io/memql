import type { QueryClient, Row } from "@znasllc-io/memql-sdk-core/client";
import { renderMemQLValue } from "@znasllc-io/memql-sdk-core/client";

// The admin console's reads and writes, and the one place that decides HOW
// this surface reaches the cluster.
//
// ===========================================================================
// NAMED QUERIES, NOT THE GENERIC CONCEPT BROWSE. THAT IS THE WHOLE POINT.
// ===========================================================================
// Everywhere else in the portal a population arrives through
// browseConceptPage -- `sort(paginate(concept==<id>, N), ...)`, a raw call the
// engine runs without a declared binding. That is correct for the concept
// browser, and it is WRONG here.
//
// The screens this module feeds replace a server-rendered console whose every
// route sat behind requireAdmin: a caller who is not owner or admin got a 403
// and an `admin_auth_forbidden` audit event, and never saw a row
// (component/identity/admin/auth.go). Moving those screens to the browser must
// not move the gate to the browser with them.
//
// A generic concept browse would do exactly that. v1:identity:user,
// v1:identity:auditEvent and v1:identity:identity declare no `@rowAuthz` tier
// (component/memql/rowauthz_undeclared_gate_test.go enumerates that debt), so
// nothing narrows a raw `concept==v1:identity:user` read by role. The named
// queries below carry `requiresOwnerOrAdmin` as a TOP-LEVEL CONJUNCT in their
// own filter, evaluated in-process against the auth envelope
// (dsl/identity/queries.memql). A reader who calls them gets zero rows from
// the engine, not from this file.
//
// So: every admin read here names a query that gates itself. Where no such
// query exists the surface says so out loud rather than reaching for the
// ungated path -- see the "beyond this console" notes in the pages.
//
// ===========================================================================
// WHAT THIS CONSOLE CANNOT REACH, AND WHY (memql#3324)
// ===========================================================================
// Four capabilities of the server-rendered console have no client-reachable
// seam, so they are NOT implemented here and the pages name their absence:
//
//   edit a user / change a role / suspend   `updateUser` is @serverOnly
//                                           (memql#2991) -- refused for a
//                                           client-originated call.
//   list or revoke a node token             `nodeTokenIdentities` and
//                                           `nodeTokenIdentityById` are
//                                           @serverOnly (memql#2987), and the
//                                           revoke additionally needs a system
//                                           credential actor (memql#2513).
//   rotate the signing key                  no stream message exists; the only
//                                           caller is POST /admin/jwks/rotate.
//   edit cluster settings                   `updateClusterSettings` carries no
//                                           role gate of its own, so writing it
//                                           from here would hand a `writer` a
//                                           power the owner/admin gate reserves.
//   revoke a personal access token          `revokePATIdentity` is the same
//                                           shape, and is enumerated as exactly
//                                           that debt -- "revokes any user's
//                                           personal access token" -- in
//                                           test/dslconformance/
//                                           caller_arg_selection_test.go.
//
// Each is a missing SERVER seam, not a missing screen. Adding one is a change
// to dsl/ or component/grpc/, deliberately out of this change's reach.
//
// The consequence is worth stating plainly rather than discovering screen by
// screen: EVERY WRITE the server-rendered console performed is, today, either
// origin-gated away from the browser or reachable without the owner/admin gate
// that console applies at its route. So these four surfaces are READINGS, and
// each one names the write it does not carry and where that write still lives.
// Shipping a control whose only gate was a boolean in this file would move the
// authorization decision into a browser, which is the one outcome the move must
// not produce.

// namedCall composes a MemQL named-primitive invocation.
//
// renderMemQLValue is the SDK's own literal renderer, so quoting and escaping
// are not reimplemented here. An absent or empty arg is DROPPED rather than
// sent as "": a `when(args.x)` guard is dropped when the arg is absent but
// satisfied by an empty string, so passing "" turns "no filter" into "match
// the empty string" and silently empties the page.
export function namedCall(
  kind: "query" | "mutation",
  name: string,
  args: Record<string, string | undefined> = {},
): string {
  const parts: string[] = [];
  for (const [key, value] of Object.entries(args)) {
    if (value === undefined || value === "") continue;
    parts.push(`${key}: ${renderMemQLValue(value)}`);
  }
  return `${kind} ${name}(${parts.join(", ")})`;
}

async function run(
  query: QueryClient,
  name: string,
  call: string,
  signal?: AbortSignal,
): Promise<Row[]> {
  const result = await query.executeNamed(name, call, signal ? { signal } : {});
  return result.rows();
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// People who can sign in. `searchUsers` filters on `requiresOwnerOrAdmin`
// (dsl/identity/queries.memql), which is why this and not `activeUsers` --
// that one is @serverOnly and answers a client caller with a refusal.
//
// No `active` argument: the console wants deactivated accounts listed too, and
// the arg is arg-conditional, so omitting it is how you say "all of them".
export function readPeople(query: QueryClient, signal?: AbortSignal): Promise<Row[]> {
  return run(query, "searchUsers", namedCall("query", "searchUsers"), signal);
}

// The audit trail, newest first. `category` is optional and pushes the filter
// into SQL rather than narrowing a page in the browser -- which matters,
// because the query pages at 50 and a client-side filter over one page would
// quietly claim "no configuration events" whenever the last 50 were all auth.
export function readAuditEvents(
  query: QueryClient,
  category: string,
  signal?: AbortSignal,
): Promise<Row[]> {
  return run(
    query,
    "recentAuditEvents",
    namedCall("query", "recentAuditEvents", { category }),
    signal,
  );
}

// The singleton runtime settings row (id="cluster").
export async function readClusterSettings(
  query: QueryClient,
  signal?: AbortSignal,
): Promise<Row | null> {
  const rows = await run(
    query,
    "clusterSettingsCurrent",
    namedCall("query", "clusterSettingsCurrent"),
    signal,
  );
  return rows[0] ?? null;
}

// One person's personal access tokens, revoked ones included, in the
// credential-free `patSummary` shape (no keyHash -- the shape is the boundary).
//
// PER USER, because that is the only admin-gated PAT read the cluster
// publishes: `patIdentitiesForUser` takes a required userId. There is no
// `patIdentities()`. The console therefore fans out across the people it just
// read, which useTokenConsole does with a bounded window; see its note.
export function readTokensForUser(
  query: QueryClient,
  userId: string,
  signal?: AbortSignal,
): Promise<Row[]> {
  return run(
    query,
    "patIdentitiesForUser",
    namedCall("query", "patIdentitiesForUser", { userId }),
    signal,
  );
}

// There is deliberately no revoke here. `revokePATIdentity` takes an arbitrary
// identity id, applies no predicate, and the coarse write check admits every
// role from `writer` up -- so the owner/admin rule that protects the revoke
// today is the identity service's ROUTE, not the cluster's. Calling it from a
// browser would leave the button gated by a React boolean and nothing else.

// ---------------------------------------------------------------------------
// The signing keys, over plain HTTP
// ---------------------------------------------------------------------------

// One published key. Mirrors component/identity/jwks.go's JWK, minus the
// members this console has no use for.
export interface SigningKey {
  kid: string;
  kty: string;
  crv: string;
  alg: string;
  use: string;
}

// fetchSigningKeys reads /.well-known/jwks.json off the identity origin.
//
// NOT over the stream, and not because the stream is inconvenient: the JWKS
// feed is a PUBLIC document by specification -- every verifier node in the
// mesh fetches it unauthenticated to check a token's signature -- so a second,
// gated copy of it on the stream would assert a confidentiality this data does
// not have. The console reads the same bytes any verifier reads, which is also
// the only way it can tell an operator what verifiers are actually seeing.
//
// `origin` is the runtime config's identityUrl. Empty means this deployment
// could not derive one; the caller renders that as an explained gap rather
// than fetching a relative path that would resolve against the portal's own
// origin and 404.
export async function fetchSigningKeys(
  origin: string,
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
  signal?: AbortSignal,
): Promise<SigningKey[]> {
  const base = origin.replace(/\/+$/, "");
  if (base === "") throw new Error("this deployment publishes no identity origin");

  const response = await fetchImpl(`${base}/.well-known/jwks.json`, {
    // No credential. The feed is public, and sending one would make a
    // cross-origin read that works today fail the moment an operator's
    // deployment tightens its CORS policy.
    credentials: "omit",
    ...(signal ? { signal } : {}),
  });
  if (!response.ok) {
    throw new Error(`the identity service answered ${response.status}`);
  }
  const body: unknown = await response.json();
  const keys = (body as { keys?: unknown })?.keys;
  if (!Array.isArray(keys)) throw new Error("the JWKS feed carried no key set");

  const out: SigningKey[] = [];
  for (const entry of keys) {
    if (entry === null || typeof entry !== "object") continue;
    const k = entry as Record<string, unknown>;
    out.push({
      kid: typeof k["kid"] === "string" ? k["kid"] : "",
      kty: typeof k["kty"] === "string" ? k["kty"] : "",
      crv: typeof k["crv"] === "string" ? k["crv"] : "",
      alg: typeof k["alg"] === "string" ? k["alg"] : "",
      use: typeof k["use"] === "string" ? k["use"] : "",
    });
  }
  return out;
}
