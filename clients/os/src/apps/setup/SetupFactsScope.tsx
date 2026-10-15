import { isRoleTestTab } from "../../auth/roleTest";
import { useCallback, useMemo, type ReactNode } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { boolOr } from "../../kit";
import { useSession } from "../../chrome/access";
import { useReading } from "../../cluster/reading";
import { useOsConnection } from "../../live/connection";
import { SetupFactsProvider } from "./context";
import { coreIsConfigured, stopsAreKnown, stopsFor, type PasskeyReading } from "./stops";

// WHAT THIS CLUSTER STILL NEEDS SET UP, READ ONCE (design record
// 2026-09-07-core-gate-and-honest-install, D1 and D4).
//
// ===========================================================================
// THREE SURFACES NOW ASK THE SAME QUESTION
// ===========================================================================
// The core gate before the desk, the effect that decides whether the setup
// widget is on a desk at all, and the widget's own gate. They must not each
// ask it. `passkeysForSelf` is an on-demand read, so a hook per surface is a
// read per surface on every boot -- and two readings of one person's
// credentials are free to disagree, which is a gate that lifts on one surface
// while another still holds. SetupGate's own header made that argument when
// there were two consumers; there are now three.
//
// ===========================================================================
// IT SITS ABOVE THE DESK
// ===========================================================================
// The presence effect has to run when the widget is NOT on the desk, which is
// exactly when the widget's own gate is not mounted. So this provider is
// mounted in Shell.tsx, inside SessionScope (it reads the feed) and outside
// ShellRoster (the effect runs inside the desk, and the gate runs instead of
// it).
export function SetupFactsScope({ children }: { children: ReactNode }) {
  const { access, config, readiness } = useSession();
  const connection = useOsConnection();
  const role = access?.role ?? "";

  const readPasskeys = useCallback(
    async (signal: AbortSignal): Promise<Row[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.passkeysForSelf({}, { signal });
      return result.rows();
    },
    [connection],
  );
  // NOT ASKED AT ALL ON A CLUSTER WITH AUTHENTICATION OFF. The passkey stop is
  // `skipped` there and nothing reads the answer, so the read would be one
  // request per boot that could not change a pixel.
  const passkeys = useReading<Row[]>(
    `setup:passkeys:${config.authEnabled ? "on" : "off"}`,
    connection === null || !config.authEnabled || isRoleTestTab() ? null : readPasskeys,
  );

  // A FAILED READ IS `unknown`, NEVER `none` (D6 of the first-run record).
  // "We could not ask whether you have a passkey" and "you have no passkey"
  // lead to opposite acts, and the second told over the first sends somebody
  // to register a credential they already hold. `active` defaults TRUE on the
  // concept and a folded row carries only what a write touched, so it is read
  // through boolOr with that default rather than a bare truthiness test that
  // would count a revoked passkey as live.
  const reading: PasskeyReading =
    passkeys.value === null
      ? "unknown"
      : passkeys.value.filter((row) => boolOr(row, "active", true)).length > 0
        ? "held"
        : "none";

  const value = useMemo(() => {
    const stops = stopsFor({ readiness, passkeys: reading, authEnabled: config.authEnabled && !isRoleTestTab() });
    return {
      stops,
      // DERIVED HERE, ONCE. Three surfaces branch on these two booleans, and
      // three copies of `stopsAreKnown(stops) && !coreIsConfigured(stops)` is
      // three places for the fail-closed direction to be got wrong.
      known: stopsAreKnown(stops),
      configured: coreIsConfigured(stops),
      readiness,
      identityUrl: config.identityUrl,
      role,
    };
  }, [readiness, reading, config.authEnabled, config.identityUrl, role]);

  return <SetupFactsProvider value={value}>{children}</SetupFactsProvider>;
}
