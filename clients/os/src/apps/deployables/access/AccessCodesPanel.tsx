import { useEffect, useRef, useState, type MouseEvent, type ReactNode } from "react";
import { CalendarClock, Lock, Power, RefreshCw, Trash2 } from "lucide-react";
import { sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, CopyValue, DateTimeInput, EmptyState, Field, Input, Notice, RecordListSkeleton, Select, Subhead, useNow } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { IconButton } from "../../../kit/IconButton";
import { formatMoment } from "../../../kit/format";
import { InPlaceAsk } from "../../../kit/pressHold";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { browserZone, instantToLocal } from "../../email/when";
import { partsForOrganization, useDeployableParts, type PartsHeld } from "../parts";
import { siteIsClusterOwned, type SiteRow } from "../rows";
import { CUSTOM_EXPIRY, DEFAULT_EXPIRY, EXPIRY_PRESETS, expiryFromDraft, endWord, expiryWord, hasExpired, lastOpenedWord, type ExpiryDraft } from "./expiry";
import { useAccessCodes, type AccessCodeRow, type AccessCodes, type AccessGateRow, type WriteOutcome } from "./useAccessCodes";

// The Access codes panel (design record 2026-10-29-deployable-access-codes,
// section 5; clients/os/DESIGN.md rules 7, 8 and 12).
//
// ===========================================================================
// ONE THING ON THIS SURFACE IS LOUD, AND IT IS THE CODE
// ===========================================================================
// A record list and a short form, all of it the kit's and all of it quiet, so that
// the one moment that cannot be repeated -- the plain code, shown once -- is the
// only thing drawn at more than the shell's ordinary size.
//
// ===========================================================================
// A QUESTION STANDS WHERE THE ACT WAS ASKED, AND HOLDS ITS PRESSES
// ===========================================================================
// Regenerate and Remove take somebody out of a site they are using now, so each
// asks under its own row and says what stops working. The act's button is the
// second click of a double-click waiting to happen, so `InPlaceAsk` holds it.
// Done is held for the same reason from the other side: Create's second click must
// not press what the first one brought, and throw away a code shown once.
//
// ===========================================================================
// A PRESS THAT WRITES NOTHING IS NOT THE NEXT ACT
// ===========================================================================
// The code goes when the next write starts (`useAccessCodes`), not when a question
// opens: a person who asks and answers Keep it has changed nothing, and an uncopied
// code gone from under them would cost a regenerate to learn that.
//
// ===========================================================================
// THE DIALOG WAITS WHILE IT HOLDS THE ONLY COPY
// ===========================================================================
// Closed with a write out, the reply lands in nothing: the old code is dead and the
// new one was never seen. Closed over a new code, that code is gone for good. So
// `onBusyChange` keeps Close and Escape from acting for both, and Done is the way out.

/** One clock for the panel (`useNow`), so a code turns to Expired with the panel left open. */
const CLOCK_MS = 5_000;

type RowAct = "regenerate" | "setExpiry" | "remove";

/** The panel: the codes, the way to add one, and whichever single question is open. */
export function AccessCodesPanel({ site, onBusyChange }: {
  site: SiteRow;
  /** Told whether the panel holds the only copy of something: a reply on its way, or a new code. */
  onBusyChange?: (busy: boolean) => void;
}) {
  const access = useAccessCodes(site);
  const held = useDeployableParts();
  const now = useNow(CLOCK_MS).getTime();
  const holding = access.busy !== null || access.minted !== null;
  useEffect(() => {
    onBusyChange?.(holding);
  }, [holding, onBusyChange]);
  // WHICH FORM TAKES THE LIST'S PLACE: a code's, or the guest pass's.
  const [adding, setAdding] = useState<"code" | "guest" | null>(null);
  // The gates the form opened with. A re-read that fails answers null, and a draft
  // somebody is part-way through must not go with it.
  const [formGates, setFormGates] = useState<readonly AccessGateRow[]>([]);
  const [asked, setAsked] = useState<{ codeId: string; act: RowAct } | null>(null);

  // WHERE FOCUS GOES BACK TO. The form, the question and the code each take focus
  // and then leave, and focus left on nothing in a modal sends the next Tab to the
  // dialog's header. The control that asked is usually still there; Add is not (it
  // is absent while the form is open), and neither is the row of a removed code.
  // The panel itself is the last resort: a list with every gate gone has no Add.
  const root = useRef<HTMLDivElement>(null);
  const opener = useRef<HTMLElement | null>(null);
  const backTo = useRef<string | null>(null);
  const [refocus, setRefocus] = useState(0);
  useEffect(() => {
    if (refocus === 0) return;
    // The control that asked, else the one named for it (Allow guests is drawn again after its form
    // closes, so the button that asked is not the one on screen), else Add, else the panel.
    const named = backTo.current === null ? null : root.current?.querySelector<HTMLElement>(backTo.current);
    backTo.current = null;
    const back = opener.current?.isConnected ? opener.current : named ?? root.current?.querySelector<HTMLElement>(".os-add-button") ?? root.current;
    back?.focus();
  }, [refocus]);
  // A NEW GUEST PASS TAKES FOCUS WHEN THE RE-READ AFTER ITS CREATE SETTLES: the form is gone by then, and
  // the row comes with that read. ARMED WHEN THE WRITE STARTS (`onWriting`), because the re-read can
  // begin before the write's answer is handled, and DISARMED when that read settles, whether or not the
  // pass came in, so a pass that arrives with any later read cannot pull focus. A pass that opens nothing
  // has no Change end date, and the last resort is the panel, as everywhere here.
  const focusPass = useRef(false);
  const sawRead = useRef(false);
  const codesState = access.codes.state;
  const passId = access.guestPass?.id;
  useEffect(() => {
    if (!focusPass.current) return;
    if (codesState === "reading") {
      sawRead.current = true;
      return;
    }
    if (!sawRead.current || codesState === "unread" || adding !== null) return;
    focusPass.current = false;
    sawRead.current = false;
    const part = root.current?.querySelector<HTMLElement>(".access-guest");
    const target =
      part?.querySelector<HTMLElement>('button[aria-label="Change end date"]') ??
      part?.querySelector<HTMLElement>('button[aria-label="Turn off"]') ??
      root.current?.querySelector<HTMLElement>(".os-add-button") ??
      root.current;
    target?.focus();
  }, [codesState, passId, adding]);
  const returnFocus = (): void => setRefocus((n) => n + 1);

  const askFor = (event: MouseEvent<HTMLElement>, codeId: string, act: RowAct): void => {
    opener.current = event.currentTarget;
    access.clearRefusal();
    setAsked({ codeId, act });
  };

  const codes = access.codes.value;
  const gates = access.gates.value;
  const noGates = gates !== null && gates.length === 0;
  // ABSENT WITH NO OWNER (rule 12). The engine refuses a mint on a site with no owner,
  // and a code's row is written as that owner, so there is nobody to write it as.
  const ownerless = siteIsClusterOwned(site);
  const canAdd = gates !== null && gates.length > 0 && !ownerless;
  const gatesOut = gates === null && access.gates.state !== "failed";

  // A REFUSAL WHOSE ROW HAS GONE. The re-read after a refused regenerate of a code
  // somebody else removed takes the row, and the question under it, off the list,
  // and a refusal nobody can read is a refusal nobody can act on.
  const refusal = access.refusal;
  const guestPass = access.guestPass;
  // The guest pass is not on the code list, and its refusals belong to its row all the same.
  const refusedRowGone = refusal !== null && refusal.codeId !== "" && !(codes ?? []).some((code) => code.id === refusal.codeId) && guestPass?.id !== refusal.codeId;
  // The question went with its row, with focus inside it.
  useEffect(() => {
    if (refusedRowGone) setRefocus((n) => n + 1);
  }, [refusedRowGone]);

  // THE SAME RULE AS THE HEADING OVER CODES: no part over nothing. With no owner or no gate and
  // nothing standing, the one explanation below is the panel.
  const showGuests = codes !== null && (guestPass !== undefined || !((noGates || ownerless) && codes.length === 0));

  let body: ReactNode;
  if (!access.connected) {
    body = <Notice tone="error" sentence="Not connected to the cluster, so the access codes could not be read." />;
  } else if (codes === null) {
    // A failure is said above, and a skeleton beneath it would claim a read is still out.
    body = access.codes.state === "failed" ? null : <RecordListSkeleton label="Loading access codes" />;
  } else if (codes.length > 0) {
    body = (
      <>
        {/* Said once, over the list: it is true of every row. */}
        {ownerless ? <Notice tone="warn" sentence="This deployable has no owner, so its codes open nothing and no code can be added. Give it an owner to use them." /> : noGates ? <Caption><NoGates /></Caption> : null}
        <RecordList as="ul" label="Access codes">
          {codes.map((code) => (
            <CodeEntry
              key={code.id}
              code={code}
              site={site}
              gates={gates}
              held={held}
              now={now}
              access={access}
              asked={asked?.codeId === code.id ? asked.act : null}
              onAsk={(event, act) => askFor(event, code.id, act)}
              onKeep={() => {
                access.clearRefusal();
                setAsked(null);
                returnFocus();
              }}
              onAnswered={(act, outcome) => {
                setAsked(null);
                // THE ROW OF A REMOVED CODE IS ABOUT TO GO. It stays until the re-read
                // lands, so it would take focus back now and drop it on nothing then.
                if (act === "remove") opener.current = null;
                // A new code takes focus for itself, and gives it back on Done.
                if (act !== "regenerate" || outcome === "unknown") returnFocus();
              }}
            />
          ))}
        </RecordList>
      </>
    );
  } else if (ownerless) {
    body = (
      <EmptyState title="This deployable has no owner" icon={Lock}>
        It cannot have access codes until it has one. Give it an owner, then add a code here.
      </EmptyState>
    );
  } else if (gatesOut) {
    // What an empty list means depends on the gates, so it is not said until they land.
    body = <RecordListSkeleton label="Loading access codes" />;
  } else if (noGates) {
    body = (
      <EmptyState title="This app has no gates yet" icon={Lock}>
        <NoGates />
      </EmptyState>
    );
  } else {
    body = (
      <EmptyState title="No access codes yet" icon={Lock}>
        A code opens one gate of this app to whoever holds it, until it expires.
      </EmptyState>
    );
  }

  return (
    <div className="access-codes" ref={root} tabIndex={-1}>
      {/* ABOVE THE CODES, and gone while a form has taken the list's place. */}
      {adding === null && showGuests ? (
        <GuestAccess
          site={site}
          pass={guestPass}
          canAllow={canAdd}
          now={now}
          access={access}
          asked={guestPass !== undefined && asked?.codeId === guestPass.id ? asked.act : null}
          onAllow={() => {
            // CANCEL GIVES FOCUS BACK TO THIS ACT, which is drawn again with the part; a created pass
            // clears it (`onCreated`), because the act is gone then.
            opener.current = null;
            backTo.current = ".access-guest .os-head button";
            access.clearRefusal();
            setAsked(null);
            setFormGates(gates ?? []);
            setAdding("guest");
          }}
          onAsk={(event, act) => guestPass !== undefined && askFor(event, guestPass.id, act)}
          onKeep={() => {
            access.clearRefusal();
            setAsked(null);
            returnFocus();
          }}
          onAnswered={(act) => {
            setAsked(null);
            if (act === "remove") opener.current = null;
            returnFocus();
          }}
        />
      ) : null}
      {/* No heading over nothing: with no codes and no way to add one, the explanation is the panel. */}
      {(noGates || ownerless) && codes !== null && codes.length === 0 ? null : (
        <Subhead
          actions={
            canAdd && !adding ? (
              <AddButton
                label="Add an access code"
                onClick={(event) => {
                  opener.current = event.currentTarget;
                  access.clearRefusal();
                  setAsked(null);
                  setFormGates(gates);
                  setAdding("code");
                }}
              />
            ) : null
          }
        >
          {adding === "code" ? "Add an access code" : adding === "guest" ? "Allow guests" : "Codes"}
        </Subhead>
      )}
      {access.readFailures.map((sentence) => (
        <Notice key={sentence} tone="error" sentence={sentence}>
          <Button onClick={access.reread}>Try again</Button>
        </Notice>
      ))}
      {refusal?.unknown ? <Notice tone="warn" sentence={refusal.sentence} /> : refusedRowGone ? <Notice tone="error" sentence={refusal.sentence} /> : null}
      {access.minted !== null ? (
        <MintedCodeNotice
          code={access.minted.code}
          label={access.minted.label}
          onDone={() => {
            access.dropMinted();
            returnFocus();
          }}
        />
      ) : null}
      {/* THE FORM TAKES THE LIST'S PLACE (rule 11), under the name of the act. A
          list beneath a form is more to read past, in a dialog that has no height
          to spare, and nothing on it helps somebody name a new code. */}
      {adding !== null ? (
        <AddCodeForm
          mode={adding}
          siteId={site.id}
          gates={gates ?? formGates}
          now={now}
          access={access}
          onCancel={() => {
            access.clearRefusal();
            setAdding(null);
            returnFocus();
          }}
          onWriting={(writing) => {
            focusPass.current = writing;
            sawRead.current = false;
          }}
          onCreated={(outcome) => {
            setAdding(null);
            if (adding === "guest") {
              opener.current = null;
              backTo.current = null;
              // Created: the new row takes focus when it arrives. Not known: the list is what to look at.
              if (outcome !== "done") returnFocus();
              return;
            }
            // A new code takes focus for itself. Without one, nothing else will.
            if (outcome === "unknown") returnFocus();
          }}
        />
      ) : (
        body
      )}
    </div>
  );
}

/**
 * The Guest access part, above the codes.
 *
 * OFF IS A SENTENCE AND ONE ACT. ON IS ONE ROW: what it opens, when it ends and when a guest last
 * came in, with the two acts the engine allows. THE PASS HAS NO CODE, so nothing here regenerates,
 * copies or shows one, and its row is never on the code list. Its ending and its removal are the
 * codes' own builtins, which is why the questions are `CodeAsk`'s, in the pass's words.
 *
 * AN ACT THAT IS NOT LEGAL IS ABSENT: no Allow guests with no gate or no owner, and, as on a code,
 * no new end date for a pass minted under another organization or on a site with no owner.
 */
function GuestAccess({
  site,
  pass,
  canAllow,
  now,
  access,
  asked,
  onAllow,
  onAsk,
  onKeep,
  onAnswered,
}: {
  site: SiteRow;
  pass: AccessCodeRow | undefined;
  canAllow: boolean;
  now: number;
  access: AccessCodes;
  asked: RowAct | null;
  onAllow: () => void;
  onAsk: (event: MouseEvent<HTMLElement>, act: RowAct) => void;
  onKeep: () => void;
  onAnswered: (act: RowAct, outcome: WriteOutcome) => void;
}) {
  const stale = pass !== undefined && (pass.accountId !== "" || site.accountId !== "") && !sameEntityId(pass.accountId, site.accountId);
  const ownerless = siteIsClusterOwned(site);
  const opensNothing = stale || ownerless;
  const writing = access.busy !== null;
  return (
    <section className="access-guest" aria-label="Guest access">
      <Subhead
        actions={
          pass === undefined && canAllow ? (
            <Button disabled={writing} onClick={onAllow}>
              Allow guests
            </Button>
          ) : null
        }
      >
        Guest access
      </Subhead>
      {pass === undefined ? (
        <Caption>Off. Only someone with a code can open it.</Caption>
      ) : (
        <>
          <RecordList as="ul" label="Guest pass">
            <RecordRow
              name={pass.label}
              secondary={
                <span className="os-record-facts">
                  {stale ? (
                    <span className="access-code-warn">Allowed under another organization, so it opens nothing</span>
                  ) : access.gates.value !== null && !access.gates.value.some((gate) => gate.gate === pass.gate) ? (
                    <span className="access-code-warn">Opens {pass.gate}, which this app no longer declares</span>
                  ) : (
                    <span>Opens {pass.gate}{pass.subject === "" ? "" : ` for ${pass.subject}`}</span>
                  )}
                </span>
              }
              state={stale ? undefined : endWord(pass.expiresAt, now)}
              tone={hasExpired(pass.expiresAt, now) ? "warn" : "muted"}
              stateTitle={formatMoment(pass.expiresAt)}
              stateExtra={stale ? undefined : <span className="access-code-opened" title={pass.lastOpenedAt === "" ? undefined : formatMoment(pass.lastOpenedAt)}>{lastOpenedWord(pass.lastOpenedAt, pass.issuedAt, now)}</span>}
              actions={
                <>
                  {opensNothing ? null : (
                    <IconButton label="Change end date" disabled={writing} aria-expanded={asked === "setExpiry"} onClick={(event) => onAsk(event, "setExpiry")}>
                      <CalendarClock size={16} aria-hidden />
                    </IconButton>
                  )}
                  <IconButton label="Turn off" disabled={writing} aria-expanded={asked === "remove"} onClick={(event) => onAsk(event, "remove")}>
                    <Power size={16} aria-hidden />
                  </IconButton>
                </>
              }
            />
            {asked !== null ? <CodeAsk key={asked} code={pass} act={asked} opensNothing={opensNothing} now={now} access={access} onKeep={onKeep} onAnswered={(outcome) => onAnswered(asked, outcome)} /> : null}
          </RecordList>
        </>
      )}
    </section>
  );
}

function NoGates() {
  return (
    <>
      A code opens a gate, and an app declares one by marking a query or mutation with <code className="os-mono">@accessGate</code>. Deploy
      that, and you can add codes here.
    </>
  );
}

/**
 * One code on the list, and the question it has open if it has one.
 *
 * WHAT CAN BE SAID AND WHAT CANNOT. A gate is called undeclared only once the gates
 * have been read: not knowing is not the same as no, and the row's expiry would be
 * the wrong thing to doubt. An organization that differs is D15's race, a code that
 * opens nothing at all, so it draws no expiry and offers only the one act that helps.
 *
 * THAT ACT IS NOT EVERYBODY'S. The shared gate judges the organization COPIED onto the
 * code, which on this row is not the site's, so the page having admitted this person says
 * nothing about it. `partsForOrganization` asks what the gate asks: somebody with that
 * organization, or an operator, who has no scoped decisions and passes as they hold.
 *
 * A SITE WITH NO OWNER leaves the same one act, and it is the state that can change: give
 * the site an owner and the code works again, so the row keeps its facts and its time, and
 * what is true of every row is said once, over the list.
 */
function CodeEntry({
  code,
  site,
  gates,
  held,
  now,
  access,
  asked,
  onAsk,
  onKeep,
  onAnswered,
}: {
  code: AccessCodeRow;
  site: SiteRow;
  gates: readonly AccessGateRow[] | null;
  held: PartsHeld;
  now: number;
  access: AccessCodes;
  asked: RowAct | null;
  onAsk: (event: MouseEvent<HTMLElement>, act: RowAct) => void;
  onKeep: () => void;
  onAnswered: (act: RowAct, outcome: WriteOutcome) => void;
}) {
  // `sameEntityId` answers false for two empties, and a site with no organization
  // and a code that copied none are the same organization.
  const stale = (code.accountId !== "" || site.accountId !== "") && !sameEntityId(code.accountId, site.accountId);
  const ownerless = siteIsClusterOwned(site);
  const opensNothing = stale || ownerless;
  const canRemove = !stale || partsForOrganization(code.accountId, held).access;
  const undeclared = gates !== null && !gates.some((gate) => gate.gate === code.gate);
  const expired = hasExpired(code.expiresAt, now);
  const writing = access.busy !== null;
  const lastOpened = lastOpenedWord(code.lastOpenedAt, code.issuedAt, now);
  const unlocks = `Unlocks ${code.gate}${code.subject === "" ? "" : ` for ${code.subject}`}`;
  return (
    <>
      <RecordRow
        name={code.label}
        secondary={
          <span className="os-record-facts">
            {stale ? (
              <span className="access-code-warn">Minted under another organization, so it opens nothing{canRemove ? "" : ". A cluster operator must remove it"}</span>
            ) : undeclared ? (
              <span className="access-code-warn">{unlocks}, which this app no longer declares</span>
            ) : (
              <span>{unlocks}</span>
            )}
            {code.codeHint === "" ? null : (
              <span>
                Ends in <span className="os-mono">{code.codeHint}</span>
              </span>
            )}
          </span>
        }
        state={stale ? undefined : expiryWord(code.expiresAt, now)}
        tone={expired ? "warn" : "muted"}
        stateTitle={formatMoment(code.expiresAt)}
        stateExtra={stale ? undefined : <span className="access-code-opened" title={code.lastOpenedAt === "" ? undefined : formatMoment(code.lastOpenedAt)}>{lastOpened}</span>}
        actions={
          <>
            {opensNothing ? null : (
              <>
                <IconButton label={`Regenerate ${code.label}`} disabled={writing} aria-expanded={asked === "regenerate"} onClick={(event) => onAsk(event, "regenerate")}>
                  <RefreshCw size={16} aria-hidden />
                </IconButton>
                <IconButton label={`Change expiry of ${code.label}`} disabled={writing} aria-expanded={asked === "setExpiry"} onClick={(event) => onAsk(event, "setExpiry")}>
                  <CalendarClock size={16} aria-hidden />
                </IconButton>
              </>
            )}
            {canRemove ? (
              <IconButton label={`Remove ${code.label}`} disabled={writing} aria-expanded={asked === "remove"} onClick={(event) => onAsk(event, "remove")}>
                <Trash2 size={16} aria-hidden />
              </IconButton>
            ) : null}
          </>
        }
      />
      {asked !== null ? <CodeAsk key={asked} code={code} act={asked} opensNothing={opensNothing} now={now} access={access} onKeep={onKeep} onAnswered={(outcome) => onAnswered(asked, outcome)} /> : null}
    </>
  );
}

/** What a question says: what is being asked, then what it costs, above the buttons (rule 8). */
function askWords(act: RowAct, label: string, expired: boolean, opensNothing: boolean, guest = false): { question: string; consequence: string; answer: string; answering: string } {
  if (guest) return guestAskWords(act, expired, opensNothing);
  const nobody = "It has already expired, so nobody is using it.";
  switch (act) {
    case "regenerate":
      return {
        question: `Regenerate ${label}?`,
        consequence: expired ? `${nobody} Choose when the new code expires.` : "The current code stops working and anyone using it is taken out now.",
        answer: "Regenerate",
        answering: "Regenerating",
      };
    case "remove":
      return {
        question: `Remove ${label}?`,
        consequence: `${opensNothing ? "It already opens nothing." : expired ? nobody : "The code stops working and anyone using it is taken out now."} Removal is final: to give someone access again, add a new code.`,
        answer: "Remove",
        answering: "Removing",
      };
    case "setExpiry":
      return {
        question: `Change when ${label} expires`,
        consequence: expired ? "The same code works again until the new time." : "The code stays the same, and the new time applies at once.",
        answer: "Change expiry",
        answering: "Changing",
      };
  }
}

/** What the guest pass's two questions say. Regenerate is never asked of a pass: it has no code. */
function guestAskWords(act: RowAct, expired: boolean, opensNothing: boolean): { question: string; consequence: string; answer: string; answering: string } {
  if (act === "setExpiry") {
    return {
      question: "Change when guest access ends",
      consequence: expired ? "Guests can come in again until the new time." : "The new time applies at once.",
      answer: "Set end date",
      answering: "Changing",
    };
  }
  return {
    question: "Turn off guest access?",
    consequence: opensNothing || expired ? "Nobody is in as a guest now. To let guests in again, allow guests again." : "Everyone in as a guest is taken out now.",
    answer: "Turn off guest access",
    answering: "Turning off",
  };
}

/**
 * The question under a row.
 *
 * AN EXPIRED CODE'S REGENERATE NEEDS A NEW EXPIRY, or the engine refuses it: a fresh
 * secret that has never opened anything and never would. So the question asks for
 * one where it will be needed rather than offering what would be taken back, and it
 * can start asking while it is open, if the code expires under it.
 *
 * THE WHOLE OF IT IS HELD, not only its buttons, because what it says can change
 * while the same buttons stay: that, and a refusal drawn beside them.
 */
function CodeAsk({
  code,
  act,
  opensNothing,
  now,
  access,
  onKeep,
  onAnswered,
}: {
  /** Also the guest pass, which is told by `code.guest`. */
  code: AccessCodeRow;
  act: RowAct;
  opensNothing: boolean;
  now: number;
  access: AccessCodes;
  onKeep: () => void;
  onAnswered: (outcome: WriteOutcome) => void;
}) {
  const [draft, setDraft] = useState<ExpiryDraft>(DEFAULT_EXPIRY);
  const root = useRef<HTMLDivElement>(null);
  const expired = hasExpired(code.expiresAt, now);
  const asksExpiry = act === "setExpiry" || (act === "regenerate" && expired);
  const { question, consequence, answer: answerWord, answering } = askWords(act, code.label, expired, opensNothing, code.guest);
  const refusal = access.refusal?.codeId === code.id ? access.refusal.sentence : "";

  // Into the question, so a keyboard is on the way out rather than on the act, and
  // the list above it is not between the person and their answer.
  useEffect(() => {
    root.current?.querySelector<HTMLElement>("button, input")?.focus();
  }, []);

  async function answer(): Promise<void> {
    let outcome: WriteOutcome;
    if (act === "remove") outcome = await access.remove(code);
    else if (!asksExpiry) outcome = await access.regenerate(code);
    else {
      // From the press, and not from when the question opened.
      const expiresAt = expiryFromDraft(draft, Date.now());
      if (expiresAt === null) return;
      outcome = await (act === "setExpiry" ? access.setExpiry(code, expiresAt) : access.regenerate(code, expiresAt));
    }
    if (outcome !== "refused") onAnswered(outcome);
  }

  return (
    <div className="access-code-ask" ref={root}>
      <InPlaceAsk role="group" aria-label={question}>
        <Notice tone="warn" sentence={question}>
          <p className="os-caption">{consequence}</p>
          {asksExpiry ? <ExpiryField id={`access-expiry-${code.id}`} draft={draft} now={now} onChange={setDraft} label={code.guest ? "Ends" : "Expires"} /> : null}
          <div className="os-panel-actions">
            <Button disabled={access.busy !== null} onClick={onKeep}>
              {act === "setExpiry" ? "Cancel" : "Keep it"}
            </Button>
            <Button
              tone={act === "setExpiry" ? "primary" : "danger"}
              busy={access.busy === act}
              busyLabel={answering}
              disabled={asksExpiry && expiryFromDraft(draft, now) === null}
              onClick={() => void answer()}
            >
              {answerWord}
            </Button>
          </div>
        </Notice>
        {refusal === "" ? null : <Notice tone="error" sentence={refusal} />}
      </InPlaceAsk>
    </div>
  );
}

/** The five presets and Custom, with the date behind Custom naming the zone it is read in. */
function ExpiryField({ id, draft, now, onChange, label = "Expires" }: { id: string; draft: ExpiryDraft; now: number; onChange: (next: ExpiryDraft) => void; label?: "Expires" | "Ends" }) {
  const dateLabel = `${label === "Ends" ? "Ends" : "Expires"} at · ${browserZone()}`;
  return (
    <>
      <Field label={label}>
        <Select id={id} label={label} value={draft.choice} onChange={(choice) => onChange({ ...draft, choice })}>
          {EXPIRY_PRESETS.map((preset) => (
            <option key={preset.id} value={preset.id}>
              {preset.label}
            </option>
          ))}
          <option value={CUSTOM_EXPIRY}>Custom</option>
        </Select>
      </Field>
      {draft.choice === CUSTOM_EXPIRY ? (
        <Field label={dateLabel}>
          <DateTimeInput id={`${id}-at`} label={dateLabel} value={draft.at} min={instantToLocal(new Date(now))} onChange={(at) => onChange({ ...draft, at })} />
        </Field>
      ) : null}
    </>
  );
}

/**
 * The short form behind Add.
 *
 * THE GATE IS DERIVED, NOT COPIED INTO STATE: one declared gate is the answer until
 * somebody says otherwise, and several are a choice nobody has made yet. A subject
 * typed for a gate that was then changed is not sent, because that gate does not
 * read one. A refusal lands here, beside what it refused, with the draft untouched.
 */
function AddCodeForm({
  mode,
  siteId,
  gates,
  now,
  access,
  onCancel,
  onCreated,
  onWriting,
}: {
  /** A code's form, or the same form for the guest pass: without the code, and said in its own words. */
  mode: "code" | "guest";
  siteId: string;
  gates: readonly AccessGateRow[];
  now: number;
  access: AccessCodes;
  onCancel: () => void;
  onCreated: (outcome: WriteOutcome) => void;
  /** The guest pass's write started (true), or did not land (false). */
  onWriting: (writing: boolean) => void;
}) {
  const guest = mode === "guest";
  const [name, setName] = useState(guest ? "Guests" : "");
  const [chosenGate, setChosenGate] = useState("");
  const [subject, setSubject] = useState("");
  const [draft, setDraft] = useState<ExpiryDraft>(DEFAULT_EXPIRY);
  const root = useRef<HTMLElement>(null);
  // THE FORM IS OVER ONCE THE WRITE HAS ANSWERED. The hook's guard lets go in `finally`,
  // and the form is still drawn until React next renders. A disabled Create stops a
  // click in that frame, and nothing stops Enter in a field, which would mint a second
  // code that nobody holds.
  const answered = useRef(false);

  useEffect(() => {
    root.current?.querySelector("input")?.focus();
  }, []);

  const gate = chosenGate !== "" ? chosenGate : gates.length === 1 ? (gates[0]?.gate ?? "") : "";
  const usesSubject = gates.some((declared) => declared.gate === gate && declared.usesSubject);
  const complete = name.trim() !== "" && gate !== "" && expiryFromDraft(draft, now) !== null;
  const refusal = access.refusal?.codeId === "" ? access.refusal.sentence : "";

  async function create(): Promise<void> {
    if (answered.current) return;
    // From the press: a form left open must not shorten the code it makes.
    const expiresAt = expiryFromDraft(draft, Date.now());
    if (!complete || expiresAt === null) return;
    if (guest) onWriting(true);
    const outcome = await (guest
      ? access.allowGuests({ label: name, gate, subject: usesSubject ? subject : "", expiresAt })
      : access.create({ label: name, gate, subject: usesSubject ? subject : "", expiresAt }));
    if (guest && outcome !== "done") onWriting(false);
    if (outcome === "refused") return;
    answered.current = true;
    onCreated(outcome);
  }

  return (
    <section className="os-form access-code-form" aria-label={guest ? "Allow guests" : "Add an access code"} ref={root}>
      <Field label="Name">
        <Input id={`access-name-${siteId}`} label="Name" value={name} onChange={setName} placeholder={guest ? "What to call them" : "Who it is for"} onEnter={() => void create()} />
      </Field>
      <Field label={guest ? "Opens" : "Unlocks"}>
        <Select id={`access-gate-${siteId}`} label={guest ? "Opens" : "Unlocks"} value={gate} onChange={setChosenGate}>
          {gates.length === 1 ? null : <option value="">Choose a gate</option>}
          {gates.map((declared) => (
            <option key={declared.gate} value={declared.gate}>
              {declared.gate}
            </option>
          ))}
        </Select>
      </Field>
      {usesSubject ? (
        <Field label={guest ? "Subject" : "Subject (optional)"}>
          <Input id={`access-subject-${siteId}`} label={guest ? "Subject" : "Subject (optional)"} value={subject} onChange={setSubject} placeholder="Whose part of the app it opens" onEnter={() => void create()} />
        </Field>
      ) : null}
      <ExpiryField id={`access-expiry-${siteId}`} draft={draft} now={now} onChange={setDraft} label={guest ? "Ends" : "Expires"} />
      {refusal === "" ? null : <Notice tone="error" sentence={refusal} />}
      <div className="os-panel-actions">
        <Button disabled={access.busy !== null} onClick={onCancel}>
          Cancel
        </Button>
        <Button tone="primary" busy={access.busy === (guest ? "allowGuests" : "create")} busyLabel={guest ? "Allowing" : "Creating"} disabled={!complete} onClick={() => void create()}>
          {guest ? "Allow guests" : "Create"}
        </Button>
      </div>
    </section>
  );
}

/**
 * The plain code, the one time it exists on a screen.
 *
 * ONLY DONE IS HELD. Copy changes its own name when it has copied, which would hold
 * Done again on every copy and again when the word decays, and a stray press on it
 * costs nothing.
 *
 * FOCUS COMES HERE because the control that asked has gone with its form or question,
 * and because a row low in a long list asked for something that appears at the top.
 */
function MintedCodeNotice({ code, label, onDone }: { code: string; label: string; onDone: () => void }) {
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    root.current?.querySelector("button")?.focus();
  }, [code]);
  return (
    <div className="access-code-once" ref={root}>
      <Notice tone="warn" sentence={`Copy the code for ${label} now. This is the only time it is shown.`}>
        <div className="access-code-once-line">
          <CopyValue value={code} label="access code" displayValue={<span className="access-code-value os-mono">{code}</span>} />
          <InPlaceAsk className="os-panel-actions">
            <Button onClick={onDone}>Done</Button>
          </InPlaceAsk>
        </div>
      </Notice>
    </div>
  );
}
