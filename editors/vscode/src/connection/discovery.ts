// RFC 8414 discovery: ask the cluster where its endpoints are (memql#4624).
//
// WHAT THIS REPLACES. The extension derived the identity host purely by
// convention (`identity.<domain>`) and hard-coded `/authorize`, `/oauth/token`,
// `/device/code` and `/.well-known/jwks.json` (endpoint.ts). The cluster has
// published RFC 8414 metadata at `/.well-known/oauth-authorization-server` the
// whole time and nothing read it. Three things followed:
//
//   - An identity service NOT at `identity.<domain>` was unreachable without
//     hand-editing clusters.yaml.
//   - Pasting the API host into the domain field ("api.example.com") composed
//     `api.api.example.com`, failed a probe with a generic "no answer within
//     10s", and dead-ended at sign-in much later.
//   - `cluster.issuer` was never written by anything, which is what made the
//     claim probe in memql#4620 unreachable.
//
// AND WHAT IT REPLACES IN THE SIGN-IN PATH. flow.ts opened a browser and parked
// for 600 seconds without ever asking whether the issuer existed. A wrong
// domain, an unreachable host, a bad certificate, an old cluster or a non-MemQL
// host all cost the full deadline and were then blamed on the browser. A
// cluster predating the `memql-vscode` built-in client renders an HTML 400
// "Unknown client" page that is never redirected, so there is no callback and
// no OAuth error envelope -- just silence. One round trip here answers all of
// them, which is what the device path already did.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/** The subset of `fetch` this module needs. */
export type DiscoveryResponseLike = {
  ok: boolean;
  status: number;
  text: () => Promise<string>;
};
export type DiscoveryFetch = (
  url: string,
  init: { method: string; headers: Record<string, string> },
) => Promise<DiscoveryResponseLike>;

/** Where a cluster's OAuth endpoints actually are. */
export interface IssuerMetadata {
  /** The issuer identifier, verbatim from the document. Written to `issuer`. */
  issuer: string;
  authorizationEndpoint: string;
  tokenEndpoint: string;
  /** "" when the cluster does not advertise the device grant. */
  deviceAuthorizationEndpoint: string;
  jwksUri: string;
}

export type DiscoveryFailureKind =
  /** Nothing answered, or not in time. */
  | "unreachable"
  /** Something answered and it was not an authorization server. */
  | "notAnIssuer"
  /** It is an authorization server and it disagrees about who it is. */
  | "issuerMismatch";

export interface DiscoveryFailure {
  ok: false;
  kind: DiscoveryFailureKind;
  /** A sentence for a human, naming the URL and what came back. */
  message: string;
}

export type DiscoveryResult = ({ ok: true } & IssuerMetadata) | DiscoveryFailure;

const WELL_KNOWN = "/.well-known/oauth-authorization-server";

/**
 * How long one metadata fetch may take.
 *
 * BOUNDED HERE RATHER THAN BY THE CALLER, because this call sits in FRONT of a
 * sign-in whose whole complaint was an unbounded wait. `fetch` has no default
 * timeout: a host that completes a TCP handshake and then says nothing, or a
 * firewall that drops packets rather than refusing them, hangs for as long as
 * the OS allows. A pre-flight that can outlast the 600-second deadline it
 * replaced would have made the problem worse rather than better.
 *
 * Ten seconds matches `CONNECT_PROBE_TIMEOUT_MS`, the other network call this
 * extension makes before an operator has committed to anything.
 *
 * Raced rather than passed as a signal, deliberately: the fetch is injected,
 * and an injected implementation that ignored the signal -- every test fake
 * does -- would silently restore the unbounded wait. A race binds regardless of
 * what the caller supplied.
 */
const DISCOVERY_TIMEOUT_MS = 10_000;

type TimeoutRace<T> = { timedOut: true } | { timedOut: false; value: T };

async function withTimeout<T>(work: Promise<T>, ms: number): Promise<TimeoutRace<T>> {
  let bell: ReturnType<typeof setTimeout> | undefined;
  const alarm = new Promise<TimeoutRace<T>>((resolve) => {
    bell = setTimeout(() => resolve({ timedOut: true }), ms);
    // Never hold the process open for a timer nobody is waiting on.
    (bell as unknown as { unref?: () => void }).unref?.();
  });
  try {
    return await Promise.race([work.then((value) => ({ timedOut: false as const, value })), alarm]);
  } finally {
    if (bell !== undefined) clearTimeout(bell);
  }
}

/**
 * Fetch and validate a cluster's authorization-server metadata.
 *
 * ONE REDIRECTION IS FOLLOWED, AND EXACTLY ONE. The cluster serves the same
 * document from the API host as a POINTER: it carries
 * `issuer: https://identity.<domain>` rather than its own URL, which is right
 * -- `api.<domain>` is not an issuer and nothing signs tokens there -- and
 * which also means a strict RFC 8414 §3.3 match against the fetched URL fails
 * there. So a document naming a different issuer is treated as a signpost:
 * re-fetch at the issuer it names, and require a strict match THERE. That is
 * what turns "the operator pasted the API host" from a dead end into a
 * successful registration.
 *
 * The second fetch never redirects again. A chain would let a hostile or
 * misconfigured host walk a client to an issuer of its choosing, and one hop
 * is all the legitimate topology needs.
 */
export async function discoverIssuer(
  baseUrl: string,
  fetchImpl: DiscoveryFetch,
): Promise<DiscoveryResult> {
  const first = await fetchMetadata(baseUrl, fetchImpl);
  if (!first.ok) return first;

  const fetchedFrom = normalizeBase(baseUrl);
  const named = normalizeBase(first.metadata.issuer);
  if (named === fetchedFrom) return { ok: true, ...first.metadata };

  // The signpost case. Re-fetch at the issuer the document names.
  const second = await fetchMetadata(named, fetchImpl);
  if (!second.ok) {
    return {
      ok: false,
      kind: second.kind,
      message:
        `${fetchedFrom} points at ${named} as its issuer, and ${named} ` +
        `could not be used: ${second.message}`,
    };
  }
  if (normalizeBase(second.metadata.issuer) !== named) {
    return {
      ok: false,
      kind: "issuerMismatch",
      message:
        `${named} publishes metadata naming a different issuer ` +
        `(${second.metadata.issuer}). A chain of redirections is refused.`,
    };
  }
  return { ok: true, ...second.metadata };
}

type MetadataFetch =
  | { ok: true; metadata: IssuerMetadata }
  | DiscoveryFailure;

async function fetchMetadata(
  baseUrl: string,
  fetchImpl: DiscoveryFetch,
): Promise<MetadataFetch> {
  const base = normalizeBase(baseUrl);
  if (base === "") {
    return { ok: false, kind: "unreachable", message: "no identity service URL is known" };
  }
  const url = base + WELL_KNOWN;

  let response: DiscoveryResponseLike;
  try {
    const raced = await withTimeout(
      fetchImpl(url, { method: "GET", headers: { accept: "application/json" } }),
      DISCOVERY_TIMEOUT_MS,
    );
    if (raced.timedOut) {
      return {
        ok: false,
        kind: "unreachable",
        message:
          `${url} did not answer within ${String(DISCOVERY_TIMEOUT_MS / 1000)}s ` +
          `(the host may not resolve, or a firewall may be dropping it)`,
      };
    }
    response = raced.value;
  } catch (err) {
    return { ok: false, kind: "unreachable", message: `${url} could not be reached (${errorText(err)})` };
  }
  if (!response.ok) {
    // A 404 here is the specific and common one: something is serving HTTP at
    // this host and it is not a MemQL identity service.
    return {
      ok: false,
      kind: response.status === 404 ? "notAnIssuer" : "unreachable",
      message:
        response.status === 404
          ? `${url} returned 404 -- that host is not a MemQL identity service`
          : `${url} returned ${response.status}`,
    };
  }

  const raw = await response.text().catch(() => "");
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(raw) as Record<string, unknown>;
  } catch {
    // An HTML page is what a wrong host, a captive portal or a proxy returns.
    return {
      ok: false,
      kind: "notAnIssuer",
      message: `${url} did not return JSON -- that host is not a MemQL identity service`,
    };
  }

  const issuer = stringField(parsed, "issuer");
  const authorizationEndpoint = stringField(parsed, "authorization_endpoint");
  const tokenEndpoint = stringField(parsed, "token_endpoint");
  if (issuer === "" || authorizationEndpoint === "" || tokenEndpoint === "") {
    return {
      ok: false,
      kind: "notAnIssuer",
      message:
        `${url} answered JSON without issuer / authorization_endpoint / token_endpoint, ` +
        `so it is not an RFC 8414 authorization server`,
    };
  }

  return {
    ok: true,
    metadata: {
      issuer,
      authorizationEndpoint,
      tokenEndpoint,
      deviceAuthorizationEndpoint: stringField(parsed, "device_authorization_endpoint"),
      jwksUri: stringField(parsed, "jwks_uri"),
    },
  };
}

/** Trailing slashes off, case-folded host, so two spellings of one URL match. */
export function normalizeBase(raw: string): string {
  const trimmed = raw.trim().replace(/\/+$/, "");
  if (trimmed === "") return "";
  try {
    const url = new URL(trimmed);
    return `${url.protocol}//${url.host}`.toLowerCase() + url.pathname.replace(/\/+$/, "");
  } catch {
    return trimmed.toLowerCase();
  }
}

function stringField(doc: Record<string, unknown>, name: string): string {
  const value = doc[name];
  return typeof value === "string" ? value.trim() : "";
}

function errorText(err: unknown): string {
  if (err instanceof Error) {
    // Node's undici hides the real reason in `.cause` (memql#4619).
    const cause = (err as { cause?: unknown }).cause;
    if (cause instanceof Error && cause.message !== "") return `${err.message}: ${cause.message}`;
    return err.message;
  }
  return String(err);
}
