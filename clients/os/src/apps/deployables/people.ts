import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import { flatten } from "../../kit/rows";
import type { DeploymentRow, PackageRow } from "./packages/rows";
import type { SiteRow } from "./rows";

// WHO DEPLOYED WHAT (epic memql#5289, task memql#5306; design section 4
// "Attribution").
//
// ===========================================================================
// THE FACT IS ON THE ROWS ALREADY READ; ONLY THE NAME IS LOOKED UP
// ===========================================================================
// A deploy is requested by a person, and the pipeline stamps them: the run's
// `requestedBy`, and -- since PR #5284 stopped the erasure -- the site's own
// `ownerUserId`, which is the deployer's from the first write. A source's
// `ownerUserId` is who added it. So "deployed by" needs NO new query: it is
// read off the rows the list and the page already hold, in that order, and
// only the id-to-name step reaches the cluster.
//
// ===========================================================================
// A NAME IS OFFERED, NEVER INVENTED
// ===========================================================================
// Names come from the people roster (`searchUsers`), which is floored at
// developer -- the two people this was asked for (the owner and the
// developer sharing a cluster) both read it. A reader cannot, and for them
// the read refuses: nothing is installed, and a row deployed by somebody
// else says nothing beyond the ownership chip it already carries. An opaque
// id is not printed in a name's place, and "someone" adds nothing to
// "another owner". The one name every session can resolve without a read is
// its own, which is what "deployed by you" is.
//
// A ONE-SHOT READ, like the ladder's: the roster is reference data for this
// purpose, and a LiveCollection over every user row -- with the arrival cue
// and the per-heartbeat churn that implies -- would be paying for news
// nobody asked for. A person added while the window is open is named on the
// next open.

/** A resolver from a user id to a display name, "" when none is known. */
export type NameOf = (userId: string) => string;

/**
 * The people this session can name, read once per connection.
 *
 * Refusal-tolerant by design: a roster the caller may not read answers an
 * empty map, and the surfaces then render only what they already knew.
 */
export function usePeopleNames(): NameOf {
  const connection = useOsConnection();
  const [names, setNames] = useState<ReadonlyMap<string, string>>(() => new Map());

  useEffect(() => {
    if (connection === null) return;
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const result = await connection.query.searchUsers({}, { signal: controller.signal });
        if (!live) return;
        setNames(namesFrom(result.rows() as Row[]));
      } catch {
        // Below the roster's floor, or a dropped stream: nothing to name
        // with, and nothing pretended.
        if (live) setNames(new Map());
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection]);

  return useMemo(() => (userId: string) => names.get(bare(userId)) ?? "", [names]);
}

/** A person's name off their row: display name, else the email, else nothing. */
export function namesFrom(rows: readonly Row[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const raw of rows) {
    const row = flatten(raw);
    const id = typeof row["id"] === "string" ? bare(row["id"]) : "";
    if (id === "") continue;
    const display = typeof row["displayName"] === "string" ? row["displayName"].trim() : "";
    const email = typeof row["primaryEmail"] === "string" ? row["primaryEmail"].trim() : "";
    const name = display !== "" ? display : email;
    if (name !== "") out.set(id, name);
  }
  return out;
}

/**
 * The user id in its bare spelling. Rows reach the shell bare at the wire
 * seam, but a `requestedBy` written by an older run may still carry the
 * canonical `v1:identity:user:` prefix; comparing the two forms is what
 * would make a person fail to recognise their own deploy.
 */
export function bare(userId: string): string {
  const trimmed = userId.trim();
  const at = trimmed.lastIndexOf(":");
  return at < 0 ? trimmed : trimmed.slice(at + 1);
}

/**
 * Who deployed a row: the run's requester, else the site's owner, else the
 * source's owner. "" when no row says.
 */
export function deployerOf(
  run: DeploymentRow | null | undefined,
  site: SiteRow | null | undefined,
  pkg: PackageRow | null | undefined,
): string {
  const candidates = [run?.requestedBy, site?.ownerUserId, pkg?.ownerUserId];
  for (const c of candidates) {
    if (typeof c === "string" && c.trim() !== "") return bare(c);
  }
  return "";
}

/**
 * The attribution in words, or "" when there is nothing honest to say.
 *
 * "you" for the viewer's own deploy, the person's name when the roster gave
 * one, and nothing otherwise -- never an id, never a placeholder person.
 */
export function deployedByLabel(deployer: string, viewerUserId: string, nameOf: NameOf): string {
  const who = bare(deployer);
  if (who === "") return "";
  if (viewerUserId !== "" && who === bare(viewerUserId)) return "you";
  return nameOf(who);
}
