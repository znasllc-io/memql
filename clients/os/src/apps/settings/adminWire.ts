import type { Connection, Row } from "@znasllc-io/memql-sdk-core/client";

// The reads behind the three operator sections the portal used to hold:
// Providers, Tokens and Keys (epic memql#4984).
//
// ===========================================================================
// NAMED, SELF-GATING QUERIES -- NEVER THE GENERIC CONCEPT BROWSE
// ===========================================================================
// Everywhere else a population arrives through a live collection over a
// concept. That is right for a surface whose rows carry a row-authz tier, and
// it is WRONG here. `v1:identity:identity` and `v1:identity:clusterSettings`
// declare NO tier, so nothing narrows a raw `concept==v1:identity:identity`
// read by role -- a reader would get every credential row in the cluster.
//
// Each read below names a query whose own filter carries
// `requiresOwnerOrAdmin` as a top-level conjunct, evaluated in-process
// against the auth envelope (dsl/identity/queries.memql). A reader who calls
// them gets zero rows FROM THE ENGINE, not from this file. That is the whole
// reason these are functions here rather than `useLiveCollection` calls in a
// component: the gate has to stay on the cluster's side of the wire.
//
// It is also why there is no live feed on these three sections. A credential
// list that streamed would need a subscription whose admission is decided per
// row, and these concepts have no tier for that to read -- so they are asked
// on open and re-asked by a control somebody presses. The panels say when
// they looked.

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

/**
 * People who can sign in, so the token console has somebody to fan out over.
 *
 * `searchUsers` and not `activeUsers`: the latter is @serverOnly and answers a
 * client caller with a refusal. No `active` argument either -- it is
 * arg-conditional, and omitting it is how you ask for deactivated accounts
 * too. A token belonging to a deactivated person is exactly the one an
 * operator is looking for.
 *
 * WINDOW: one page, newest first, capped by the engine's MaxResults (500 by
 * default) rather than by a paginate directive -- `searchUsers` declares
 * `sort` and no `paginate`, so the unmarked-list backstop does not apply. A
 * cluster with more people than that loses the tail, and `useTokenFacts` says
 * so in surface rather than presenting a partial list as the whole.
 */
export async function readPeople(conn: Connection, signal: AbortSignal): Promise<Row[]> {
  const result = await conn.query.searchUsers({}, { signal });
  return [...result.rows()];
}

/**
 * One person's personal access tokens, revoked ones included.
 *
 * PER USER, because that is the only admin-gated PAT read the cluster
 * publishes: `patIdentitiesForUser` takes a required userId and there is no
 * `patIdentities()`. The shape is `patSummary`, which carries no keyHash --
 * the shape is the boundary, not this file's discretion.
 */
export async function readTokensForUser(
  conn: Connection,
  userId: string,
  signal: AbortSignal,
): Promise<Row[]> {
  const result = await conn.query.patIdentitiesForUser({ userId }, { signal });
  return [...result.rows()];
}

/**
 * The cluster's node credentials -- one row per node that has bootstrapped.
 *
 * `nodeTokenIdentitiesAdmin`, not `nodeTokenIdentities`: the original is
 * @serverOnly AND projects identityFull, which carries the credentials object
 * keyHash and all. The admin twin gates itself on `requiresOwnerOrAdmin` and
 * projects the credential-free `nodeTokenSummary`, so the row that reaches a
 * browser cannot carry a secret whatever the caller's role. The @serverOnly
 * original stays for the verifier, which genuinely needs the hash.
 */
export async function readNodeTokens(conn: Connection, signal: AbortSignal): Promise<Row[]> {
  const result = await conn.query.nodeTokenIdentitiesAdmin({}, { signal });
  return [...result.rows()];
}

/**
 * The audit trail, newest first, optionally narrowed to one category.
 *
 * OWNER ONLY, and that is stricter than the sections that call it. The filter
 * is `actor.isClusterOwner==true`, so an admin gets ZERO ROWS rather than an
 * error -- which is why every caller gates the call on the role and says in
 * surface that the history is the owner's to read. An empty list rendered as
 * "nothing has happened" would be a lie told to exactly the person who cannot
 * check it.
 *
 * `category` pushes the narrowing into SQL rather than filtering a page in the
 * browser, which matters: the query pages at 50, and a client-side filter over
 * one page would quietly claim "no key has ever been rotated" whenever the
 * last 50 events were all sign-ins.
 */
export async function readAuditEvents(
  conn: Connection,
  category: string,
  signal: AbortSignal,
): Promise<Row[]> {
  const args = category === "" ? {} : { category };
  const result = await conn.query.recentAuditEvents(args, { signal });
  return [...result.rows()];
}

/** The singleton runtime settings row (id="cluster"). */
export async function readClusterSettings(
  conn: Connection,
  signal: AbortSignal,
): Promise<Row | null> {
  const result = await conn.query.clusterSettingsCurrent({}, { signal });
  return [...result.rows()][0] ?? null;
}

// Revocation is NOT here, and its absence is deliberate rather than an
// omission: it is a write, and every write these sections make goes through
// IdentityAdminClient so the gate is the cluster's rather than a boolean in a
// browser. See settingsWrites.ts.

// ---------------------------------------------------------------------------
// The signing keys, over plain HTTP
// ---------------------------------------------------------------------------

/** One published key. Mirrors component/identity/jwks.go's JWK, minus the
 *  members this surface has no use for. */
export interface SigningKey {
  kid: string;
  kty: string;
  crv: string;
  alg: string;
  use: string;
}

/**
 * Read `/.well-known/jwks.json` off an identity origin.
 *
 * NOT over the stream, and not because the stream is inconvenient: the JWKS
 * feed is a PUBLIC document by specification -- every verifier node in the
 * mesh fetches it unauthenticated to check a token's signature -- so a second,
 * gated copy of it on the stream would assert a confidentiality this data does
 * not have. Reading the same bytes a verifier reads is also the ONLY way to
 * tell an operator what verifiers are actually seeing.
 *
 * `origin` is the runtime config's identityUrl. Empty means this deployment
 * could not derive one; the caller renders that as an explained gap rather
 * than fetching a relative path that would resolve against the OS's own origin
 * and 404.
 *
 * No credential is sent. Attaching one would make a cross-origin read that
 * works today fail the moment an operator's deployment tightens its CORS
 * policy -- and the feed needs none.
 */
export async function fetchSigningKeys(
  origin: string,
  fetchImpl: typeof globalThis.fetch = globalThis.fetch,
  signal?: AbortSignal,
): Promise<SigningKey[]> {
  const base = origin.replace(/\/+$/, "");
  if (base === "") throw new Error("this deployment publishes no identity origin");

  const response = await fetchImpl(`${base}/.well-known/jwks.json`, {
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

/**
 * The keyset FINGERPRINT: the published `kid`s, sorted, joined.
 *
 * This is what the Keys section actually compares, and sorting is what makes
 * the comparison honest. A JWKS feed states no order, so two replicas holding
 * an identical keyset can serve it in different orders; comparing the raw
 * document would report a disagreement that does not exist, and an operator
 * who chased one false alarm will not chase the real one.
 */
export function keysetFingerprint(keys: readonly SigningKey[]): string {
  return keys
    .map((k) => k.kid)
    .filter((kid) => kid !== "")
    .sort()
    .join(" ");
}
