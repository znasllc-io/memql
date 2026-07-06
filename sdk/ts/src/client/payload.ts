// Pure payload transforms shared by mutation call sites.
//
// These operate on the plain-object payloads a client builds before
// handing them to a generated mutation method -- client-agnostic
// shaping that encodes a memQL WIRE rule, not any one product's
// concerns, so it lives in the runtime core alongside renderMemQLValue.

/**
 * Recursively remove null AND undefined values from an object tree.
 *
 * Why both: memQL concept schemas with string-typed datetime fields
 * (savedAt, archivedAt, expiresAt, ...) reject an explicit `null` --
 * the "cleared" state is represented by OMITTING the field from the
 * new version, not by setting it to null. `undefined` is dropped here
 * too so optional-spread payload builds (`...(cond ? { x } : {})`)
 * don't leak `undefined` keys onto the wire.
 */
export function deepStripNulls<T>(obj: T): T {
  if (Array.isArray(obj)) return obj.map((v) => deepStripNulls(v)) as unknown as T;
  if (obj !== null && typeof obj === "object") {
    return Object.fromEntries(
      Object.entries(obj as Record<string, unknown>)
        .filter(([, v]) => v != null)
        .map(([k, v]) => [k, deepStripNulls(v)]),
    ) as T;
  }
  return obj;
}
