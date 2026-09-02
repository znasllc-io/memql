import { useEffect, useState } from "react";

import { useOsConnection } from "../../live/connection";
import { type RoleRung, setRoleLadder } from "../../system/roles";

/**
 * Read the cluster's role ladder and install it as the shell's ordering
 * (epic memql#4832, D1).
 *
 * WHY A READ AND NOT A LITERAL. `clients/os/src/system/roles.ts` used to
 * carry the ordering as a five-item array that disagreed with the engine's.
 * Two hand-maintained ladders is the defect; picking a winner without
 * deleting the loser leaves it, and keeping the literal as a fallback hides
 * the divergence behind a condition nobody exercises.
 *
 * WHY IT IS A ONE-SHOT READ AND NOT A LIVE COLLECTION. `activeRoles` is
 * `@public @unbounded @cache(300)` -- global, immutable-in-practice reference
 * data that the cluster seeds on every boot -- and the concept carries no
 * broadcast routing rule, so there is no feed to subscribe to. Opening a
 * collection over it would render "Loading from the cluster" forever, which
 * is the failure clients/os/README.md warns about by name. A role added
 * while somebody is signed in appears on their next load, which is the same
 * latency their own role change already has.
 *
 * THE FAILURE MODE IS FAIL-CLOSED AND SHARED WITH useResolvedAccess. Until
 * this lands, roleRank answers -1 for every slug and roleAdmits refuses
 * everything gated -- the same answer an unreported actor role already gets.
 * A refused or dropped read leaves the ladder empty rather than guessing,
 * because a guessed ordering is precisely what this replaced.
 */
export function useRoleLadder(): boolean {
  const connection = useOsConnection();
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    if (connection === null) {
      // NOT CLEARED, and this is the deliberate divergence from
      // useResolvedAccess, which DOES clear the identity here.
      //
      // Its reason does not transfer. A stale identity beside a dead
      // connection reads as "still signed in to that cluster", which is the
      // one thing it must never imply. A stale LADDER implies nothing about
      // anybody: it is cluster SCHEMA -- which roles exist and how they rank
      // -- not a fact about this session.
      //
      // And discarding it has a cost the identity does not. Every gated
      // surface would vanish the instant a connection blipped, which states
      // something false and alarming -- that this person has lost access they
      // still hold -- where the honest reading of a dropped connection is
      // that the shell cannot reach the cluster right now.
      //
      // Fail-closed still holds where it matters: on a FIRST load there is no
      // ladder to keep, so nothing gated renders until the read lands.
      setLoaded(false);
      return;
    }
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const result = await connection.query.activeRoles({ signal: controller.signal });
        if (!live) return;
        const rungs = rungsFrom(result.rows());
        if (rungs.length === 0) {
          // A SUCCESSFUL READ THAT RETURNS NOTHING IS NOT A LADDER.
          //
          // Every booted cluster seeds five base roles, so an empty catalog
          // means the read did not reach one -- an unseeded cluster, a
          // narrowed result, a shape change. Installing it would assert
          // "this cluster has no roles", which is never true and which
          // blanks every gated surface for everybody.
          //
          // Treated exactly like a failed read: nothing installed, nothing
          // discarded. On a first read the ladder stays empty and gated
          // surfaces stay hidden, which is the fail-closed direction.
          setLoaded(false);
          return;
        }
        setRoleLadder(rungs);
        setLoaded(true);
      } catch {
        // A REFUSED OR DROPPED READ DOES NOT DISCARD A LADDER ALREADY HELD.
        //
        // The ordering this shell holds only ever came from the cluster, so
        // keeping it is not the "guessed ordering" this file replaced -- and
        // dropping it would empty the launcher of every gated app over one
        // failed read, which reads to the person in front of it as losing
        // access they still have.
        if (live) setLoaded(false);
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection]);

  return loaded;
}

/**
 * Narrow the read's rows to rungs, dropping anything that cannot be ranked.
 *
 * A row with no slug or a non-numeric rank is DROPPED rather than defaulted.
 * A default here would invent an ordering -- the entire class of bug this file
 * exists to remove -- and a dropped rung simply means the surfaces that name
 * it stay hidden, which is the fail-closed direction.
 */
function rungsFrom(rows: Record<string, unknown>[]): RoleRung[] {
  const out: RoleRung[] = [];
  for (const row of rows) {
    const slug = typeof row.slug === "string" ? row.slug.trim() : "";
    const rank = typeof row.rank === "number" ? row.rank : Number.NaN;
    if (!slug || !Number.isFinite(rank)) continue;
    if (row.active === false) continue;
    out.push({
      slug,
      name: typeof row.name === "string" && row.name.trim() ? row.name.trim() : slug,
      rank,
      aliases: Array.isArray(row.aliases)
        ? row.aliases.filter((a): a is string => typeof a === "string")
        : [],
    });
  }
  return out;
}
