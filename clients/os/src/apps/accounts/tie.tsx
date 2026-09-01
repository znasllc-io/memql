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
 * ONE COLLECTION PER MOUNTING COMPONENT, and it is worth being exact about
 * that rather than assuming the key shares it. The SDK HAS a registry that
 * shares a collection by key (`LiveRegistry.collection`), and
 * `live/useLiveCollection.ts` does not call it -- it constructs a
 * `LiveCollection` per component, memoised on `[connection, key]`. So four
 * apps mounting a picker at once open four subscriptions over this concept,
 * not one.
 *
 * That is accepted here and is NOT accepted inside the Accounts app itself,
 * and the difference is what the two feeds decide. Two readings inside one
 * app would be free to disagree about the registry while deciding whether a
 * form or a list renders, which is why AccountsApp retains exactly one and
 * passes it down. Across apps there is nothing to disagree about: each window
 * renders its own picker from its own snapshot, an account list is small, and
 * the alternative -- routing four apps' feeds through one shared retain -- is
 * a shell-level change that would want to move every app's collection at once
 * rather than being invented for this picker.
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
