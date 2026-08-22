import { useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";

import { Empty, ErrorMessage } from "../components/StatusMessage";
import { Band, Breadcrumbs, Button, ConfirmDialog, Container, Field, PageHeader, Select, Skeleton, TextInput } from "../ui";
import { ViewElement } from "../views/ViewElement";
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
// SENDING EXISTS NOW (memql#3348). "Start sending" calls the
// `campaignStartSend` builtin, which preflights -- is an email sender
// registered on this node, is one-click unsubscribe configured, is the
// template marked ready, is the audience non-empty and inside the ceiling --
// and refuses with the reason rather than partially sending. Those refusals
// arrive here as actionError, verbatim, because the engine's own wording is
// more useful than anything this page could paraphrase.
//
// SCHEDULING SENDS NOW TOO (memql#3459). The schedule button calls
// `campaignScheduleSend`, which runs the SAME preflight as Start and enqueues
// a send job the drain worker fires when the time arrives -- so the button no
// longer has to carry a disclaimer, which is the whole point of the change.
// It refuses a time already past, because that is far more often a typo in the
// year or the offset than a request to send immediately, and there is an
// unambiguous button for the latter.

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
    startSend,
    pauseSend,
    resumeSend,
  } = useCampaigns();
  const { descriptor } = useCampaignConcepts();

  const existing = useMemo(
    () => (creating ? undefined : campaigns.find((row) => row["id"] === routeId)),
    [campaigns, creating, routeId],
  );

  // Which irreversible action is awaiting confirmation. Starting a send
  // and cancelling a campaign are the two commitments on this page; both
  // go through ConfirmDialog so the consequence is stated before it lands.
  const [confirming, setConfirming] = useState<"start" | "cancel" | null>(null);
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
    return <Skeleton variant="kv" rows={6} />;
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
    <Container>
      <section className="flex min-h-full flex-col gap-6 pb-8">
        <PageHeader
          eyebrow={
            <Breadcrumbs
              items={[
                { label: "Campaigns", to: campaignsPath() },
                { label: creating ? "New campaign" : name || routeId },
              ]}
            />
          }
          title={creating ? "New campaign" : name || routeId}
          blurb={
            <>
              Status <span className="font-medium text-fg">{status}</span>.
            </>
          }
        />

        {actionError ? <ErrorMessage>{actionError}</ErrorMessage> : null}
        {actionMessage ? (
          <p role="status" className="rounded border border-line bg-raised px-3 py-2 text-sm text-fg">
            {actionMessage}
          </p>
        ) : null}

        {/* The frame is the shell's full width (ui/Container); the MEASURE is
            the form's own, because a text field stretched across a wide display
            is hard to read and a per-recipient table below it is not. Capping
            content rather than the page is what lets both be right. */}
        <form onSubmit={submit} className="flex max-w-3xl flex-col gap-3">
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
              <TextInput value={fromName} onChange={setFromName} placeholder="The MemQL team" />
            </Field>
            <Field label="Reply-to" grow hint="Empty sends replies to the configured sender address.">
              <TextInput value={replyTo} onChange={setReplyTo} placeholder="hello@example.com" />
            </Field>
          </div>

          <Field
            label="Send time"
            hint="RFC3339, e.g. 2026-09-01T09:00:00Z. A value with no offset is read as UTC. Schedule below to commit to it -- the send then starts on its own."
          >
            <TextInput
              value={scheduledAt}
              onChange={setScheduledAt}
              placeholder="2026-09-01T09:00:00Z"
            />
          </Field>

          <div className="flex flex-wrap items-center gap-2 pt-1">
            <Button type="submit" size="xs" busyLabel="Working…" busy={busy} disabled={incomplete}>
              {creating ? "Create draft" : "Save"}
            </Button>
            {creating ? null : (
              <>
                <Button size="xs"
                  onClick={() => scheduleCampaign({ campaignId: routeId, scheduledAt })}
                  disabled={busy || scheduledAt.trim() === ""}
                >
                  Schedule send
                </Button>
                {/* One control, three states. A single button whose label and
                    action follow the campaign's status, rather than three
                    buttons of which two are always wrong: the operator's
                    question is "what can I do to this campaign now", and a row
                    of disabled controls answers it worse than one enabled one.
                    Terminal states (sent / cancelled) offer nothing, which is
                    correct -- re-sending means authoring a new campaign, because
                    the delivery ledger is per (campaign, recipient) and a second
                    run would find every recipient terminal and mail nobody. */}
                {status === "sending" ? (
                  <Button size="xs" onClick={() => pauseSend(routeId)} disabled={busy}>
                    Pause sending
                  </Button>
                ) : status === "paused" ? (
                  <Button size="xs" onClick={() => resumeSend(routeId)} disabled={busy}>
                    Resume sending
                  </Button>
                ) : status === "sent" || status === "cancelled" ? null : (
                  <Button size="xs" onClick={() => setConfirming("start")} disabled={busy}>
                    Start sending
                  </Button>
                )}
                <Button size="xs" onClick={() => setConfirming("cancel")} disabled={busy} tone="danger">
                  Cancel campaign
                </Button>
              </>
            )}
          </div>
        </form>

        {creating ? null : (
          <Band title="Per-recipient outcomes" meta="one row per recipient, filled in as the send runs">
            {detail.error ? (
              <ErrorMessage>Could not read delivery records: {detail.error}</ErrorMessage>
            ) : detail.deliveries.length === 0 ? (
              <Empty>
                {status === "sending"
                  ? "No delivery records yet. The send works through the audience in batches, so the first rows appear within a minute or so."
                  : "No delivery records. This campaign has not been sent. A row lands here for every recipient once it does — including the ones that are skipped, so a suppression is an outcome rather than a silence."}
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

        <ConfirmDialog
          open={confirming === "start"}
          title="Start sending this campaign?"
          confirmLabel="Start sending"
          busy={busy}
          onConfirm={() => {
            setConfirming(null);
            startSend(routeId);
          }}
          onCancel={() => setConfirming(null)}
        >
          Sending mails every subscribed member of the audience, minus the cluster-wide
          suppression list, and cannot be re-run: the delivery ledger records one attempt per
          recipient. Pausing stops new sends; it does not recall anything already delivered.
        </ConfirmDialog>
        <ConfirmDialog
          open={confirming === "cancel"}
          title="Cancel this campaign?"
          confirmLabel="Cancel campaign"
          tone="danger"
          busy={busy}
          onConfirm={() => {
            setConfirming(null);
            cancelCampaign(routeId);
          }}
          onCancel={() => setConfirming(null)}
        >
          A cancelled campaign is terminal: it stops sending and cannot be resumed or re-sent.
          Deliveries already made stay delivered; re-sending means authoring a new campaign.
        </ConfirmDialog>

      </section>
    </Container>
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
    <Select value={value} onChange={onChange}>
      <option value="">Choose…</option>
      {options.map((option) => (
        <option key={option.value} value={option.value}>
          {option.label}
        </option>
      ))}
    </Select>
  );
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}
