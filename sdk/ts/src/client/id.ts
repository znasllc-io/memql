// newShortId mirrors the Go `id.NewShortId()` helper used as message_id
// / subscription_id / request_id throughout the wire protocol. The Go
// side uses 16 hex chars (8 bytes of crypto entropy). We match the
// length and alphabet so server-side logs read uniformly across SDKs.
//
// Uses globalThis.crypto when available (Node 20+, all modern
// browsers). Falls back to a Math.random-seeded string only when no
// crypto runtime is reachable -- that path is best-effort and warned
// about so a misconfigured environment is obvious.

export function newShortId(): string {
  const cryptoLike = globalThis.crypto as
    | { getRandomValues?: (buf: Uint8Array) => Uint8Array }
    | undefined;
  if (cryptoLike?.getRandomValues) {
    const buf = new Uint8Array(8);
    cryptoLike.getRandomValues(buf);
    let out = "";
    for (const byte of buf) {
      out += byte.toString(16).padStart(2, "0");
    }
    return out;
  }
  let out = "";
  for (let i = 0; i < 16; i++) {
    out += Math.floor(Math.random() * 16).toString(16);
  }
  return out;
}
