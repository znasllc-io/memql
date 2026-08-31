// The bridge endpoint, resolved for DISPLAY (memql#4744).
//
// The OS dials a RELATIVE path (`/_memql/ws`, `live/connection.tsx`) because
// component/edge serves the bundle, so the cluster is always the origin that
// served it. Showing an operator "/_memql/ws" answers nothing, and composing
// `wss://api.<domain>/memql/ws` from the runtime-config domain would print an
// endpoint this client does not use -- a second derivation that disagrees
// with the first is the failure mode the front-door work exists to prevent.
//
// So: resolve the path the SDK actually resolves, the way the SDK resolves
// it (`resolveWsUrl`), and display THAT. Never re-dialed.

export function resolveBridgeEndpoint(
  path: string,
  location: { protocol: string; host: string } | undefined,
): string {
  if (/^wss?:\/\//i.test(path)) return path;
  if (!location || !location.host) return path;
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${location.host}${path.startsWith("/") ? "" : "/"}${path}`;
}
