import { useCallback, useEffect, useRef, useState } from "react";

import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { Button, Caption, Fact, Facts, Notice, Subhead } from "../../../kit";
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

// Model sharing is independent from control of the host. Keep the current
// audience visible, with local setup instructions only when a share is blocked.
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
  const [receipt, setReceipt] = useState<{ receipt: SharingReceipt; before: string; serve: string } | null>(null);
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
  //
  // AND WHILE THE MACHINE'S OWN CONSENT IS AS IT WAS: a receipt that told the
  // owner to set inference.serve contradicts the consent line above it the
  // moment the machine agrees.
  const showReceipt =
    receipt !== null &&
    machine.inferenceServe === receipt.serve &&
    (sharingSignature(machine) === receipt.before || receiptDescribes(receipt.receipt, machine));

  return (
    <div className="os-fleet-sharing">
      {!standalone ? <Subhead>Model sharing</Subhead> : null}

      <p className="os-fleet-sharing-state" data-in-force={inForce || undefined}>
        {summary}
      </p>

      <Facts>
        <Fact label="Shared access" value="Model inference only" />
        <Fact label="Machine control" value={isOwner ? "Only you" : "Only the person who enrolled it"} />
        <Fact label="Cockpit" value={cockpitGiven ? "Ready to serve shared models" : "Personal models only"} />
      </Facts>

      {ownerGiven && !cockpitGiven ? (
        <Notice tone="info" sentence="Model sharing is waiting for Cockpit."
          next={isOwner
            ? "On this machine, set inference.serve to cluster in policy.yaml."
            : "The person who enrolled this machine needs to enable model sharing in Cockpit."} />
      ) : null}
      {machine.sharingMode === "cluster" ? (
        <Caption>Existing share. Change sharing to choose specific people and groups.</Caption>
      ) : null}

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
            setReceipt({ receipt: done, before: sharingSignature(machine), serve: machine.inferenceServe });
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
