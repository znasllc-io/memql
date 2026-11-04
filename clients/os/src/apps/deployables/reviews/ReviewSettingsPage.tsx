import { useEffect, useState } from "react";

import { Button, Caption, Field, Head, Input, Notice, Panel, RecordListSkeleton, Switch } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import type { Breadcrumb } from "../../../kit/Breadcrumbs";
import type { Reading } from "../../../cluster/reading";
import { deployedByLabel, type NameOf } from "../people";
import type { ReviewSettings, ReviewSettingsValues } from "./model";
import type { ReviewWrites } from "./useReviews";

// A STORE'S REVIEW SETTINGS, edited in place with the store's part
// (packs/reviewspack/settings.go). What is in force is the engine's answer,
// defaults applied; a change is sent as only what changed, and the engine
// merges it into the store's one settings row.
//
// WHO MAY CHANGE WHAT IS THE ENGINE'S TOO (reviewsSettings' `may`): a store
// with no settings is set up by the cluster's operators, and only the store's
// current owner, or an operator, names who approves removals. A field this
// person may not change is shown, not offered.

type Draft = {
  publicDisplay: boolean;
  requestsEnabled: boolean;
  requestDelayDays: string;
  linkLifetimeDays: string;
  requestTemplateId: string;
  requestSenderIdentityId: string;
  notifyEmail: string;
  removalApproverUserId: string;
};

const draftOf = (v: ReviewSettingsValues): Draft => ({
  publicDisplay: v.publicDisplay,
  requestsEnabled: v.requestsEnabled,
  requestDelayDays: String(v.requestDelayDays),
  linkLifetimeDays: String(v.linkLifetimeDays),
  requestTemplateId: v.requestTemplateId,
  requestSenderIdentityId: v.requestSenderIdentityId,
  notifyEmail: v.notifyEmail,
  removalApproverUserId: v.removalApproverUserId,
});

/** A whole number of days within bounds, or null. */
function days(text: string, min: number, max: number): number | null {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return null;
  const n = Number(t);
  return n >= min && n <= max ? n : null;
}

/** Only what changed, typed as the engine takes it; or the first thing wrong with it. */
export function settingsChanges(from: ReviewSettingsValues, draft: Draft, may: { nameApprover: boolean; nameSender: boolean }): { changes: Partial<ReviewSettingsValues>; problem: string } {
  const changes: Partial<ReviewSettingsValues> = {};
  if (draft.publicDisplay !== from.publicDisplay) changes.publicDisplay = draft.publicDisplay;
  if (draft.requestsEnabled !== from.requestsEnabled) changes.requestsEnabled = draft.requestsEnabled;
  const delay = days(draft.requestDelayDays, 0, 365);
  if (delay === null) return { changes, problem: "The days before asking are a whole number from 0 to 365." };
  if (delay !== from.requestDelayDays) changes.requestDelayDays = delay;
  const life = days(draft.linkLifetimeDays, 1, 365);
  if (life === null) return { changes, problem: "The days a link stays open are a whole number from 1 to 365." };
  if (life !== from.linkLifetimeDays) changes.linkLifetimeDays = life;
  const template = draft.requestTemplateId.trim();
  if (template !== from.requestTemplateId) changes.requestTemplateId = template;
  const sender = draft.requestSenderIdentityId.trim();
  if (may.nameSender && sender !== from.requestSenderIdentityId) changes.requestSenderIdentityId = sender;
  const email = draft.notifyEmail.trim();
  if (email !== "" && !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) return { changes, problem: `${email} is not an email address.` };
  if (email !== from.notifyEmail) changes.notifyEmail = email;
  const approver = draft.removalApproverUserId.trim();
  if (may.nameApprover && approver !== from.removalApproverUserId) changes.removalApproverUserId = approver;
  return { changes, problem: "" };
}

export function ReviewSettingsPage({ settings, writes, nameOf, viewerUserId, trail, back }: {
  settings: Reading<ReviewSettings>;
  writes: ReviewWrites;
  nameOf: NameOf;
  viewerUserId: string;
  trail: readonly Breadcrumb[];
  back: { label: string; onSelect: () => void };
}) {
  const value = settings.value;
  const [draft, setDraft] = useState<Draft | null>(null);
  // A fresh answer from the engine is the form's new starting point.
  useEffect(() => {
    if (value !== null) setDraft(draftOf(value.values));
  }, [value]);

  const head = <Head navigationKey="review-settings" title="Settings" breadcrumbs={[...trail, { label: "Settings" }]} back={back} />;
  if (value === null || draft === null) {
    return (
      <section className="os-action-pane">
        <div className="os-action-body os-app-stack">
          {head}
          {settings.state === "failed" ? (
            <Notice tone="error" sentence={settings.error || "The settings could not be read."}><Button onClick={settings.reread}>Try again</Button></Notice>
          ) : (
            <RecordListSkeleton label="Reading the settings" rows={3} />
          )}
        </div>
        <ActionBar state="" tone="busy" />
      </section>
    );
  }

  const canEdit = value.may.edit;
  const { changes, problem } = settingsChanges(value.values, draft, value.may);
  const dirty = Object.keys(changes).length > 0 || problem !== "";
  const saving = writes.busy === "settings";
  const set = <K extends keyof Draft>(key: K) => (next: Draft[K]) => setDraft((d) => (d === null ? d : { ...d, [key]: next }));
  const approverName = draft.removalApproverUserId.trim() === "" ? "" : deployedByLabel(draft.removalApproverUserId.trim(), viewerUserId, nameOf);

  async function save() {
    if (problem !== "" || Object.keys(changes).length === 0) return;
    await writes.saveSettings(changes);
  }

  const acts: Act[] = canEdit && dirty
    ? [
        { label: "Discard", text: true, onAct: () => setDraft(draftOf(value.values)) },
        ...(problem === "" ? [{ label: "Save", tone: "primary" as const, busy: saving, onAct: () => void save() }] : []),
      ]
    : [];

  return (
    <section className="os-action-pane os-review-settings" data-os-page-context={JSON.stringify({ page: "Review settings" })}>
      <div className="os-action-body os-app-stack">
        {head}
        {!value.exists ? (
          <Notice sentence="This store's reviews are not set up yet." next={canEdit ? "Saving these settings sets them up." : "The cluster's operators set them up; after that, the store's people can change them here."} />
        ) : null}
        {writes.refusal ? <Notice tone="error" sentence={writes.refusal} /> : null}
        {problem !== "" ? <Notice tone="warn" sentence={problem} /> : null}
        <Panel label="On the site">
          <Switch checked={draft.publicDisplay} disabled={!canEdit} onChange={set("publicDisplay")}>Show published reviews on the site</Switch>
          <Caption>Only reviews somebody published are ever shown. Off, the site shows none.</Caption>
        </Panel>
        <Panel label="Asking buyers">
          <Switch checked={draft.requestsEnabled} disabled={!canEdit} onChange={set("requestsEnabled")}>Ask every buyer for a review</Switch>
          <Field label="Days after an order before asking">
            <Input id="review-request-delay" label="Days after an order before asking" value={draft.requestDelayDays} disabled={!canEdit} onChange={set("requestDelayDays")} />
          </Field>
          <Field label="Days a review link stays open">
            <Input id="review-link-lifetime" label="Days a review link stays open" value={draft.linkLifetimeDays} disabled={!canEdit} onChange={set("linkLifetimeDays")} />
          </Field>
          <Field label="Request email template">
            <Input id="review-request-template" label="Request email template" code value={draft.requestTemplateId} disabled={!canEdit} onChange={set("requestTemplateId")} placeholder="A Campaigns template id" />
          </Field>
          <Field label="Request sender">
            <Input id="review-request-sender" label="Request sender" code value={draft.requestSenderIdentityId} disabled={!canEdit || !value.may.nameSender} onChange={set("requestSenderIdentityId")} placeholder="A Campaigns sender identity id" />
          </Field>
          <Caption>Each paid product of a paid order gets its own link, sent once. A link stays open until its review is published or it expires, and the buyer can change their review through it until then.{value.may.nameSender ? "" : " Only the cluster's operators choose the mailbox requests leave from."}</Caption>
        </Panel>
        <Panel label="Moderation">
          <Field label="Email when a review is waiting">
            <Input id="review-notify-email" label="Email when a review is waiting" value={draft.notifyEmail} disabled={!canEdit} onChange={set("notifyEmail")} placeholder="moderators@example.com" />
          </Field>
          <Field label="Owner who approves removals">
            <Input id="review-removal-approver" label="Owner who approves removals" code value={draft.removalApproverUserId} disabled={!canEdit || !value.may.nameApprover} onChange={set("removalApproverUserId")} placeholder="A person's user id" />
          </Field>
          <Caption>
            {approverName !== "" ? `${approverName === "you" ? "You are" : `${approverName} is`} the one person who can remove a published review or erase a writer's data. ` : "Nobody can remove a published review until an owner is named. "}
            {value.may.nameApprover ? "" : "Only that owner, or the cluster's operators, can name someone else."}
          </Caption>
        </Panel>
      </div>
      <ActionBar state={saving ? "Saving" : dirty ? "Unsaved changes" : value.exists ? "Saved" : "Not set up"} tone={saving ? "busy" : dirty ? "paused" : "none"} acts={acts} />
    </section>
  );
}
