import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useLiveCollection } from "../../live/useLiveCollection";
import { ACCOUNT_CONCEPT, accountFromRow, type AccountRow } from "./rows";

// THE SEAM THE TIE SURFACES IMPORT.
//
// Four other apps -- Deployables, Files, Users, Training -- render and edit an
// account tie, and each one needs the same two things: the account list, so a
// picker has options and an id can be resolved to a name, and the picker
// itself (AccountPicker.tsx beside this file).
//
// It lives HERE, in the app that owns the concept, rather than in kit/. The
// kit is the OS's shared vocabulary -- rows, chips, notices, the live list --
// and an account picker is not vocabulary, it is one domain's surface that
// four apps happen to mount. Putting it in the kit would make every app carry
// a dependency on a concept most of them do not otherwise know about.

/**
 * The account list, for a picker.
 *
 * A SEPARATE COLLECTION FROM THE APP'S, and deliberately so: `useLiveCollection`
 * keys by identity, so a surface in Files and the Accounts window open beside
 * it share nothing and neither can unmount the other's feed. The linger in
 * `useLiveCollection` means the second retain of the same key reuses one
 * collection and issues no new read, so mounting this in four apps at once
 * costs one subscription, not four.
 *
 * `includeArchived: true` for the reason the app's own feed asks for
 * everything: a picker must be able to show an archived client that a row is
 * ALREADY tied to, and filtering server-side would make that tie render as an
 * unresolvable id.
 */
export function useAccountOptions(): AccountRow[] {
  const { snapshot } = useLiveCollection<Row>("accounts:options", (connection) => ({
    concept: ACCOUNT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.clientAccountsAll({ includeArchived: true }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    paged: false,
  }));

  return useMemo(
    () => snapshot.rows.map(accountFromRow).filter((a) => a.id !== ""),
    [snapshot],
  );
}
