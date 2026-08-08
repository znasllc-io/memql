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
