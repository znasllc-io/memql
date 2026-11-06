/**
 * How an address becomes the key the engine files it under.
 *
 * A mirror of the engine's `campaigns.NormalizeEmail` and `EmailDigest`
 * (component/campaigns/digest.go): trim, lower-case, require an `@` that is
 * neither first nor last, refuse any space, tab or line break, then take the
 * lower-case hex SHA-256 of what is left. It is a stored format, so a test
 * pins it against values the Go function produced. It returns "" for an
 * address the engine would not digest, the same answer the engine gives.
 *
 * Known gaps, backstopped by the engine refusing a duplicate itself: JS and Go
 * lower-case a few non-ASCII letters differently, and JS `trim` strips U+FEFF
 * where Go's `TrimSpace` does not (and the reverse for U+0085).
 */
export function normalizeAddress(address: string): string {
  const trimmed = address.trim().toLowerCase();
  const at = trimmed.lastIndexOf("@");
  if (at <= 0 || at === trimmed.length - 1) return "";
  if (/[ \t\r\n]/.test(trimmed)) return "";
  return trimmed;
}

export async function addressDigest(address: string): Promise<string> {
  const normalized = normalizeAddress(address);
  if (normalized === "") return "";
  const sum = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(normalized));
  return Array.from(new Uint8Array(sum), byte => byte.toString(16).padStart(2, "0")).join("");
}
