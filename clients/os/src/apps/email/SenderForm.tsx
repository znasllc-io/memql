import { useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Field, Input, Notice, Panel } from "../../kit";
import { AccountPicker } from "../accounts/AccountPicker";
import { organizationChosen, useDefaultOrganization } from "../accounts/organization";
import { useAccountOptions } from "../accounts/tie";
import type { CampaignWrites } from "./actions";

// THE OLD CAMPAIGN JOURNEY'S "ADD A MAILBOX" FORM, kept only until that journey
// is replaced (Task 8 of the one Email app, memql#6033).
//
// A sender is added and edited in Settings now (`settings/AddSenderWizard.tsx`,
// `settings/SenderPage.tsx`), and the journey's inline form is the last place a
// sender is still added from a form. It is the Settings form it used to be, with
// the edit half gone and the write held under one id for as long as the form is
// open. The wizard that replaces the journey asks for "Set up sending" in
// Settings instead of embedding this, and this file goes with the journey.

export function SenderForm({
  initialAccountId = "",
  writes,
  onDone,
}: {
  initialAccountId?: string;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  // The add's id, held while the form is open: a press whose answer was lost is
  // tried again as the same sender.
  const [senderIdentityId] = useState(newShortId);
  const write = writes.createSender;
  const [draft, setDraft] = useState({ address: "", fromName: "", replyTo: "", accountId: "", notes: "" });

  const accountId = draft.accountId || initialAccountId || defaultAccountId;
  const ready = organizationChosen(accounts, accountId) && draft.address.trim() !== "" && draft.fromName.trim() !== "";

  async function submit() {
    if (!ready) return;
    if (await write.create(senderIdentityId, { ...draft, accountId })) onDone(senderIdentityId);
  }

  return (
    <Panel label="Add a mailbox">
      <div className="os-campaign-form">
        <Field label="Address">
          <Input
            id="os-sender-address"
            label="Mailbox address mail is sent from"
            value={draft.address}
            onChange={(v) => setDraft({ ...draft, address: v })}
            placeholder="news@acme.com"
          />
        </Field>
        <Field label="Shows as">
          <Input
            id="os-sender-fromname"
            label="Display name on the From line"
            value={draft.fromName}
            onChange={(v) => setDraft({ ...draft, fromName: v })}
            placeholder="Acme News"
          />
        </Field>
        <Field label="Replies go to">
          <Input
            id="os-sender-replyto"
            label="Reply-to address"
            value={draft.replyTo}
            onChange={(v) => setDraft({ ...draft, replyTo: v })}
          />
        </Field>
        <Field label="Organization">
          <AccountPicker
            id="os-sender-account"
            label="Organization this mailbox is for"
            required
            value={accountId}
            onChange={(v) => setDraft({ ...draft, accountId: v })}
            accounts={accounts}
          />
        </Field>
        <Field label="Notes">
          <Input
            id="os-sender-notes"
            label="Notes about this mailbox"
            value={draft.notes}
            onChange={(v) => setDraft({ ...draft, notes: v })}
          />
        </Field>
      </div>

      <Caption>
        Declaring a mailbox here says this cluster may send as it. It does not create the mailbox or
        grant access to it -- if your mail tenant has not been told to allow it, the first send
        comes back with the provider&apos;s own refusal on the campaign.
      </Caption>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence="This mailbox was not added."
          next="Nothing was written; what is above is still as you typed it."
          detail={write.error}
        />
      )}

      <div className="os-campaign-actions">
        <Button tone="primary" busy={write.busy} busyLabel="Saving" onClick={submit} disabled={!ready}>
          Add mailbox
        </Button>
        <Button
          onClick={() => {
            write.reset();
            onDone("");
          }}
        >
          Cancel
        </Button>
      </div>
      {ready ? null : (
        <Caption>
          An address and a display name are both needed -- they are what a recipient sees.
        </Caption>
      )}
    </Panel>
  );
}
