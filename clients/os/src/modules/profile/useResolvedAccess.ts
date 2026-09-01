import { useEffect, useState } from "react";
import type { AccessSummary } from "@znasllc-io/memql-sdk-core/client";

import { useOsConnection } from "../../live/connection";
import type { ProfileAccess } from "./access";

// Who is signed in, resolved from the CLUSTER STREAM.
//
// ===========================================================================
// WHY THIS REPLACED AN HTTP READ, AND WHAT THAT READ WAS DOING
// ===========================================================================
// The shell used to GET `{identityUrl}/me/api/profile`. That route does not
// exist -- it is registered in no Go file in this repo -- and the identity
// service answers the path with its own HTML at **200**. So the read sailed
// past its own `!response.ok` guard, `response.json()` threw on the markup,
// the surrounding try/catch swallowed it, and the shell concluded it had no
// access facts.
//
// The consequence was total and silent: `clusterRole` became "", `roleAdmits`
// refuses an unrankable role, and therefore EVERY role-gated app was invisible
// to EVERY user in EVERY cluster -- including the owner. Users (admin) and
// Training (writer) could never appear; the ungated apps could, which made it
// look like those two had never been built. Settings -> Diagnostics said
// "You are unknown" to a cluster owner.
//
// It looked verified because its test handed `fetchMyAccess` a stub `fetch`
// that returned the JSON it wanted and then asserted the URL STRING. A double
// that answers the call you are about to make cannot tell you whether anything
// serves it.
//
// So the facts now come from `query.getMyAccess()` over the connection the
// shell already holds -- the same call the portal makes
// (`clients/portal/src/cluster/useMyAccess.ts`), against a message the engine
// really implements. The comment that justified the HTTP stopgap said "the OS
// bundle cannot dial the engine stream yet"; that stopped being true when the
// live substrate landed, and nothing revisited it.

/**
 * Narrow an `AccessSummary` to what the shell needs.
 *
 * LENIENT ON PURPOSE, and specifically on `primaryEmail`. The parser this
 * replaces returned null unless `userId`, `primaryEmail` AND `clusterRole`
 * were all non-blank, so a session with no email erased a perfectly good role
 * -- a second, independent way to arrive at "you are unknown". The SDK's own
 * type notes that some credentials legitimately carry no session and no
 * address (a PAT, an operator key, a service account); an email is what the
 * Diagnostics panel prints, not what any decision reads.
 *
 * Null only when there is NOTHING usable. A blank role with a real user id is
 * still returned: it admits no gated surface (fail-closed, which is right) and
 * it keeps the user id that owner-scoped client filters depend on.
 */
export function accessFromSummary(summary: AccessSummary | null): ProfileAccess | null {
  if (summary === null) return null;
  const userId = summary.userId.trim();
  const primaryEmail = summary.primaryEmail.trim();
  const clusterRole = String(summary.clusterRole ?? "").trim();
  if (userId === "" && clusterRole === "") return null;
  return { userId, primaryEmail, clusterRole };
}

/**
 * `override` wins outright when given, and no read is made.
 *
 * That is what keeps every existing harness working -- the shell's tests and
 * previews pass a `ProfileAccess` directly -- and it is also the honest
 * precedence: a caller that already knows who is signed in is not asking.
 */
export function useResolvedAccess(override: ProfileAccess | null): ProfileAccess | null {
  const connection = useOsConnection();
  const [resolved, setResolved] = useState<ProfileAccess | null>(null);

  useEffect(() => {
    if (override !== null) return;
    if (connection === null) {
      // Cleared rather than kept: a stale identity beside a dead connection
      // reads as "still signed in to that cluster", which is the one thing
      // this must never imply. The portal's own hook states the same rule.
      setResolved(null);
      return;
    }
    const controller = new AbortController();
    let live = true;
    void (async () => {
      try {
        const summary = await connection.query.getMyAccess({ signal: controller.signal });
        if (!live) return;
        setResolved(accessFromSummary(summary));
      } catch {
        // A refusal or a dropped stream. Unknown is the honest answer, and it
        // admits nothing gated.
        if (live) setResolved(null);
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, override]);

  return override ?? resolved;
}
