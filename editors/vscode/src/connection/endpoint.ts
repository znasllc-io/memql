// Deriving a WebSocket URL from a cluster's stored gRPC endpoint.
//
// clusters.yaml records a gRPC address (host:port) because the cockpit dials
// native gRPC. This extension speaks the /memql/ws bridge instead, so the
// address is lifted to a URL here -- one place, so the rest of the extension
// never reasons about transport.

import type { ClusterConfig } from "../clusters/model.js";

const BRIDGE_PATH = "/memql/ws";

// Loopback hosts are served over plain HTTP by a raw port-forward, which is a
// debugging path rather than the front door. Everything else is TLS. Callers
// pass the bracket-stripped form for an IPv6 literal (see webSocketUrlFor).
function isLoopback(host: string): boolean {
  return host === "localhost" || host === "127.0.0.1" || host === "::1";
}

// Matches a leading `<scheme>://`. Used both to detect an explicit URL and to
// name the offending scheme when it isn't ws/wss (e.g. an operator pasting
// the cockpit's `https://` domain instead of the gRPC host:port).
const SCHEME_RE = /^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//;

// Matches a bracketed IPv6 literal, optionally followed by :<port>, e.g.
// "[::1]" or "[::1]:50051". Without the brackets, a bare IPv6 address's own
// colons make it impossible to tell where the host ends and a port begins.
const BRACKETED_IPV6_RE = /^\[([^\]]+)\](?::(\d+))?$/;

// identityBaseUrlFor names the identity service a refresh exchange must POST
// to (memql#3385). It is a DIFFERENT host from the bff the stream dials --
// `cockpit.<domain>` serves the gRPC/WS front door, `identity.<domain>` serves
// /oauth/token and the JWKS feed.
//
// Derived rather than stored as a fifth field, because clusters.yaml already
// carries the two facts that name it:
//
//   1. `issuer` -- what the cockpit writes from the cluster's discovery
//      document (component/identity/discovery.go). Authoritative when present;
//      an operator whose identity service is not at identity.<domain> sets it.
//   2. `domain` -- the single value the add/edit flow collects. The endpoint is
//      already composed from it by convention (cockpit.<domain>:443), and this
//      is the same convention's other half.
//   3. Failing both, a `cockpit.<host>` endpoint implies its sibling.
//
// undefined when nothing names it. Guessing would mean POSTing a refresh token
// -- a 30-day credential -- at a host nobody nominated, so the honest answer is
// to say which field is missing and let the operator supply it.
export function identityBaseUrlFor(cluster: ClusterConfig): string | undefined {
  const issuer = (cluster.issuer ?? "").trim();
  if (issuer !== "") return issuer.replace(/\/+$/, "");

  const domain = (cluster.domain ?? "").trim().replace(/^\.+|\.+$/g, "");
  if (domain !== "") return `https://identity.${domain}`;

  const host = hostOf(cluster.endpoint);
  const COCKPIT = "cockpit.";
  if (host !== undefined && host.startsWith(COCKPIT) && host.length > COCKPIT.length) {
    return `https://identity.${host.slice(COCKPIT.length)}`;
  }
  return undefined;
}

// hostOf extracts the host from a stored endpoint, tolerating the same spellings
// webSocketUrlFor accepts. Returns undefined for anything it cannot read as a
// host -- callers treat that as "not derivable" rather than guessing.
function hostOf(rawEndpoint: string): string | undefined {
  const raw = rawEndpoint.trim();
  if (raw === "") return undefined;
  const schemeMatch = raw.match(SCHEME_RE);
  if (schemeMatch) {
    try {
      return new URL(raw).hostname;
    } catch {
      return undefined;
    }
  }
  const bracketed = raw.match(BRACKETED_IPV6_RE);
  if (bracketed) return undefined; // an IP literal names no DNS sibling
  const lastColon = raw.lastIndexOf(":");
  const hasPort = lastColon > 0 && /^\d+$/.test(raw.slice(lastColon + 1));
  const host = hasPort ? raw.slice(0, lastColon) : raw;
  return host === "" ? undefined : host;
}

export function webSocketUrlFor(cluster: ClusterConfig): string {
  const raw = cluster.endpoint.trim();
  if (raw === "") {
    throw new Error(`cluster "${cluster.name}": endpoint is empty`);
  }

  const schemeMatch = raw.match(SCHEME_RE);
  if (schemeMatch) {
    const scheme = schemeMatch[1].toLowerCase();
    if (scheme !== "ws" && scheme !== "wss") {
      throw new Error(
        `cluster "${cluster.name}": endpoint scheme must be ws:// or wss://, got "${scheme}://" -- store the gRPC host:port (or an explicit ws(s):// bridge URL), not a general-purpose URL`,
      );
    }
    // An operator may store a full URL. Honor it, adding the bridge path when
    // it carries none.
    const url = new URL(raw);
    if (url.pathname === "" || url.pathname === "/") {
      url.pathname = BRIDGE_PATH;
    }
    return url.toString();
  }

  let host: string;
  let port: string;
  const bracketed = raw.match(BRACKETED_IPV6_RE);
  if (bracketed) {
    host = `[${bracketed[1]}]`;
    port = bracketed[2] ?? "";
  } else {
    const lastColon = raw.lastIndexOf(":");
    const hasPort = lastColon > 0 && /^\d+$/.test(raw.slice(lastColon + 1));
    host = hasPort ? raw.slice(0, lastColon) : raw;
    port = hasPort ? raw.slice(lastColon + 1) : "";
  }

  const bareHost = host.startsWith("[") && host.endsWith("]") ? host.slice(1, -1) : host;
  const scheme = isLoopback(bareHost) ? "ws" : "wss";
  // 443 is the front door's implicit port; emitting it produces a URL that
  // works but reads as a misconfiguration in logs and error messages.
  const authority = port === "" || port === "443" ? host : `${host}:${port}`;
  return `${scheme}://${authority}${BRIDGE_PATH}`;
}
