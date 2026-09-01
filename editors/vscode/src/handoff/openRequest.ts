// What the portal and MemQL OS send, and what this extension accepts
// (design 4.1, 4.3; the artifact target is memql#4748).
//
// ONE SHAPE, VERSIONED. The handler refuses any `v` but "1" rather than
// guessing at keys it does not know: a caller that needs a different shape
// bumps the version. Every refusal names the field, because the person
// reading the toast is debugging a link, not this code.
//
// HOSTILE INPUT IS THE NORMAL CASE. Any web page can fire a vscode:// link at
// this handler, so nothing here is trusted: lengths are capped, the path must
// be exactly /open, duplicate keys are refused, and neither `name` nor `id`
// may carry a path separator or a control character. `originPath` never comes
// from the link at all -- the catalog supplies it after the cluster is matched.
//
// TWO TARGETS, ONE LINK SHAPE, AND A DISCRIMINATED UNION RATHER THAN AN
// OPTIONAL FIELD. A construct is addressed by NAME (a registry key the catalog
// resolves) and an artifact by ID (a row id the cluster resolves), and those
// are different things that happen to both be strings. Carrying the artifact
// id in `name` would run it through the construct-name validator, which is not
// the rule an id should be judged by; carrying both as optional fields on one
// interface would push "which one is set" out to every consumer, where
// forgetting it reads as a link that quietly does the wrong thing rather than
// one that is refused. So the parse decides once, and `target` is what every
// consumer switches on.
//
// A LINK CARRYING BOTH IS REFUSED. It states two addresses for one open, and
// there is no reading of it that is not a guess -- the caller is confused about
// what it is asking for, and honouring either half would hide that.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4251 #4748

import { normalizeDomain } from "../connection/endpoint.js";

export const OPEN_REQUEST_VERSION = "1";
const MAX_QUERY = 4096;
const MAX_NAME = 512;
const KIND = /^[a-z][A-Za-z0-9]{0,31}$/;
const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
const CONTROL = /[\u0000-\u001f\u007f]/;

/** The one `kind` that addresses a Library row rather than a loaded construct. */
export const ARTIFACT_KIND = "artifact";

/**
 * What an artifact id may look like.
 *
 * BOTH SPELLINGS, because both are real: the OS forwards whatever the row
 * carried, which is the canonical `v1:library:artifact:<shortId>` for a row
 * read out of the graph and a bare short id everywhere the engine has already
 * bare-ified it (docs/public/concepts/identifiers.md). This extension never
 * composes, splits or compares one -- it hands it to the cluster, which is the
 * only side that knows which form it is holding.
 *
 * The character class is what makes forwarding it safe: alphanumerics, colon,
 * underscore and hyphen, with the first character alphanumeric so a leading
 * `-` can never be read as a flag by anything downstream. A dot is
 * deliberately ABSENT -- an id has no use for one, and its absence is what
 * makes `..` unrepresentable rather than merely checked for. 255 characters is
 * far beyond any id the engine mints and short enough to render in a toast.
 */
const ARTIFACT_ID = /^[A-Za-z0-9][A-Za-z0-9:_-]{0,254}$/;

export interface OpenConstructRequest {
  version: "1";
  target: "construct";
  domain: string;
  kind: string;
  name: string;
}

export interface OpenArtifactRequest {
  version: "1";
  target: "artifact";
  domain: string;
  kind: "artifact";
  id: string;
}

export type OpenRequest = OpenConstructRequest | OpenArtifactRequest;

export type OpenRequestError = { error: string };

/**
 * Whether a string is an artifact id this extension will forward to a cluster.
 *
 * EXPORTED so the rule can be tested on its own and stated once. The three
 * checks after the pattern are belt and braces: the character class already
 * excludes every one of them, and they are here so that a later edit widening
 * that class -- to admit a dot, say -- cannot silently admit `../..` with it.
 */
export function isArtifactId(value: string): boolean {
  if (!ARTIFACT_ID.test(value)) return false;
  if (CONTROL.test(value)) return false;
  if (/[\\/]/.test(value)) return false;
  return !value.includes("..");
}

export function parseOpenRequest(uri: { path: string; query: string }): OpenRequest | OpenRequestError {
  // TRUNCATED, because this string is rendered into a TOAST and the path is
  // attacker-supplied by the same route the query is -- which is capped on the
  // next line. 64 characters is enough to recognise the link that was clicked
  // and far short of anything worth pasting into a notification.
  if (uri.path !== "/open") {
    return { error: `unsupported path ${uri.path.slice(0, 64)}; this extension opens constructs at /open` };
  }
  if (uri.query.length > MAX_QUERY) return { error: "the query is longer than this handler accepts" };
  const params = new URLSearchParams(uri.query);
  const one = (key: string): string | OpenRequestError => {
    const all = params.getAll(key);
    if (all.length === 0) return { error: `missing ${key}` };
    if (all.length > 1) return { error: `${key} appears more than once` };
    const v = all[0]!.trim();
    return v === "" ? { error: `missing ${key}` } : v;
  };
  // Whether the link says anything at all under a key. Blank counts as
  // SILENCE, which is the reading `one` above already takes -- one definition
  // of "carries a key", so `kind=artifact&name=` is a link with no name rather
  // than one this function refuses for a reason the next reader cannot find.
  const carries = (key: string): boolean => params.getAll(key).some((v) => v.trim() !== "");
  const v = one("v");
  if (typeof v !== "string") return v;
  if (v !== OPEN_REQUEST_VERSION) return { error: `unsupported link version v=${v}; this extension accepts v=1` };
  const cluster = one("cluster");
  if (typeof cluster !== "string") return cluster;
  const domain = normalizeDomain(cluster).toLowerCase();
  if (!HOST.test(domain)) return { error: `cluster ${JSON.stringify(cluster)} is not a domain` };
  const kind = one("kind");
  if (typeof kind !== "string") return kind;
  if (!KIND.test(kind)) return { error: `kind ${JSON.stringify(kind)} is not a construct kind` };

  if (kind === ARTIFACT_KIND) {
    if (carries("name")) {
      return { error: "kind=artifact is addressed by id, and this link also carries a name" };
    }
    const id = one("id");
    if (typeof id !== "string") return id;
    // NAMED, NEVER ECHOED. A rejected id is attacker-supplied and this sentence
    // is rendered into a toast; the field name plus the link the caller still
    // has is enough to debug it, and `name is not a construct name` sets the
    // precedent.
    if (!isArtifactId(id)) return { error: "id is not an artifact id" };
    return { version: "1", target: "artifact", domain, kind: ARTIFACT_KIND, id };
  }

  if (carries("id")) {
    return { error: `kind ${JSON.stringify(kind)} is addressed by name, and this link also carries an id` };
  }
  const name = one("name");
  if (typeof name !== "string") return name;
  if (name.length > MAX_NAME || CONTROL.test(name) || /[\\/]/.test(name) || name.includes("..")) {
    return { error: "name is not a construct name" };
  }
  return { version: "1", target: "construct", domain, kind, name };
}

/**
 * What a request names, for a log line or a toast.
 *
 * ONE RENDERING, because the handoff writes this phrase into a dozen channel
 * lines and every one of them predated the union. Inlining
 * `${request.kind} ${request.name}` per site is a compile error per site once
 * `name` is no longer always there -- and the tempting repair, widening each to
 * its own `'name' in request ? ... : ...`, is a dozen places for the two
 * targets to start describing themselves differently.
 */
export function describeOpenRequest(request: OpenRequest): string {
  return request.target === "artifact" ? `${request.kind} ${request.id}` : `${request.kind} ${request.name}`;
}

/**
 * The link a request came from, recomposed.
 *
 * The replay after a window reload goes back through the uri handler as a URI
 * rather than jumping into the landing step, so that ONE handler decides again
 * against whatever this window turns out to be. That makes composing the link
 * part of this module's contract rather than string-building at the call site,
 * and it lives beside the parser so the two are edited together: the test
 * asserts a round trip through both.
 */
export function openRequestUri(request: OpenRequest): string {
  const address =
    request.target === "artifact"
      ? `id=${encodeURIComponent(request.id)}`
      : `name=${encodeURIComponent(request.name)}`;
  return (
    `vscode://znasllc.memql/open?v=${OPEN_REQUEST_VERSION}` +
    `&cluster=${encodeURIComponent(request.domain)}` +
    `&kind=${encodeURIComponent(request.kind)}` +
    `&${address}`
  );
}
