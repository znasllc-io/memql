import { useEffect, useRef, useState } from "react";
import { Trash2 } from "lucide-react";

import { Button, Caption, Details, DetailsSection, Fact, Facts, Head, InlineSkeleton, Notice, SettingRow, Switch, formatFreshness, formatMoment, useNow } from "../../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../../kit/ActionBar";
import { IconButton } from "../../../kit/IconButton";
import { PageHelp } from "../../../kit/pageHelp";
import { InPlaceAsk } from "../../../kit/pressHold";
import { KeyLending } from "./KeyLending";
import { useElapsed } from "./useElapsed";
import { useKeyActivity, type VendorKeys } from "./useVendorKeys";
import { costFigure, MAX_NAME, nameProblem, stateWords, vendorName, type KeyCard } from "./words";

// One key's page (design record 2026-11-21-vendor-accounts-and-api-keys, U3
// and K5), in place of the list and under the trail back to it.
//
// WHAT IS ON IT DEPENDS ON WHOSE KEY IT IS, and an act that is not this
// reader's to take is absent, never disabled:
//
//   ITS OWNER      what the cluster knows about it, its settings (name, the
//                  default, Auto, removal), who it is lent to, and Check now.
//   A CLUSTER      the same facts, how many it is lent to and not who, Check
//   OWNER          now and Remove. They may stop a key on their cluster. They
//                  may not rename it, spend it, or see who its owner trusts.
//   SOMEBODY IT    the short card: its name, its vendor, whose it is and
//   IS LENT TO     whether it works. Nothing to press.
//
// THE STATE AND CHECK NOW STAND IN THE ONE ACTION BAR (DESIGN.md rule 12).
// Removal is with the key's other settings, behind a question asked in place
// that says what it does and what it does not: the key stops paying here, and
// it is not revoked at the vendor.
//
// NO VALUE IS EVER ON THIS PAGE. A card carries none, and there is no field
// here that could show one.

export function KeyDetail({
  card,
  keys,
  siblings,
  onBack,
  onRemoved,
}: {
  card: KeyCard;
  keys: VendorKeys;
  /** The reader's own keys, for the default and for a name already taken. */
  siblings: readonly KeyCard[];
  onBack: () => void;
  onRemoved: () => void;
}) {
  const now = useNow(30_000);
  const mine = card.relation === "mine";
  const vendor = vendorName(card.vendor);
  // What a key paid for is its owner's to read. Nobody else is asked for it.
  const activity = useKeyActivity(card.id, mine, 0);
  // What a check that HAPPENED has to add (the vendor could not be reached,
  // a hold was lifted): information. A check that did not happen is a failure
  // and is said as one, beside it and never in its place.
  const [note, setNote] = useState("");
  const [checkRefusal, setCheckRefusal] = useState("");
  const checking = keys.busy === `check:${card.id}`;
  const waited = useElapsed(checking);

  async function checkNow() {
    setNote("");
    setCheckRefusal("");
    const result = await keys.check(card.id);
    if (result.outcome === "done") setNote(result.card.note);
    else setCheckRefusal(result.sentence);
  }

  const words = stateWords(card);
  const tone: ActionBarTone = checking ? "busy" : card.state === "good" && !card.paused ? "live" : words.needsAttention ? "paused" : "none";
  const acts: Act[] = [];
  // Listing models is what a check is. A key that may not list them, and a
  // key that is somebody's short card, has no check to offer.
  if (card.full && card.checkedBy !== "use" && !checking) acts.push({ label: "Check now", tone: "primary", onAct: () => void checkNow() });

  return (
    <div className="os-apikey" data-os-page-context={JSON.stringify({ page: "API key", name: card.label, vendor })}>
      <Head
        navigationKey={`key:${card.id}`}
        title={card.label}
        breadcrumbs={[{ label: "API keys", onSelect: onBack }, { label: card.label }]}
        back={{ label: "API keys", onSelect: onBack }}
      />
      <PageHelp title="An API key">
        <p>The state is what the vendor last said about this key: a call it paid for, or a check. A check lists the vendor's models with the key, which sends no prompt and costs nothing. The cluster checks a key once a day when nothing has used it.</p>
        <p>A refused key pays for nothing until the vendor accepts it again. A key whose account is out of credit is tried again by the next call an hour later. Removing a key stops it paying here at once. It does not revoke the key at the vendor: delete it there too.</p>
        <p>The estimated cost is this cluster's arithmetic from each model's listed price. It is not the vendor's invoice.</p>
      </PageHelp>
      <Details>
        <DetailsSection title="Key">
          <Facts>
            <Fact label="Vendor" value={vendor} />
            {card.full ? <Fact label="Fingerprint" value={card.fingerprint} mono copyValue={card.fingerprint || undefined} /> : null}
            {!mine && card.full ? <Fact label="Owner" value={card.ownerName || "Unknown"} /> : null}
            {!card.full ? <Fact label="Lent by" value={card.ownerName || "Its owner"} /> : null}
            {card.full ? <Fact label="Added" value={formatMoment(card.addedAt)} copyValue={card.addedAt || undefined} /> : null}
            {card.full ? <Fact label="Last used" value={card.lastUsedAt === "" ? "Not used yet" : formatFreshness(card.lastUsedAt, now)} title={card.lastUsedAt || undefined} /> : null}
            {card.full ? <Fact label="Last checked" value={lastChecked(card, now)} title={card.checkedAt || undefined} /> : null}
            {!mine && card.full ? <Fact label="Lent to" value={card.lentTo === 0 ? "Nobody" : card.lentTo === 1 ? "1 person or group" : `${card.lentTo} people or groups`} /> : null}
            {mine ? (
              <Fact
                label="Estimated cost this month"
                value={activity.state === "ready" && activity.activity !== null ? costFigure(activity.activity) : activity.state === "failed" ? "Not reported" : <InlineSkeleton label="Loading this month's cost" />}
                title={activity.error || undefined}
              />
            ) : null}
          </Facts>
          {mine ? <Caption>The cost is MemQL's estimate from each model's listed price, not your vendor's invoice.</Caption> : null}
        </DetailsSection>

        {card.full ? (
          <DetailsSection title="Settings">
            {mine ? <SettingRow label="Name"><RenameField card={card} keys={keys} taken={siblings.filter((held) => held.id !== card.id).map((held) => held.label)} /></SettingRow> : null}
            {mine && siblings.filter((held) => held.vendor === card.vendor).length > 1 ? (
              <SettingRow label="Default"><DefaultControl card={card} keys={keys} vendor={vendor} /></SettingRow>
            ) : null}
            {mine ? <SettingRow label="Auto"><AutoControl card={card} keys={keys} /></SettingRow> : null}
            <RemoveControl card={card} keys={keys} vendor={vendor} mine={mine} onRemoved={onRemoved} />
          </DetailsSection>
        ) : null}

        {mine ? (
          <DetailsSection title="Lending">
            <KeyLending card={card} keys={keys} activity={activity} />
          </DetailsSection>
        ) : null}
      </Details>

      {note !== "" ? <Notice tone="info" sentence={note} /> : null}
      {checkRefusal !== "" ? <Notice tone="error" sentence="The key was not checked." detail={checkRefusal} /> : null}
      <ActionBar
        state={checking ? `Checking with ${vendor}` : words.word}
        detail={checking ? "Listing models. No prompt is sent." : stateDetail(card, now)}
        tone={tone}
        meta={checking ? waited : undefined}
        live
        acts={acts}
        scope={card.id}
      />
    </div>
  );
}

/** When the key was last checked, and what the check found. */
function lastChecked(card: KeyCard, now: Date): string {
  if (card.checkedBy === "use") return "Checked by the calls it pays for";
  if (card.checkedAt === "") return "Not checked yet";
  const when = formatFreshness(card.checkedAt, now);
  switch (card.state) {
    case "good":
      return `${when}, accepted`;
    case "refused":
      return `${when}, refused`;
    case "credit":
      return `${when}, out of credit`;
  }
  return when;
}

/** What the state means now, in one clause for the bar. */
function stateDetail(card: KeyCard, now: Date): string {
  if (card.paused) return `${card.ownerName || "Its owner"} has paused lending it to you.`;
  if (!card.full) return card.state === "good" ? "It can pay for your calls." : "It is not paying for calls now.";
  switch (card.state) {
    case "refused":
      return card.detail === "" ? `${vendorName(card.vendor)} does not accept this key.` : card.detail;
    case "credit": {
      const until = card.heldUntil === "" ? "" : ` A call may try it again after ${formatMoment(card.heldUntil)}.`;
      return `${card.detail === "" ? "Its account is out of credit." : card.detail}${until}`;
    }
    case "good":
      return card.checkedAt === "" ? "" : `Last accepted ${formatFreshness(card.checkedAt, now)}`;
  }
  return card.checkedBy === "use" ? "Checked by the calls it pays for." : "";
}

function RenameField({ card, keys, taken }: { card: KeyCard; keys: VendorKeys; taken: readonly string[] }) {
  const [draft, setDraft] = useState(card.label);
  const [refusal, setRefusal] = useState("");
  // The card wins: a rename that landed from another tab reaches this field.
  useEffect(() => {
    setDraft(card.label);
  }, [card.label]);
  const busy = keys.busy === `update:${card.id}`;
  const problem = nameProblem(draft, taken);
  const changed = draft.trim() !== "" && draft.trim() !== card.label.trim();

  return (
    <>
      <form
        className="os-form-row fleet-machine-rename"
        onSubmit={(e) => {
          e.preventDefault();
          if (!changed || problem !== "") return;
          setRefusal("");
          void keys.update(card.id, { label: draft.trim() }).then((result) => {
            if (result.outcome !== "done") setRefusal(result.sentence);
          });
        }}
      >
        <label className="os-sr-only" htmlFor={`os-apikey-name-${card.id}`}>
          Name for {card.label}
        </label>
        <input id={`os-apikey-name-${card.id}`} className="os-input" value={draft} maxLength={MAX_NAME} disabled={busy} aria-invalid={problem !== "" || undefined} onChange={(e) => setDraft(e.target.value)} />
        <Button type="submit" tone="primary" disabled={!changed || problem !== ""} busy={busy} busyLabel="Saving...">
          Rename
        </Button>
      </form>
      {problem !== "" && changed ? <Caption>{problem}</Caption> : null}
      {refusal !== "" ? <Notice tone="error" sentence="The name was not changed." detail={refusal} /> : null}
    </>
  );
}

function DefaultControl({ card, keys, vendor }: { card: KeyCard; keys: VendorKeys; vendor: string }) {
  const [refusal, setRefusal] = useState("");
  const busy = keys.busy === `update:${card.id}`;
  if (card.isDefault) return <Caption>Your default for {vendor}. It pays before your other {vendor} keys.</Caption>;
  return (
    <>
      <Button
        busy={busy}
        busyLabel="Saving..."
        onClick={() => {
          setRefusal("");
          void keys.update(card.id, { isDefault: true }).then((result) => {
            if (result.outcome !== "done") setRefusal(result.sentence);
          });
        }}
      >
        Make this the default
      </Button>
      <Caption>Your default {vendor} key pays first. This one pays only on a route that names it.</Caption>
      {refusal !== "" ? <Notice tone="error" sentence="The default was not changed." detail={refusal} /> : null}
    </>
  );
}

function AutoControl({ card, keys }: { card: KeyCard; keys: VendorKeys }) {
  const [refusal, setRefusal] = useState("");
  const busy = keys.busy === `update:${card.id}`;
  return (
    <>
      <Switch
        checked={card.autoUse}
        disabled={busy}
        onChange={(autoUse) => {
          setRefusal("");
          void keys.update(card.id, { autoUse }).then((result) => {
            if (result.outcome !== "done") setRefusal(result.sentence);
          });
        }}
      >
        Let Auto use this key
      </Switch>
      <Caption>{card.autoUse ? "Auto may pay with this key, for you and for the people it is lent to." : "Only a route you choose pays with this key. Auto does not."}</Caption>
      {refusal !== "" ? <Notice tone="error" sentence="That was not changed." detail={refusal} /> : null}
    </>
  );
}

// REMOVE THIS KEY. The confirmation is in the page and names the key, as a
// machine's is: a browser confirm blocks the shell, and "are you sure" with no
// name invites confirming the wrong one.
function RemoveControl({ card, keys, vendor, mine, onRemoved }: { card: KeyCard; keys: VendorKeys; vendor: string; mine: boolean; onRemoved: () => void }) {
  const [confirming, setConfirming] = useState(false);
  const [refusal, setRefusal] = useState("");
  const root = useRef<HTMLDivElement>(null);
  const wasConfirming = useRef(false);
  useEffect(() => {
    if (confirming) root.current?.focus();
    else if (wasConfirming.current) root.current?.querySelector<HTMLButtonElement>(".fleet-machine-remove")?.focus();
    wasConfirming.current = confirming;
  }, [confirming]);
  const busy = keys.busy === `remove:${card.id}`;

  if (!confirming) {
    return (
      <div className="fleet-machine-remove-row" ref={root}>
        <span>Remove key</span>
        <IconButton label="Remove this key" className="fleet-machine-remove" disabled={keys.busy !== ""} onClick={() => { setRefusal(""); setConfirming(true); }}>
          <Trash2 size={17} aria-hidden />
        </IconButton>
      </div>
    );
  }
  return (
    // The region is named for the state it puts the page in; the button inside
    // keeps the act's own name.
    <div ref={root} role="group" tabIndex={-1} aria-label={`Removing ${card.label}`}>
      <InPlaceAsk className="os-fleet-confirm">
        <p className="os-fleet-confirm-line">
          Remove <strong>{card.label}</strong>? It stops paying for calls at once{mine ? "" : `, for ${card.ownerName || "its owner"} and anybody it is lent to`}, and it is never used again. This does not revoke the key at {vendor}: delete it at {vendor} too.
        </p>
        {refusal !== "" ? <Notice tone="error" sentence="The key was not removed." detail={refusal} /> : null}
        <div className="os-panel-actions">
          <Button disabled={busy} onClick={() => setConfirming(false)}>
            Keep it
          </Button>
          <Button
            tone="danger"
            busy={busy}
            busyLabel="Removing..."
            onClick={() => {
              setRefusal("");
              void keys.remove(card.id).then((result) => {
                // The question stays open on a refusal, with the reason beside
                // it: closing would leave somebody believing it was removed.
                if (result.outcome === "done") onRemoved();
                else setRefusal(result.sentence);
              });
            }}
          >
            Remove {card.label}
          </Button>
        </div>
      </InPlaceAsk>
    </div>
  );
}
