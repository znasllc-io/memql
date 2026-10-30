import { useEffect, useRef, useState, type MouseEvent, type ReactNode } from "react";
import { CalendarClock, Lock, RefreshCw, Trash2 } from "lucide-react";
import { sameEntityId } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, CopyValue, DateTimeInput, EmptyState, Field, Input, Notice, RecordListSkeleton, Select, Subhead, useNow } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { IconButton } from "../../../kit/IconButton";
import { formatMoment } from "../../../kit/format";
import { InPlaceAsk } from "../../../kit/pressHold";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { browserZone, instantToLocal } from "../../email/when";
import type { SiteRow } from "../rows";
import { CUSTOM_EXPIRY, DEFAULT_EXPIRY, EXPIRY_PRESETS, expiryFromDraft, expiryWord, hasExpired, lastOpenedWord, type ExpiryDraft } from "./expiry";
import { useAccessCodes, type AccessCodeRow, type AccessCodes, type AccessGateRow } from "./useAccessCodes";

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

/** One clock for the panel (`useNow`), so a code turns to Expired with the panel left open. */
const CLOCK_MS = 5_000;

type RowAct = "regenerate" | "setExpiry" | "remove";

/** The panel: the codes, the way to add one, and whichever single question is open. */
export function AccessCodesPanel({ site }: { site: SiteRow }) {
  const access = useAccessCodes(site);
  const now = useNow(CLOCK_MS).getTime();
  const [adding, setAdding] = useState(false);
  // The gates the form opened with. A re-read that fails answers null, and a draft
  // somebody is part-way through must not go with it.
  const [formGates, setFormGates] = useState<readonly AccessGateRow[]>([]);
  const [asked, setAsked] = useState<{ codeId: string; act: RowAct } | null>(null);

  // WHERE FOCUS GOES BACK TO. The form, the question and the code each take focus
  // and then leave, and focus left on nothing in a modal sends the next Tab to the
  // dialog's header. The control that asked is usually still there; Add is not (it
  // is absent while the form is open), and neither is the row of a removed code.
  const root = useRef<HTMLDivElement>(null);
  const opener = useRef<HTMLElement | null>(null);
  const [refocus, setRefocus] = useState(0);
  useEffect(() => {
    if (refocus === 0) return;
    const back = opener.current?.isConnected ? opener.current : root.current?.querySelector<HTMLElement>(".os-add-button");
    back?.focus();
  }, [refocus]);
  const returnFocus = (): void => setRefocus((n) => n + 1);

  const askFor = (event: MouseEvent<HTMLElement>, codeId: string, act: RowAct): void => {
    opener.current = event.currentTarget;
    access.clearRefusal();
    setAsked({ codeId, act });
  };

  const codes = access.codes.value;
  const gates = access.gates.value;
  const noGates = gates !== null && gates.length === 0;
  const canAdd = gates !== null && gates.length > 0;
  const gatesOut = gates === null && access.gates.state !== "failed";

  // A REFUSAL WHOSE ROW HAS GONE. The re-read after a refused regenerate of a code
  // somebody else removed takes the row, and the question under it, off the list,
  // and a refusal nobody can read is a refusal nobody can act on.
  const refusal = access.refusal;
  const refusedRowGone = refusal !== null && refusal.codeId !== "" && !(codes ?? []).some((code) => code.id === refusal.codeId);

  let body: ReactNode;
  if (!access.connected) {
    body = <Notice tone="error" sentence="Not connected to the cluster, so the access codes could not be read." />;
  } else if (codes === null) {
    // A failure is said above, and a skeleton beneath it would claim a read is still out.
    body = access.codes.state === "failed" ? null : <RecordListSkeleton label="Loading access codes" />;
  } else if (codes.length > 0) {
    body = (
      <>
        {noGates ? <Caption><NoGates /></Caption> : null}
        <RecordList as="ul" label="Access codes">
          {codes.map((code) => (
            <CodeEntry
              key={code.id}
              code={code}
              site={site}
              gates={gates}
              now={now}
              access={access}
              asked={asked?.codeId === code.id ? asked.act : null}
              onAsk={(event, act) => askFor(event, code.id, act)}
              onKeep={() => {
                access.clearRefusal();
                setAsked(null);
                returnFocus();
              }}
              onAnswered={(act) => {
                setAsked(null);
                // A regenerate's code takes focus for itself, and gives it back on Done.
                if (act !== "regenerate") returnFocus();
              }}
            />
          ))}
        </RecordList>
      </>
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
    <div className="access-codes" ref={root}>
      {/* No heading over nothing: with no gates and no codes, the explanation is the panel. */}
      {noGates && codes !== null && codes.length === 0 ? null : (
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
                  setAdding(true);
                }}
              />
            ) : null
          }
        >
          {adding ? "Add an access code" : "Codes"}
        </Subhead>
      )}
      {access.readFailures.map((sentence) => (
        <Notice key={sentence} tone="error" sentence={sentence}>
          <Button onClick={access.reread}>Try again</Button>
        </Notice>
      ))}
      {refusedRowGone ? <Notice tone="error" sentence={refusal.sentence} /> : null}
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
      {adding ? (
        <AddCodeForm
          siteId={site.id}
          gates={gates ?? formGates}
          now={now}
          access={access}
          onCancel={() => {
            access.clearRefusal();
            setAdding(false);
            returnFocus();
          }}
          onCreated={() => setAdding(false)}
        />
      ) : (
        body
      )}
    </div>
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
 */
function CodeEntry({
  code,
  site,
  gates,
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
  now: number;
  access: AccessCodes;
  asked: RowAct | null;
  onAsk: (event: MouseEvent<HTMLElement>, act: RowAct) => void;
  onKeep: () => void;
  onAnswered: (act: RowAct) => void;
}) {
  // `sameEntityId` answers false for two empties, and a site with no organization
  // and a code that copied none are the same organization.
  const opensNothing = (code.accountId !== "" || site.accountId !== "") && !sameEntityId(code.accountId, site.accountId);
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
            {opensNothing || undeclared ? (
              <span className="access-code-warn">{opensNothing ? "Minted under another organization, so it opens nothing" : `${unlocks}, which this app no longer declares`}</span>
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
        state={opensNothing ? undefined : expiryWord(code.expiresAt, now)}
        tone={expired ? "warn" : "muted"}
        stateTitle={formatMoment(code.expiresAt)}
        stateExtra={opensNothing ? undefined : <span className="access-code-opened" title={code.lastOpenedAt === "" ? undefined : formatMoment(code.lastOpenedAt)}>{lastOpened}</span>}
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
            <IconButton label={`Remove ${code.label}`} disabled={writing} aria-expanded={asked === "remove"} onClick={(event) => onAsk(event, "remove")}>
              <Trash2 size={16} aria-hidden />
            </IconButton>
          </>
        }
      />
      {asked !== null ? <CodeAsk key={asked} code={code} act={asked} now={now} access={access} onKeep={onKeep} onAnswered={() => onAnswered(asked)} /> : null}
    </>
  );
}

/** What a question says: what is being asked, then what it costs, above the buttons (rule 8). */
function askWords(act: RowAct, label: string, expired: boolean): { question: string; consequence: string; answer: string; answering: string } {
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
        consequence: `${expired ? nobody : "The code stops working and anyone using it is taken out now."} Removal is final: to give someone access again, add a new code.`,
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
  now,
  access,
  onKeep,
  onAnswered,
}: {
  code: AccessCodeRow;
  act: RowAct;
  now: number;
  access: AccessCodes;
  onKeep: () => void;
  onAnswered: () => void;
}) {
  const [draft, setDraft] = useState<ExpiryDraft>(DEFAULT_EXPIRY);
  const root = useRef<HTMLDivElement>(null);
  const expired = hasExpired(code.expiresAt, now);
  const asksExpiry = act === "setExpiry" || (act === "regenerate" && expired);
  const { question, consequence, answer: answerWord, answering } = askWords(act, code.label, expired);
  const refusal = access.refusal?.codeId === code.id ? access.refusal.sentence : "";

  // Into the question, so a keyboard is on the way out rather than on the act, and
  // the list above it is not between the person and their answer.
  useEffect(() => {
    root.current?.querySelector<HTMLElement>("button, input")?.focus();
  }, []);

  async function answer(): Promise<void> {
    let done: boolean;
    if (act === "remove") done = await access.remove(code);
    else if (!asksExpiry) done = await access.regenerate(code);
    else {
      // From the press, and not from when the question opened.
      const expiresAt = expiryFromDraft(draft, Date.now());
      if (expiresAt === null) return;
      done = await (act === "setExpiry" ? access.setExpiry(code, expiresAt) : access.regenerate(code, expiresAt));
    }
    if (done) onAnswered();
  }

  return (
    <div className="access-code-ask" ref={root}>
      <InPlaceAsk role="group" aria-label={question}>
        <Notice tone="warn" sentence={question}>
          <p className="os-caption">{consequence}</p>
          {asksExpiry ? <ExpiryField id={`access-expiry-${code.id}`} draft={draft} now={now} onChange={setDraft} /> : null}
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
function ExpiryField({ id, draft, now, onChange }: { id: string; draft: ExpiryDraft; now: number; onChange: (next: ExpiryDraft) => void }) {
  const dateLabel = `Expires at · ${browserZone()}`;
  return (
    <>
      <Field label="Expires">
        <Select id={id} label="Expires" value={draft.choice} onChange={(choice) => onChange({ ...draft, choice })}>
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
  siteId,
  gates,
  now,
  access,
  onCancel,
  onCreated,
}: {
  siteId: string;
  gates: readonly AccessGateRow[];
  now: number;
  access: AccessCodes;
  onCancel: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [chosenGate, setChosenGate] = useState("");
  const [subject, setSubject] = useState("");
  const [draft, setDraft] = useState<ExpiryDraft>(DEFAULT_EXPIRY);
  const root = useRef<HTMLElement>(null);

  useEffect(() => {
    root.current?.querySelector("input")?.focus();
  }, []);

  const gate = chosenGate !== "" ? chosenGate : gates.length === 1 ? (gates[0]?.gate ?? "") : "";
  const usesSubject = gates.some((declared) => declared.gate === gate && declared.usesSubject);
  const complete = name.trim() !== "" && gate !== "" && expiryFromDraft(draft, now) !== null;
  const refusal = access.refusal?.codeId === "" ? access.refusal.sentence : "";

  async function create(): Promise<void> {
    // From the press: a form left open must not shorten the code it makes.
    const expiresAt = expiryFromDraft(draft, Date.now());
    if (!complete || expiresAt === null) return;
    if (await access.create({ label: name, gate, subject: usesSubject ? subject : "", expiresAt })) onCreated();
  }

  return (
    <section className="os-form access-code-form" aria-label="Add an access code" ref={root}>
      <Field label="Name">
        <Input id={`access-name-${siteId}`} label="Name" value={name} onChange={setName} placeholder="Who it is for" onEnter={() => void create()} />
      </Field>
      <Field label="Unlocks">
        <Select id={`access-gate-${siteId}`} label="Unlocks" value={gate} onChange={setChosenGate}>
          {gates.length === 1 ? null : <option value="">Choose a gate</option>}
          {gates.map((declared) => (
            <option key={declared.gate} value={declared.gate}>
              {declared.gate}
            </option>
          ))}
        </Select>
      </Field>
      {usesSubject ? (
        <Field label="Subject">
          <Input id={`access-subject-${siteId}`} label="Subject" value={subject} onChange={setSubject} placeholder="Whose part of the app it opens" onEnter={() => void create()} />
        </Field>
      ) : null}
      <ExpiryField id={`access-expiry-${siteId}`} draft={draft} now={now} onChange={setDraft} />
      {refusal === "" ? null : <Notice tone="error" sentence={refusal} />}
      <div className="os-panel-actions">
        <Button disabled={access.busy !== null} onClick={onCancel}>
          Cancel
        </Button>
        <Button tone="primary" busy={access.busy === "create"} busyLabel="Creating" disabled={!complete} onClick={() => void create()}>
          Create
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
