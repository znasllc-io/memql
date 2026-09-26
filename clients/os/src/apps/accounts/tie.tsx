import { useMemo } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../chrome/access";
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
  return useAccountOptionsFeed().accounts;
}

/** Options together with their read state for creation flows. */
export function useAccountOptionsFeed() {
  const { snapshot } = useLiveCollection<Row>("accounts:options", (connection) => ({
    concept: ACCOUNT_CONCEPT,
    seed: async (_cursor, signal) => {
      const result = await connection.query.clientAccountsAll({ includeArchived: true }, { signal });
      return { rows: result.rows(), nextCursor: "" };
    },
    paged: false,
  }));
  const { access } = useSession();

  const accounts = useMemo(() => {
    const readable = snapshot.rows.map(accountFromRow).filter((a) => a.id !== "");
    if (readable.length > 0) return readable;

    // ===================================================================
    // THE FALLBACK: THE CALLER'S OWN CLIENTS, FROM MyAccess
    // ===================================================================
    // A client-rank person cannot read `v1:accounts:account` -- the row's
    // composite owner tier admits its owner and a cluster owner, and they
    // are neither -- so this read answers EMPTY for them, and a picker with
    // no options would let a Member of Acme tie their campaign to nobody.
    // Their work would then land where their colleagues cannot see it, which
    // is the opposite of what the tie is for.
    //
    // What they CAN be told is which groups they are in, because MyAccess
    // tells them as part of who they are (epic memql#5165, section H). Each
    // group carries the client's id AND name, so the option is nameable
    // without a second read that would be refused for the same reason the
    // first one was.
    //
    // IT IS A FALLBACK, NOT A MERGE, and only when the read came back with
    // NOTHING. A caller who can read accounts gets the rows -- those are
    // richer, current, and include clients they are not a member of.
    const seen = new Set<string>();
    const fromGroups: AccountRow[] = [];
    for (const group of access?.groups ?? []) {
      if (group.accountId === "" || seen.has(group.accountId)) continue;
      seen.add(group.accountId);
      fromGroups.push({
        ...EMPTY_ACCOUNT,
        id: group.accountId,
        // The group's own name is the fallback for a client whose name the
        // cluster did not carry: it is what this person calls that client,
        // and it beats rendering an id.
        name: group.accountName || group.name,
        // ACTIVE, because a group that grants an archived client is archived
        // with it (the cascade) -- so a group this person is in names a
        // client that is still active by construction.
        status: "active",
      });
    }
    return fromGroups;
  }, [snapshot, access]);
  return { accounts, state: snapshot.state, error: snapshot.error };
}

/**
 * The zero row a fallback option is built from.
 *
 * Every field a picker does not know is BLANK rather than invented: this
 * person cannot read the client's row, so its domain, its contact and its
 * walk state are things this window does not know -- and a plausible default
 * for any of them would be a fact about somebody's client that nobody stated.
 */
const EMPTY_ACCOUNT: AccountRow = {
  id: "",
  name: "",
  domain: "",
  primaryContactName: "",
  primaryContactEmail: "",
  notes: "",
  status: "",
  configuredAt: "",
  ownerUserId: "",
  createdAt: "",
  domainToken: "",
  domainStatus: "",
  domainFailureReason: "",
  domainFailureDetail: "",
  domainLastCheckedAt: "",
  domainVerifiedAt: "",
  joinOnDomain: false,
  memqlDomain: "",
  memqlReservedAt: "",
      memqlReservationReason: "",
};
