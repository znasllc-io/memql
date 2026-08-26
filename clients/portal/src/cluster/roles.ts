import type { Role } from "@znasllc-io/memql-sdk-core/client";

import { useMyAccess } from "./useMyAccess";

// WHO MAY BE OFFERED THE CLUSTER'S INTERNALS -- one predicate, one place.
//
// It lives beside useMyAccess rather than inside the admin console because it
// stopped being an admin-console question in memql#4653: ErrorNotice's
// "Technical details" disclosure, PageGuide's technical section and the rail's
// admin rows all ask it, and three modules each computing `role === "owner" ||
// role === "admin"` is three places for the answer to drift.
//
// ===========================================================================
// IT DECIDES WHAT IS OFFERED. IT DECIDES NOTHING ABOUT WHAT IS PERMITTED.
// ===========================================================================
// Every read and every write behind these surfaces is gated server-side, per
// stream and per row. A reader who forces this true sees a disclosure
// containing a string their own browser already had; they do not see one row
// more of anybody else's data. The reasoning is stated at length in
// src/admin/useAdminConsole.ts, and this constant is the one it used to own.
export const CAN_ADMINISTER: readonly Role[] = ["owner", "admin"];

export function canAdminister(role: string): boolean {
  return (CAN_ADMINISTER as readonly string[]).includes(role);
}

// The hook form, for a component that only needs the boolean.
//
// It rides the SAME connection-scoped read every other caller does
// (useMyAccess is deduped through the SDK's LiveValue), so a page rendering
// ten ErrorNotices issues zero extra round trips.
//
// UNRESOLVED READS AS FALSE, which is the safe direction for a disclosure:
// the section appears when the role arrives, and never flashes internals at
// somebody while the connection is still handshaking.
export function useCanAdminister(): boolean {
  const { access } = useMyAccess();
  return canAdminister(access?.clusterRole ?? "");
}
