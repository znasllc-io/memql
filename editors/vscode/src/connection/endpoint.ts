// Deriving a WebSocket URL from a cluster's stored gRPC endpoint.
//
// clusters.yaml records a gRPC address (host:port) because the cockpit dials
// native gRPC. This extension speaks the /memql/ws bridge instead, so the
// address is lifted to a URL here -- one place, so the rest of the extension
// never reasons about transport.

import type { ClusterConfig } from "../clusters/model.js";

const BRIDGE_PATH = "/memql/ws";

// Loopback hosts are served over plain HTTP by a raw port-forward, which is a
// debugging path rather than the front door. Everything else is TLS.
function isLoopback(host: string): boolean {
  return host === "localhost" || host === "127.0.0.1" || host === "::1";
}

export function webSocketUrlFor(cluster: ClusterConfig): string {
  const raw = cluster.endpoint.trim();
  if (raw === "") {
    throw new Error(`cluster "${cluster.name}": endpoint is empty`);
  }

  // An operator may store a full URL. Honor it, adding the bridge path when
  // it carries none.
  if (raw.startsWith("wss://") || raw.startsWith("ws://")) {
    const url = new URL(raw);
    if (url.pathname === "" || url.pathname === "/") {
      url.pathname = BRIDGE_PATH;
    }
    return url.toString();
  }

  const lastColon = raw.lastIndexOf(":");
  const hasPort = lastColon > 0 && /^\d+$/.test(raw.slice(lastColon + 1));
  const host = hasPort ? raw.slice(0, lastColon) : raw;
  const port = hasPort ? raw.slice(lastColon + 1) : "";

  const scheme = isLoopback(host) ? "ws" : "wss";
  // 443 is the front door's implicit port; emitting it produces a URL that
  // works but reads as a misconfiguration in logs and error messages.
  const authority = port === "" || port === "443" ? host : `${host}:${port}`;
  return `${scheme}://${authority}${BRIDGE_PATH}`;
}
