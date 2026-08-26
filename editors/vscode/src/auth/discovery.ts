// RFC 8414 discovery: asking the cluster where its endpoints are, instead of
// composing them from a convention (memql#4624).
//
// -----------------------------------------------------------------------------
// WHY THIS EXISTS
// -----------------------------------------------------------------------------
//
// The extension derived the identity host purely by convention
// (`identity.<domain>`) and hard-coded `/authorize`, `/oauth/token`,
// `/device/code` and `/.well-known/jwks.json`. The cluster has published RFC
// 8414 metadata at `GET /.well-known/oauth-authorization-server` the whole
// time -- from the identity host AND from `api.<domain>` -- and nothing read
// it.
//
// Two things followed, and they are the same defect twice:
//
//   1. A wrong host was not caught. Pasting `api.example.com` into the domain
//      field composed `api.api.example.com:443`; the probe failed with a
//      generic "no answer within 10s", the operator clicked "Save anyway", and
//      sign-in dead-ended much later.
//   2. An identity service NOT at `identity.<domain>` was unrecoverable without
//      hand-editing clusters.yaml.
//
// -----------------------------------------------------------------------------
// WHY IT IS ALSO THE PRE-FLIGHT
// -----------------------------------------------------------------------------
//
// The browser flow used to open a browser and park for 600 seconds without
// ever asking whether the issuer exists. A wrong domain, an unreachable host, a
// bad certificate, an old cluster and a non-memQL host all cost the full
// deadline and were then blamed on the browser: "No sign-in callback arrived
// within 600 seconds... or it could not reach 127.0.0.1" -- wrong in every one
// of those cases.
//
// A cluster predating the `memql-vscode` built-in client is the sharpest
// example: /authorize renders an HTML 400 "Unknown client" page that is never
// redirected, so there is no callback AND no OAuth error envelope. Just
// silence, for ten minutes. One GET answers it in a round trip, which is what
// the device path has always done.
//
// -----------------------------------------------------------------------------
// THE TWO SERVER FACTS THIS HAS TO ACCOMMODATE
// -----------------------------------------------------------------------------
//
// The copy served from `api.<domain>` reports `issuer:
// https://identity.<domain>` rather than its own URL, because it is a POINTER
// to the identity service rather than a claim to be one. A strict RFC 8414
// issuer match against the fetch URL therefore fails there, correctly. So a
// document whose issuer names somewhere else is FOLLOWED -- refetched from the
// issuer it names and validated strictly there -- rather than rejected. One
// hop only; a second would be a redirect chain a hostile document could steer.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import type { FetchLike } from "../connection/credentials.js";
// The SHARED unwrapper, not a local one (memql#4517's reasoning): undici wraps
// every transport failure in a bare TypeError("fetch failed") and puts the real
// reason -- ENOTFOUND, ECONNREFUSED, a certificate error -- in `.cause` or
// `.errors`. A naive `err.message` here would reduce a wrong hostname to "fetch
// failed", which is the exact sentence this pre-flight exists to replace.
import { errorText } from "./errors.js";

/** RFC 8414 §3. Served by identity, and by the bff as a pointer. */
export const OAUTH_METADATA_PATH = "/.well-known/oauth-authorization-server";

/** How long a pre-flight may take. Short: it is one GET, and its whole purpose
 *  is to fail faster than the 600-second callback deadline it replaces. */
export const DISCOVERY_TIMEOUT_MS = 10_000;

/** The endpoints a sign-in needs, as the cluster itself names them. */
export interface AuthorizationServerMetadata {
  issuer: string;
  authorizationEndpoint: string;
  tokenEndpoint: string;
  deviceAuthorizationEndpoint?: string;
  jwksUri?: string;
  codeChallengeMethodsSupported: string[];
  grantTypesSupported: string[];
}

export type DiscoveryOutcome =
  | { kind: "ok"; metadata: AuthorizationServerMetadata }
  /** The host answered, but not with an authorization server's metadata. */
  | { kind: "notAnAuthorizationServer"; detail: string }
  /** The host could not be reached at all -- DNS, TLS, connection, timeout. */
  | { kind: "unreachable"; detail: string }
  /** Reached, but answered an HTTP error. */
  | { kind: "refused"; status: number; detail: string };

export interface DiscoverRequest {
  /** Where to look first: an identity base URL, or the API front door. */
  baseUrl: string;
  fetch: FetchLike;
}

/**
 * discoverAuthorizationServer fetches and validates the cluster's RFC 8414
 * metadata, following one issuer hop.
 *
 * NEVER THROWS -- every failure is a value carrying a sentence a human can act
 * on, because the entire point is to replace an error that named the wrong
 * cause with one that names the right one.
 */
export async function discoverAuthorizationServer(
  req: DiscoverRequest,
): Promise<DiscoveryOutcome> {
  const first = await fetchMetadata(req.baseUrl, req.fetch);
  if (first.kind !== "ok") return first;

  // FOLLOW A POINTER, ONCE. The bff's copy names the identity service rather
  // than itself; that is not a defect, it is what makes `api.<domain>` a usable
  // starting point for a client that only knows the front door.
  const named = normalizeBase(first.metadata.issuer);
  if (named !== "" && named !== normalizeBase(req.baseUrl)) {
    const second = await fetchMetadata(named, req.fetch);
    if (second.kind !== "ok") return second;
    // At the issuer it names, the match must be exact (RFC 8414 §3.3). A
    // document that points somewhere else AGAIN is a chain, and a chain is
    // something a hostile document could steer.
    if (normalizeBase(second.metadata.issuer) !== named) {
      return {
        kind: "notAnAuthorizationServer",
        detail:
          `${named} publishes metadata for a different issuer ` +
          `(${second.metadata.issuer}). Discovery follows one hop, not a chain.`,
      };
    }
    return second;
  }
  return first;
}

async function fetchMetadata(baseUrl: string, doFetch: FetchLike): Promise<DiscoveryOutcome> {
  const base = normalizeBase(baseUrl);
  if (base === "") {
    return { kind: "unreachable", detail: "no identity host is known for this cluster" };
  }
  const url = `${base}${OAUTH_METADATA_PATH}`;

  let response;
  try {
    response = await doFetch(url, {
      method: "GET",
      headers: { accept: "application/json" },
      body: "",
    });
  } catch (err) {
    return { kind: "unreachable", detail: errorText(err) };
  }
  if (!response.ok) {
    return {
      kind: "refused",
      status: response.status,
      detail: `${url} answered ${response.status}`,
    };
  }

  let body: string;
  try {
    body = await response.text();
  } catch (err) {
    return { kind: "unreachable", detail: errorText(err) };
  }

  let raw: unknown;
  try {
    raw = JSON.parse(body);
  } catch {
    // A LOGIN PAGE IS THE COMMON CASE and it deserves its own sentence: a
    // captive portal, a proxy, or simply a host that is not a MemQL cluster
    // answers 200 with HTML. "Not JSON" is what an operator can act on.
    return {
      kind: "notAnAuthorizationServer",
      detail: `${url} did not answer with JSON (got ${describeBody(body)})`,
    };
  }
  const metadata = readMetadata(raw);
  if (metadata === undefined) {
    return {
      kind: "notAnAuthorizationServer",
      detail: `${url} answered JSON without the RFC 8414 fields an authorization server publishes`,
    };
  }
  return { kind: "ok", metadata };
}

function readMetadata(raw: unknown): AuthorizationServerMetadata | undefined {
  if (typeof raw !== "object" || raw === null) return undefined;
  const doc = raw as Record<string, unknown>;
  const issuer = str(doc.issuer);
  const authorizationEndpoint = str(doc.authorization_endpoint);
  const tokenEndpoint = str(doc.token_endpoint);
  // issuer + token_endpoint are the minimum that makes this an authorization
  // server rather than an arbitrary JSON document.
  if (issuer === "" || tokenEndpoint === "") return undefined;
  return {
    issuer,
    authorizationEndpoint,
    tokenEndpoint,
    deviceAuthorizationEndpoint: emptyToUndefined(str(doc.device_authorization_endpoint)),
    jwksUri: emptyToUndefined(str(doc.jwks_uri)),
    codeChallengeMethodsSupported: strList(doc.code_challenge_methods_supported),
    grantTypesSupported: strList(doc.grant_types_supported),
  };
}

/**
 * describeDiscoveryFailure is the sentence shown instead of a ten-minute
 * spinner. Each names the actual cause -- which the callback-timeout message it
 * replaces could not, because by then the only fact left was that nothing
 * arrived.
 */
export function describeDiscoveryFailure(host: string, outcome: DiscoveryOutcome): string {
  switch (outcome.kind) {
    case "ok":
      return "";
    case "unreachable":
      return (
        `${host} could not be reached (${outcome.detail}). Check the domain, and that the ` +
        `cluster is running and its certificate is valid.`
      );
    case "refused":
      return `${host} answered ${outcome.status}. That host is reachable but is not serving MemQL identity.`;
    case "notAnAuthorizationServer":
      return (
        `${host} is reachable but does not look like a MemQL identity service ` +
        `(${outcome.detail}). Check the domain.`
      );
  }
}

/** Does this cluster support the browser flow this extension wants to run? */
export function supportsAuthorizationCodeWithS256(m: AuthorizationServerMetadata): boolean {
  const grants = m.grantTypesSupported;
  const methods = m.codeChallengeMethodsSupported;
  // An EMPTY list means "not advertised", which RFC 8414 leaves open -- and
  // refusing on a missing optional field would reject a conformant server. Only
  // a list that is present AND lacks the value is a no.
  const grantOk = grants.length === 0 || grants.includes("authorization_code");
  const pkceOk = methods.length === 0 || methods.includes("S256");
  return grantOk && pkceOk;
}

export function supportsDeviceCode(m: AuthorizationServerMetadata): boolean {
  if (m.deviceAuthorizationEndpoint === undefined) return false;
  const grants = m.grantTypesSupported;
  return grants.length === 0 || grants.includes("urn:ietf:params:oauth:grant-type:device_code");
}

/** normalizeBase trims trailing slashes and adds https:// when a bare host was
 *  given, so a domain field and a stored issuer compose identically. */
export function normalizeBase(value: string | undefined): string {
  const trimmed = (value ?? "").trim().replace(/\/+$/, "");
  if (trimmed === "") return "";
  if (/^https?:\/\//i.test(trimmed)) return trimmed;
  return `https://${trimmed}`;
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}
function strList(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
}
function emptyToUndefined(v: string): string | undefined {
  return v === "" ? undefined : v;
}
function describeBody(body: string): string {
  const head = body.trimStart().slice(0, 40).replace(/\s+/g, " ");
  if (/^<!doctype html|^<html/i.test(body.trimStart())) return "an HTML page";
  return head === "" ? "an empty body" : `"${head}..."`;
}
