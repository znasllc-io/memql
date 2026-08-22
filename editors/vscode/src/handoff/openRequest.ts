// What the portal sends, and what this extension accepts (design 4.1, 4.3).
//
// ONE SHAPE, VERSIONED. The handler refuses any `v` but "1" rather than
// guessing at keys it does not know: a portal that needs a different shape
// bumps the version. Every refusal names the field, because the person
// reading the toast is debugging a link, not this code.
//
// HOSTILE INPUT IS THE NORMAL CASE. Any web page can fire a vscode:// link at
// this handler, so nothing here is trusted: lengths are capped, the path must
// be exactly /open, duplicate keys are refused, and `name` may not carry a
// path separator or a control character. `originPath` never comes from the
// link at all -- the catalog supplies it after the cluster is matched.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4251

import { normalizeDomain } from "../connection/endpoint.js";

export const OPEN_REQUEST_VERSION = "1";
const MAX_QUERY = 4096;
const MAX_NAME = 512;
const KIND = /^[a-z][A-Za-z0-9]{0,31}$/;
const HOST = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$/;
const CONTROL = /[\u0000-\u001f\u007f]/;

export interface OpenRequest {
  version: "1";
  domain: string;
  kind: string;
  name: string;
}

export type OpenRequestError = { error: string };

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
  const name = one("name");
  if (typeof name !== "string") return name;
  if (name.length > MAX_NAME || CONTROL.test(name) || /[\\/]/.test(name) || name.includes("..")) {
    return { error: "name is not a construct name" };
  }
  return { version: "1", domain, kind, name };
}
