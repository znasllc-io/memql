// WebSocket readyState constants as plain numbers.
//
// WHY NOT `WebSocket.OPEN`: that dereferences the GLOBAL `WebSocket`. Both
// operands of a comparison are evaluated, so on a host with no global --
// notably the VS Code extension host, which is Node 20 (no global WebSocket
// below Node 22) -- `socket.readyState === WebSocket.OPEN` throws
// `ReferenceError: WebSocket is not defined` BEFORE readyState is ever
// consulted. That defeats the `webSocketFactory` escape hatch entirely: a
// consumer can inject a perfectly good `ws` socket and the SDK still dies
// reaching for a global it was told not to need.
//
// The values are fixed by the WHATWG WebSocket standard and are identical
// across browsers, `ws`, and undici, so comparing against the numeric literal
// is exactly equivalent and depends on nothing ambient.
//
// Guarded reads (`typeof WebSocket !== "undefined"` before use, as in the
// default socket factories) are fine and stay as they are -- the hazard is the
// UNGUARDED dereference.

export const WS_CONNECTING = 0;
export const WS_OPEN = 1;
export const WS_CLOSING = 2;
export const WS_CLOSED = 3;
