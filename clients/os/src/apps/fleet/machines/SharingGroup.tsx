import { useCallback, useEffect, useRef, useState } from "react";

import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { Button, Caption, Notice, Subhead } from "../../../kit";
import { formatMoment } from "../../../kit/format";
import { isRevoked, type MachineRow } from "../rows";
import { ShareDialog } from "./ShareDialog";
import {
  isShared,
  ownsMachine,
  receiptDescribes,
  servesOthers,
  sharingSignature,
  sharingSummary,
  storedSubjects,
  subjectKey,
} from "./sharing";
import type { MachineWrites, SharingReceipt } from "./useMachineWrites";
import type { SharingLedger } from "./useMachineInference";
import { readShareDirectory, type ShareDirectory } from "./useShareDirectory";

// Who may use this machine (epic memql#5146, D6; three modes since epic
// memql#5344).
//
// ===========================================================================
// ONE LINE OF STATE, TWO CONSENTS, ONE ACT
// ===========================================================================
// The panel answers "who can use this machine?" in one line -- "Only you",
// "Shared with Ana Ruiz and Design", "Everyone in this cluster" -- and puts
// every way of changing that answer behind ONE button. The choosing (three
// modes, a search over the people the owner may pick, the chips, the terms)
// is a dialog, because it is a decision somebody makes now and then, and a
// page that held its controls open would ask it again every time it was read.
//
// ===========================================================================
// TWO CONSENTS, RENDERED SEPARATELY, ALWAYS
// ===========================================================================
// A machine serves somebody other than its owner only when BOTH halves are
// given: the owner lends it (people or everyone), from this page, and the
// cockpit agrees, from that machine's own policy.yaml. It would be shorter to
// render one derived "shared / not shared" line, and it would be wrong,
// because THE TWO REPAIRS ARE IN DIFFERENT PLACES: one is an act here, the
// other is a line in a file on a disk this page cannot reach. So there are two
// lines, always both, each saying its own state and carrying its own repair.
//
// The one-line state says what the OWNER decided. It is set in full ink only
// while it is in force; a share the cockpit has not agreed to is muted, and
// the cockpit's line beneath it says why.
//
// ===========================================================================
// THE LEDGER IS COUNTS, AND SAYING SO IS PART OF THE OFFER
// ===========================================================================
// Somebody deciding whether to lend their Mac Studio to the team is entitled
// to know what they will and will not see. That sentence sits WITH THE ACT --
// in the dialog, while the question is being asked -- rather than in a help
// page.

export function SharingGroup({
  machine,
  writes,
  ledger,
  standalone = false,
}: {
  standalone?: boolean;
  machine: MachineRow;
  writes: MachineWrites;
  /** The week's counts, already folded by the engine: calls, people, and
   *  nothing else. Null while it has not been read. */
  ledger: SharingLedger | null;
}) {
  const { access } = useSession();
  const connection = useOsConnection();
  const isOwner = ownsMachine(machine, access?.userId ?? "");
  const revoked = isRevoked(machine);

  const [open, setOpen] = useState(false);
  // What the last save on THIS machine did, and the stored state it was made
  // against. See `showReceipt` for when it stands.
  const [receipt, setReceipt] = useState<{ receipt: SharingReceipt; before: string } | null>(null);
  const actRef = useRef<HTMLDivElement>(null);

  // A different machine is a different conversation: a receipt about the last
  // one, or its dialog, must not carry over to this one.
  useEffect(() => {
    setReceipt(null);
    setOpen(false);
  }, [machine.id]);

  // ---- names ---------------------------------------------------------------
  //
  // The stored list holds IDS; the one-line state wants NAMES, and the only
  // read that has them is the directory -- which answers the machine's owner
  // and nobody else. So the owner's panel asks it for the names it does not
  // have yet, and every directory the dialog reads is folded in too: after a
  // save, the names of whoever was just picked are already here, and the row
  // arriving on the subscription is named without a second read.
  //
  // A read that fails is not retried and does not complain: the state line
  // counts instead, which is a true sentence with nothing guessed.
  const [names, setNames] = useState<ReadonlyMap<string, string>>(() => new Map());
  const absorb = useCallback((directory: ShareDirectory) => {
    setNames((held) => {
      const next = new Map(held);
      for (const person of directory.people) next.set(subjectKey("person", person.id), person.name);
      for (const group of directory.groups) next.set(subjectKey("group", group.id), group.name);
      for (const person of directory.current.people) {
        next.set(subjectKey("person", person.id), person.known ? person.name : "Unknown person");
      }
      for (const group of directory.current.groups) {
        next.set(subjectKey("group", group.id), group.known ? group.name : "Unknown group");
      }
      return next;
    });
  }, []);
  const unnamed = storedSubjects(machine)
    .map((s) => subjectKey(s.kind, s.id))
    .filter((key) => !names.has(key))
    .sort()
    .join(",");
  useEffect(() => {
    if (!isOwner || unnamed === "" || connection === null) return undefined;
    const controller = new AbortController();
    readShareDirectory(connection, machine.id, controller.signal).then(absorb, () => {
      // The count stands; see above.
    });
    return () => controller.abort();
    // KEYED ON WHO IS UNNAMED, not on the row: a heartbeat changes the row
    // every fifteen seconds and changes nobody's name.
  }, [isOwner, unnamed, machine.id, connection, absorb]);

  const summary = sharingSummary(machine, isOwner, (s) => names.get(subjectKey(s.kind, s.id)));
  const ownerGiven = isShared(machine);
  const cockpitGiven = machine.inferenceServe === "cluster";
  const inForce = !ownerGiven || cockpitGiven;

  // THE RECEIPT STANDS WHILE IT IS STILL TRUE: until its own echo has arrived
  // (the row still reads as it did before the save), and after, for as long as
  // the row says what the receipt says. A change landing from anywhere else
  // retires it rather than leaving a sentence about a state that has gone.
  const showReceipt =
    receipt !== null && (sharingSignature(machine) === receipt.before || receiptDescribes(receipt.receipt, machine));

  return (
    <div className="os-fleet-sharing">
      {!standalone ? <Subhead>Sharing</Subhead> : null}

      <p className="os-fleet-sharing-state" data-in-force={inForce || undefined}>
        {summary}
      </p>

      <ul className="os-fleet-consents">
        <Consent
          given={ownerGiven}
          text={
            ownerGiven
              ? `${isOwner ? "You shared it" : "Its owner shared it"}${machine.sharedAt ? ` on ${formatMoment(machine.sharedAt)}` : ""}.`
              : isOwner
                ? "You have not shared it."
                : "Its owner has not shared it."
          }
        />
        <Consent
          given={cockpitGiven}
          text={
            cockpitGiven
              ? isOwner
                ? "Its cockpit has agreed to serve anyone you share it with."
                : "Its cockpit has agreed to serve anyone its owner shares it with."
              : !ownerGiven
                ? // A REPAIR IS FOR A DECISION SOMEBODY MADE. Unshared, the
                  // cockpit's position is stated and nothing more: telling an
                  // owner to edit policy.yaml for a share they have not chosen
                  // is noise. The repair appears the moment a share does --
                  // below, and in the dialog while a draft lends the machine.
                  isOwner
                  ? "Its cockpit serves only you."
                  : "Its cockpit serves only its owner."
                : isOwner
                  ? "Its cockpit has not agreed to serve anyone but you. Set inference.serve to cluster in this machine's policy.yaml."
                  : "Its cockpit has not agreed to serve anyone but its owner. Set inference.serve to cluster in this machine's policy.yaml."
          }
        />
      </ul>

      {servesOthers(machine) && ledger ? <LedgerLine ledger={ledger} /> : null}

      {isOwner && !revoked ? (
        <div className="os-fleet-sharing-act" ref={actRef}>
          <Button
            onClick={() => {
              setReceipt(null);
              setOpen(true);
            }}
          >
            Change sharing
          </Button>
          {/* A STATUS REGION, standing and empty until there is something to
              say, so the engine's answer is announced when it arrives. */}
          <p className="os-caption fleet-share-receipt" role="status">
            {showReceipt ? receipt.receipt.sentence : ""}
          </p>
        </div>
      ) : (
        <Caption>
          {isOwner
            ? "This machine was removed, so it cannot be shared."
            : "Only this machine's owner can change who it is shared with."}
        </Caption>
      )}

      {open ? (
        <ShareDialog
          machine={machine}
          writes={writes}
          onDiscard={() => setOpen(false)}
          onSaved={(done) => {
            setReceipt({ receipt: done, before: sharingSignature(machine) });
            setOpen(false);
          }}
          onDirectory={absorb}
          returnFocus={() => actRef.current?.querySelector("button") ?? null}
        />
      ) : null}
    </div>
  );
}

/**
 * One consent, given or not.
 *
 * BOTH LINES ARE ALWAYS PRESENT, including the one that is given. Rendering
 * only the missing half would make a fully shared machine show nothing at all,
 * and a person who had just shared theirs would have no confirmation that
 * anything had happened. The mark is a SHAPE -- filled or hollow -- so the two
 * states are told apart without colour.
 */
function Consent({ given, text }: { given: boolean; text: string }) {
  return (
    <li className="os-fleet-consent" data-given={given || undefined}>
      <span className="os-fleet-consent-mark" aria-hidden="true" />
      <span>{text}</span>
    </li>
  );
}

/**
 * The week, in one sentence the engine wrote.
 *
 * THE TWO ANSWERS DO NOT LOOK ALIKE, and `readable` is the only thing that can
 * tell them apart. Both arrive as a sentence, because the engine refuses to
 * hand a page a count it would have to phrase: "Served 41 calls this week, 12
 * of them for 2 other people." and "This week's usage could not be read. It is
 * not that nothing ran -- nobody looked." are both true sentences about the
 * same field.
 *
 * Rendered identically they would read alike, and the second is not a reading
 * at all -- it is a question that did not get an answer. So the count is a
 * quiet caption and the failure is a notice, which is what this surface uses
 * everywhere else for "you asked and I could not tell you".
 *
 * The surface never composes a sentence from the counts, and that is the point
 * of taking one: a renderer that could phrase the ledger could phrase it wrong,
 * and the promise made to somebody lending their machine -- counts, never
 * content -- would then be kept in two places instead of one.
 */
function LedgerLine({ ledger }: { ledger: SharingLedger }) {
  if (!ledger.readable) return <Notice tone="warn" sentence={ledger.sentence} />;
  return <p className="os-caption">{ledger.sentence}</p>;
}
