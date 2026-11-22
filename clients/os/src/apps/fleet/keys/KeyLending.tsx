import { useState } from "react";
import { sameEntityId, type Connection } from "@znasllc-io/memql-sdk-core/client";

import { PageHelp } from "../../../kit/pageHelp";
import { ShareEditor } from "../machines/SharingEditor";
import type { StoredShare, Subject } from "../machines/sharing";
import { shareDirectoryFrom, useDirectory, type ShareDirectory } from "../machines/useShareDirectory";
import type { ActivityReading, VendorKeys } from "./useVendorKeys";
import type { KeyCard } from "./words";

// Lending a key to named people and groups (design record
// 2026-11-21-vendor-accounts-and-api-keys, K6 and U3).
//
// THE EDITOR IS THE ONE THAT LENDS A MACHINE (machines/SharingEditor.tsx): the
// switch, the search picker and a card for each grant with pause, resume and
// remove. This file is only what differs for a key: where the audience is
// saved, who may be picked, the figure on a card and the words.
//
// A KEY IS LENT TO NAMED PEOPLE AND GROUPS, NEVER TO EVERYONE. The cluster
// refuses the third mode a machine has, and the editor only offers it for a
// share that already holds it, which a key never does.
//
// THE FIGURE ON A CARD IS THIS MONTH'S CALLS, not the calls in flight a
// machine's card shows. A key has nothing in flight to report, and what its
// owner is asking of a grant is what it has cost them. It is known per person;
// a group's card shows none, because a call is recorded against the person who
// made it and not the group that admitted them.

/** The rows whose changes move who a key may be lent to. */
const DIRECTORY_CONCEPTS = ["v1:identity:user", "v1:identity:group", "v1:identity:groupMembership"];

async function readDirectory(connection: Connection, keyId: string, signal: AbortSignal): Promise<ShareDirectory> {
  const result = await connection.query.vendorKeyShareDirectory({ keyId }, { signal, priority: "background" });
  const directory = shareDirectoryFrom(result.rows()[0] ?? null);
  if (directory === null) throw new Error("The cluster answered without saying who you can lend this key to.");
  return directory;
}

function storedShare(card: KeyCard): StoredShare {
  return {
    sharingMode: card.sharing.mode,
    sharingPaused: card.sharing.paused,
    sharedUserIds: card.sharing.userIds,
    sharedGroupIds: card.sharing.groupIds,
    pausedUserIds: card.sharing.pausedUserIds,
    pausedGroupIds: card.sharing.pausedGroupIds,
  };
}

export function KeyLending({ card, keys, activity }: { card: KeyCard; keys: Pick<VendorKeys, "setSharing" | "busy">; activity: ActivityReading }) {
  const [needed, setNeeded] = useState(card.sharing.mode !== "owner");
  const directory = useDirectory(`key:${card.id}`, needed, (connection, signal) => readDirectory(connection, card.id, signal), DIRECTORY_CONCEPTS);
  const [error, setError] = useState("");

  function callsOf(subject: Subject): { text: string; title?: string } {
    if (subject.kind === "group") return { text: "" };
    if (activity.state !== "ready" || activity.activity === null) return { text: "— calls this month", title: "Calls unavailable" };
    const used = activity.activity.people.find((person) => sameEntityId(person.userId, subject.id));
    const calls = used?.calls ?? 0;
    return { text: calls === 1 ? "1 call this month" : `${calls.toLocaleString("en-US")} calls this month` };
  }

  return (
    <div className="os-fleet-sharing">
      <PageHelp title="Lending a key">
        <p>Lending lets the people and groups you add pay for their vendor calls with this key. You are billed by the vendor for what they use, and the figure on each card is how many calls that person made with it this month.</p>
        <p>Turn lending on, then open search to choose existing users or groups. Selections stay checked across searches, and the plus button adds them together. The pause, resume and trash icons on a card save at once. Turning lending off pauses it for everybody and keeps the list.</p>
        <p>A person a key is lent to sees its name, its vendor, that it is yours and whether it works. They never see the key, its fingerprint or who else has it. Auto spends a lent key for them only while "Let Auto use this key" is on.</p>
      </PageHelp>
      <ShareEditor
        key={card.id}
        target={{
          share: storedShare(card),
          setSharing: async (choice) => {
            setError("");
            const result = await keys.setSharing(card.id, choice);
            if (result.outcome === "done") return result.card;
            setError(result.sentence);
            return null;
          },
          busy: keys.busy !== "",
          error: error === "" ? undefined : error,
          directory,
          needDirectory: () => setNeeded(true),
          callsOf,
          words: {
            title: "Lending",
            toggle: "Lend this key",
            whenOff: "Only you spend this key.",
            whenOn: "The people and groups you add pay for their calls with it. You are billed for what they use.",
            whenPaused: "Paused. The list is saved, and nobody else spends this key.",
            grant: "lending to",
            failed: "Lending could not be updated.",
          },
        }}
      />
    </div>
  );
}
