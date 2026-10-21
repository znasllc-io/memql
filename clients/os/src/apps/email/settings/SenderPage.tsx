import { useEffect, useState } from "react";

import { Field, Fact, Facts, Head, Input, Notice, Panel, TextArea, formatMoment } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import type { CampaignWrites } from "../actions";
import { toKitActs } from "../acts";
import { senderIsRetired, senderLabel, type SenderIdentityRow } from "../rows";
import { WORDS } from "../words";
import { senderActs } from "./senderActs";

// ONE SENDER, ITS FACTS AND THE ACTS LEGAL FROM ITS STATE (DESIGN.md rule 12).
//
// Everything that changes the sender is in the bar on the window's bottom edge
// and nowhere else: Archive (which asks first), Edit, and -- once it is
// archived -- Restore. There is no delete: past campaigns name the sender and
// its sending reputation is kept against its address.
//
// THE PAGE STAYS WHEN ITS SENDER IS ARCHIVED. It is opened from the app's whole
// senders feed, not the filtered list, so archiving a sender with archived
// senders hidden does not drop the person back to a list the sender just left:
// the page shows the new state, and Restore is right there.

interface Draft {
  fromName: string;
  replyTo: string;
  notes: string;
}

const draftOf = (sender: SenderIdentityRow): Draft => ({
  fromName: sender.fromName,
  replyTo: sender.replyTo,
  notes: sender.notes,
});

export function SenderPage({ sender, organization, writes, onBack }: {
  sender: SenderIdentityRow;
  /** The organization's name, resolved by the page that holds this. */
  organization: string;
  writes: CampaignWrites;
  onBack: () => void;
}) {
  const archived = senderIsRetired(sender);
  const status = writes.senderStatus;
  const update = writes.updateSender;
  const [editing, setEditing] = useState(false);
  const [asking, setAsking] = useState<"" | "archive">("");
  const [draft, setDraft] = useState<Draft>(() => draftOf(sender));

  // The page's writes keep their last refusal for the life of the app; leaving
  // the page leaves it behind.
  useEffect(() => () => { status.reset(); update.reset(); }, []);

  // An archived sender is not edited: it comes back first. If it was archived
  // somewhere else while this form was open, the form goes with it.
  const editingNow = editing && !archived;
  const named = draft.fromName.trim() !== "";
  const changed =
    draft.fromName.trim() !== sender.fromName.trim() ||
    draft.replyTo.trim() !== sender.replyTo.trim() ||
    draft.notes.trim() !== sender.notes.trim();
  // Save is not offered until there is something to save and a name to save it
  // under, and the state words beside it say which is missing.
  const specs = senderActs(sender, editingNow, editingNow ? "" : asking).filter(
    (spec) => spec.id !== "save" || (named && changed),
  );
  const busyId = status.busy ? (archived ? "restore" : "archive-confirmed") : update.busy ? "save" : undefined;

  function run(id: string) {
    switch (id) {
      case "archive":
        status.reset();
        setAsking("archive");
        break;
      case "keep":
        status.reset();
        setAsking("");
        break;
      case "archive-confirmed":
        void status.set(sender.id, "disabled").then((done) => done && setAsking(""));
        break;
      case "restore":
        void status.set(sender.id, "active");
        break;
      case "edit":
        update.reset();
        setDraft(draftOf(sender));
        setEditing(true);
        break;
      case "cancel":
        update.reset();
        setEditing(false);
        break;
      case "save":
        void update
          .update(sender.id, {
            address: sender.address,
            fromName: draft.fromName,
            replyTo: draft.replyTo,
            accountId: sender.accountId,
            notes: draft.notes,
          })
          .then((done) => done && setEditing(false));
        break;
    }
  }

  const state = editingNow ? "Editing" : archived ? WORDS.archived : WORDS.active;
  const detail = editingNow && !named ? "Add a display name" : undefined;

  return (
    <div
      className="os-action-pane"
      data-os-page-context={JSON.stringify({ page: "Sender", name: senderLabel(sender), id: sender.id })}
    >
      <div className="os-action-body os-app-stack">
        <Head navigationKey={`sender:${sender.id}`} title={senderLabel(sender)} back={{ label: "Settings", onSelect: onBack }} />

        {editingNow ? (
          <Panel label="Edit sender">
            <Field label="Display name">
              <Input
                id="email-sender-name"
                label="Display name"
                value={draft.fromName}
                onChange={(fromName) => setDraft({ ...draft, fromName })}
              />
            </Field>
            <Field label="Reply to">
              <Input
                id="email-sender-reply"
                label="Reply to"
                value={draft.replyTo}
                onChange={(replyTo) => setDraft({ ...draft, replyTo })}
              />
            </Field>
            <Field label="Notes">
              <TextArea
                id="email-sender-notes"
                label="Notes"
                rows={2}
                value={draft.notes}
                onChange={(notes) => setDraft({ ...draft, notes })}
              />
            </Field>
          </Panel>
        ) : (
          <Panel label="Sender details">
            <Facts>
              <Fact label="Organization" value={organization} />
              <Fact label="Display name" value={sender.fromName} />
              <Fact label="Reply to" value={sender.replyTo} mono />
              <Fact label="Notes" value={sender.notes} />
              <Fact label="Added" value={formatMoment(sender.createdAt)} />
            </Facts>
          </Panel>
        )}

        {status.error === "" ? null : (
          <Notice
            tone="error"
            sentence={archived ? "This sender wasn't restored." : "This sender wasn't archived."}
            next="Nothing changed."
            detail={status.error}
          />
        )}
        {update.error === "" ? null : (
          <Notice
            tone="error"
            sentence="Your changes weren't saved."
            next="What is above is still as you typed it."
            detail={update.error}
          />
        )}
      </div>

      <ActionBar
        state={state}
        detail={detail}
        tone={archived || editingNow ? "none" : "live"}
        acts={toKitActs(specs, run, busyId)}
      >
        {asking === "archive" && !editingNow && !archived ? (
          <span className="os-actbar-confirm">
            {`Archive ${senderLabel(sender)}? Campaigns and automations that send as it stop until it's restored.`}
          </span>
        ) : null}
      </ActionBar>
    </div>
  );
}
