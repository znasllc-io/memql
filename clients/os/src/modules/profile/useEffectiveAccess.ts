import { useEffect, useState } from "react";

import { useOsConnection } from "../../live/connection";
import { boolOr, flatten } from "../../kit/rows";
import {
  effectiveAccessEpoch,
  onGrantWritten,
  setEffectiveCapabilities,
  type EffectiveCapability,
  type OrganizationCapability,
} from "../../system/roles";

/**
 * Read the signed-in person's EFFECTIVE CAPABILITY SET and install it as the
 * shell's one access predicate (epic memql#5289, design D10 and D11).
 *
 * WHY A READ AND NOT A DERIVATION. The set the engine resolves is the role
 * catalog overlaid with the person's group grants and their own grants --
 * most specific wins, deny wins within a level. A shell that derived "may I
 * open Users" from the role alone would agree with the engine on every
 * cluster that has never written a grant and disagree on the first one that
 * has, which is the whole feature. So the shell asks, and holds the answer.
 *
 * WHEN IT ASKS AGAIN (D11): on sign-in (the connection arriving), on window
 * FOCUS, and after any grant written from this browser. Grant rows are
 * cluster-owner tier and are not broadcast, so a grant somebody else writes
 * for this person cannot arrive as an event; focus is when a person comes
 * back to a tab after being told "I gave you Deployables", and it is the
 * moment a re-read is cheap and expected. A live subscription is deliberately
 * not part of this.
 *
 * FAIL-CLOSED, like the ladder read. Until the first read lands, `holds`
 * answers false and nothing gated renders; a refused or dropped read leaves
 * whatever set was already held, because emptying it would blank the launcher
 * over one failed read and read to the person as losing access they still
 * have. A connection going away keeps the set too, for the ladder's reason
 * rather than the identity's: the identity is cleared because a stale one
 * beside a dead connection claims a session that may be gone, but the
 * launcher follows the IDENTITY (the core gate draws nothing without one), so
 * clearing the set as well would only blank a desk that a reconnect is about
 * to redraw -- and read, for the seconds in between, as access being taken
 * away.
 *
 * Returns the ACCESS EPOCH -- a counter that moves on every install -- which
 * the session and shell contexts carry so a memo that filters by capability
 * recomputes the moment the set changes (the memql#4857 lesson).
 */
export function useEffectiveAccess(): number {
  const connection = useOsConnection();
  const [epoch, setEpoch] = useState(() => effectiveAccessEpoch());
  const [nonce, setNonce] = useState(0);

  // The two re-read triggers, both of which just ask again.
  useEffect(() => {
    const again = () => setNonce((n) => n + 1);
    const offGrant = onGrantWritten(again);
    const onFocus = () => again();
    window.addEventListener("focus", onFocus);
    return () => {
      offGrant();
      window.removeEventListener("focus", onFocus);
    };
  }, []);

  useEffect(() => {
    if (connection === null) return;
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const result = await connection.query.effectiveCapabilitiesForActor({}, { signal: controller.signal });
        if (!live) return;
        const row = result.rows()[0];
        const entries = row === undefined ? null : entriesFrom(row);
        if (entries === null) {
          // A reply with no `ok` is not an answer: an engine that predates the
          // read, or a refusal in the envelope. Nothing installed, nothing
          // discarded -- the fail-closed direction on a first read, and the
          // keep-what-you-hold direction on a later one.
          return;
        }
        setEffectiveCapabilities(entries, organizationEntriesFrom(row!));
        setEpoch(effectiveAccessEpoch());
      } catch {
        // A refused or dropped read keeps the set already held; see above.
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, nonce]);

  return epoch;
}

/**
 * Narrow the builtin's one reply row to entries, or null when the reply is
 * not an answer.
 *
 * The row is `{ ok, code, role, userId, entries: [{ verb, resource, effect,
 * source }] }`. An entry missing its verb or resource is DROPPED rather than
 * defaulted -- a defaulted entry would be a capability the engine never
 * reported -- and an unrecognised effect reads as `deny`, the fail-closed
 * value.
 */
export function entriesFrom(row: Record<string, unknown>): EffectiveCapability[] | null {
  const flat = flatten(row);
  if (!boolOr(flat, "ok", false)) return null;
  const raw = flat["entries"];
  if (!Array.isArray(raw)) return [];
  const out: EffectiveCapability[] = [];
  for (const item of raw) {
    if (item === null || typeof item !== "object") continue;
    const entry = item as Record<string, unknown>;
    const verb = typeof entry["verb"] === "string" ? entry["verb"].trim() : "";
    const resource = typeof entry["resource"] === "string" ? entry["resource"].trim() : "";
    if (verb === "" || resource === "") continue;
    const source = entry["source"];
    out.push({
      verb,
      resource,
      effect: entry["effect"] === "allow" ? "allow" : "deny",
      source: source === "user" || source === "group" ? source : "role",
    });
  }
  return out;
}

/** The same response carries organization decisions without flattening denials
 * from different organizations into one misleading global action permission. */
export function organizationEntriesFrom(row: Record<string, unknown>): OrganizationCapability[] {
  const flat = flatten(row);
  if (!boolOr(flat, "ok", false) || !Array.isArray(flat["organizationEntries"])) return [];
  return flat["organizationEntries"].flatMap((item: unknown) => {
    if (item === null || typeof item !== "object") return [];
    const entry = item as Record<string, unknown>;
    const accountId = typeof entry["accountId"] === "string" ? entry["accountId"].trim() : "";
    const verb = typeof entry["verb"] === "string" ? entry["verb"].trim() : "";
    const resource = typeof entry["resource"] === "string" ? entry["resource"].trim() : "";
    if (!accountId || !verb || !resource) return [];
    return [{ accountId, verb, resource, effect: entry["effect"] === "allow" ? "allow" as const : "deny" as const }];
  });
}
