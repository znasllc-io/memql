import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { Empty, ErrorMessage, Loading } from "../components/StatusMessage";
import { Band, MetaButton } from "../views/ViewLayout";
import { ViewElement } from "../views/ViewElement";
import { Field, SubmitButton, TextInput } from "./CampaignsPage";
import { useCampaignDetail, useCampaigns } from "./useCampaigns";
import {
  DELIVERY_CONCEPT_ID,
  fallbackConcept,
  selectOptions,
  useCampaignConcepts,
} from "./concepts";
import { NEW_CAMPAIGN_SLUG, campaignsPath } from "./urls";

// One campaign, authored (memql#3323).
//
// The same route serves create and edit -- `/integrations/campaigns/new` and
// `/integrations/campaigns/:campaignId` -- because they are one screen. Two
// components would be two implementations of the same form, and the second one
// would be the one that forgot a field.
//
// THE SCHEDULE BUTTON DOES NOT SEND. It writes `status: "scheduled"` and a
// time onto the row, and nothing reads that yet. The button says so, the
// confirmation message says so, and the deliveries band says so. That is a
// deliberate stopping point rather than an unfinished one: campaign sending
// touches deliverability, unsubscribe handling and rate limits, all of which
// are product decisions rather than UI, and shipping a sender that skipped
// them would be worse than shipping none.

export function CampaignEditorPage(): ReactNode {
  const params = useParams();
  const routeId = params["campaignId"] ?? NEW_CAMPAIGN_SLUG;
  const creating = routeId === NEW_CAMPAIGN_SLUG;

  const {
    campaigns,
    audiences,
    templates,
    loading,
    error,
    actionMessage,
    actionError,
    busy,
    saveCampaign,
    scheduleCampaign,
    cancelCampaign,
  } = useCampaigns();
  const { descriptor } = useCampaignConcepts();

  const existing = useMemo(
    () => (creating ? undefined : campaigns.find((row) => row["id"] === routeId)),
    [campaigns, creating, routeId],
  );

  const [name, setName] = useState("");
  const [audienceId, setAudienceId] = useState("");
  const [templateId, setTemplateId] = useState("");
  const [fromName, setFromName] = useState("");
  const [replyTo, setReplyTo] = useState("");
  const [scheduledAt, setScheduledAt] = useState("");

  // Seed the form from the row once it arrives. Keyed on the ROW, not on the
  // route: the row lands after the first render, and a form seeded only on
  // mount would stay blank over a campaign that loaded a tick later.
  useEffect(() => {
    if (!existing) return;
    setName(text(existing["name"]));
    setAudienceId(text(existing["audienceId"]));
    setTemplateId(text(existing["templateId"]));
    setFromName(text(existing["fromName"]));
    setReplyTo(text(existing["replyTo"]));
    setScheduledAt(text(existing["scheduledAt"]));
  }, [existing]);

  const audienceOptions = useMemo(() => selectOptions(audiences, "name"), [audiences]);
  const templateOptions = useMemo(() => selectOptions(templates, "name"), [templates]);

  const detail = useCampaignDetail(creating ? "" : routeId, audienceId);
  const deliveryConcept =
    descriptor(DELIVERY_CONCEPT_ID) ?? fallbackConcept(DELIVERY_CONCEPT_ID, "delivery");

  const status = creating ? "draft" : text(existing?.["status"]) || "draft";
  const incomplete = name.trim() === "" || audienceId === "" || templateId === "";

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (incomplete) return;
    saveCampaign({
      campaignId: creating ? "" : routeId,
      name: name.trim(),
      audienceId,
      templateId,
      fromName: fromName.trim(),
      replyTo: replyTo.trim(),
      scheduledAt,
    });
  }

  if (error) {
    return <ErrorMessage>Could not read campaigns: {error}</ErrorMessage>;
  }
  if (!creating && !existing && loading) {
    return <Loading what="the campaign" />;
  }
  if (!creating && !existing) {
    return (
      <Empty>
        No campaign of yours has that id. It may have been cancelled, or the link may name a
        campaign belonging to another operator — the cluster only ever returns your own.{" "}
        <Link to={campaignsPath()} className="text-accent underline">
          Back to campaigns
        </Link>
      </Empty>
    );
  }

  return (
    <section className="mx-auto flex min-h-full max-w-3xl flex-col gap-6 pb-8">
      <header className="border-b border-line pb-4">
        <Link to={campaignsPath()} className="text-xs text-muted hover:text-fg hover:underline">
          ← Campaigns
        </Link>
        <h1 className="mt-1 text-xl font-semibold tracking-tight">
          {creating ? "New campaign" : name || routeId}
        </h1>
        <p className="mt-1 text-sm text-muted">
          Status <span className="font-medium text-fg">{status}</span>. Authoring only — nothing
          on this page sends mail.
        </p>
      </header>

      {actionError ? <ErrorMessage>{actionError}</ErrorMessage> : null}
      {actionMessage ? (
        <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
          {actionMessage}
        </p>
      ) : null}

      <form onSubmit={submit} className="flex flex-col gap-3">
        <Field label="Campaign name" grow>
          <TextInput value={name} onChange={setName} placeholder="August product update" />
        </Field>

        <div className="flex flex-wrap gap-3">
          <Field
            label="Audience"
            grow
            hint={
              audienceId === ""
                ? "Pick who this goes to."
                : `${detail.sendableCount} subscribed of this audience would be mailed. Unsubscribed, bounced and complained addresses are suppressed.`
            }
          >
            <Picker value={audienceId} onChange={setAudienceId} options={audienceOptions} />
          </Field>
          <Field label="Template" grow hint="Only the subject and bodies come from the template.">
            <Picker value={templateId} onChange={setTemplateId} options={templateOptions} />
          </Field>
        </div>

        <div className="flex flex-wrap gap-3">
          <Field
            label="From name"
            grow
            hint="Overrides the integration-wide default for this campaign. The from ADDRESS is bound to the configured mailbox and is not settable here."
          >
            <TextInput value={fromName} onChange={setFromName} placeholder="The memQL team" />
          </Field>
          <Field label="Reply-to" grow hint="Empty sends replies to the configured sender address.">
            <TextInput value={replyTo} onChange={setReplyTo} placeholder="hello@example.com" />
          </Field>
        </div>

        <Field
          label="Intended send time"
          hint="RFC3339, e.g. 2026-09-01T09:00:00Z. Recorded on the row; nothing acts on it yet."
        >
          <TextInput
            value={scheduledAt}
            onChange={setScheduledAt}
            placeholder="2026-09-01T09:00:00Z"
          />
        </Field>

        <div className="flex flex-wrap items-center gap-2 pt-1">
          <SubmitButton busy={busy} disabled={incomplete}>
            {creating ? "Create draft" : "Save"}
          </SubmitButton>
          {creating ? null : (
            <>
              <MetaButton
                onClick={() => scheduleCampaign({ campaignId: routeId, scheduledAt })}
                disabled={busy || scheduledAt.trim() === ""}
              >
                Record schedule (does not send)
              </MetaButton>
              <MetaButton onClick={() => cancelCampaign(routeId)} disabled={busy} tone="danger">
                Cancel campaign
              </MetaButton>
            </>
          )}
        </div>
      </form>

      {creating ? null : (
        <Band title="Per-recipient outcomes" meta="one row per recipient, once a send has run">
          {detail.error ? (
            <ErrorMessage>Could not read delivery records: {detail.error}</ErrorMessage>
          ) : detail.deliveries.length === 0 ? (
            <Empty>
              No delivery records. Nothing has been sent, and nothing can be yet: the sending
              engine is a separate piece of work. The schema is in place so that work adds rows
              here rather than a migration.
            </Empty>
          ) : (
            <div className="overflow-x-auto rounded-lg border border-line bg-surface p-1">
              <ViewElement
                element={TABLE_ELEMENT}
                rows={detail.deliveries}
                concept={deliveryConcept}
                options={{ sort: { field: "createdAt", direction: "desc" } }}
              />
            </div>
          )}
        </Band>
      )}
    </section>
  );
}

function Picker({
  value,
  onChange,
  options,
}: {
  value: string;
  onChange: (next: string) => void;
  options: readonly { value: string; label: string }[];
}): ReactNode {
  return (
    <select
      value={value}
      onChange={(event) => onChange(event.target.value)}
      className="w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm text-fg"
    >
      <option value="">Choose…</option>
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </select>
  );
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}
