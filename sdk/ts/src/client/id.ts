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

// ---------------------------------------------------------------------------
// Bare ids at a comparison seam.
// ---------------------------------------------------------------------------
//
// WHY THIS EXISTS. A client can receive the SAME user's id in two different
// shapes, and comparing them raw is always false:
//
//   plan.requestedBy   "abc123"                    query ROW data, bare-ified
//                                                  on egress by WireBareifyData
//                                                  (component/grpc/server.go)
//   MyAccess.userId    "v1:identity:user:abc123"   a PROTO scalar, which the
//                                                  egress bare-ifier never
//                                                  walks -- it only rewrites
//                                                  query data
//
// That mismatch is what made a goal you had just created report "This goal
// belongs to someone else" (memql#4581), and it is invisible to a test whose
// fixtures use bare on both sides.
//
// The engine has always compared correctly -- `sameRowAuthzOwner` in
// component/memql/rowauthz_enforce.go normalises BOTH sides. These helpers are
// that same rule, for the client.
//
// THIS IS A COMPARISON SEAM ONLY. Do NOT use it to change what the client
// SENDS or the server STORES. Server-side DSL filters bind `actor.userId`
// canonically (component/auth/actor_envelope.go), so a query like
// `filter requestedBy==actor.userId` requires requestedBy to be STORED
// canonical. Writing bare ids would leave rows the owner filter can never
// match -- trading a broken ownership check for a silently empty list.

// canonicalIdPattern mirrors `wireCanonicalIdPattern` in
// component/memql/wire_bareids.go, byte for byte. The trailing `.+` REQUIRES a
// fourth (shortId) segment, so a bare 3-segment concept TYPE
// ("v1:cognition:space") does not match and survives untouched. The namespace
// admits `/` because a namespace is a directory path (memql#3898).
const canonicalIdPattern = /^v[0-9]+:[a-z0-9/]+:[a-zA-Z0-9_]+:.+/;

/**
 * bareShortId returns the bare short id when `value` is shaped like a canonical
 * node id, and `value` unchanged otherwise. Mirrors Go's `BareShortId`.
 *
 * A value that cannot be decomposed is returned as-is, so calling this on an
 * already-bare id is safe and idempotent.
 */
export function bareShortId(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "" || !canonicalIdPattern.test(trimmed)) return value;
  // Everything after the third colon. The shortId itself may contain colons,
  // which is why this joins the remainder rather than taking one segment.
  return trimmed.split(":").slice(3).join(":");
}

/**
 * sameEntityId reports whether two ids name the same entity, tolerating one
 * side being canonical and the other bare. Mirrors Go's `sameRowAuthzOwner`.
 *
 * An empty id on either side is NOT a match: a caller with no resolved
 * identity has made no ownership claim, and answering "true" there would show
 * one person another person's rows during the window before identity resolves.
 */
export function sameEntityId(a: string, b: string): boolean {
  const left = a.trim();
  const right = b.trim();
  if (left === "" || right === "") return false;
  if (left === right) return true;
  return bareShortId(left) === bareShortId(right);
}
