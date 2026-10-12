import { useEffect, useMemo, useState } from "react";
import { newShortId } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { editorTemplateURL } from "../../../items/editorPreference";
import { openHandoff, VSCODE_NO_ANSWER_MESSAGE } from "../../../items/vscode";
import {
  AutoRefresh,
  Button,
  Caption,
  Check,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  Notice,
  Panel,
  RecordRow,
  Select,
  Subhead,
  formatMoment,
} from "../../../kit";
import { ContentSkeleton, InlineSkeleton } from "../../../kit/ContentSkeleton";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { RecordList } from "../../../kit/RecordRow";
import { AccountChip, AccountPicker } from "../../accounts/AccountPicker";
import { organizationChosen, useDefaultOrganization } from "../../accounts/organization";
import { accountNameFrom } from "../../accounts/rows";
import { useAccountOptions } from "../../accounts/tie";
import type { CampaignWrites } from "../actions";
import { RecurrencePanel } from "../RecurrencePanel";
import {
  audienceProjection,
  campaignIsFinished,
  campaignIsRunning,
  campaignName,
  deliveryFromRow,
  formatFigure,
  labelOfSender,
  nameOfAudience,
  nameOfTemplate,
  senderProjection,
  skipReasonSentence,
  templateProjection,
  useProjected,
  type AudienceRow,
  type CampaignRow,
  type SenderIdentityRow,
  type TemplateRow,
} from "../rows";
import { SendBar } from "../SendBar";
import { TestSendPanel } from "../TestAudiencePanel";
import {
  EMAIL_REFRESH_MS,
  useCampaignDeliveries,
  useCampaignStats,
  useSendingReadiness,
  type EmailFeeds,
  type EmailRecordFeeds,
  type SendingReadiness,
} from "../useEmail";

// ONE CAMPAIGN'S PAGE, which replaces the list it was opened from (DESIGN.md
// rule 11): its results, its tests, its sending controls and, while it is a
// draft, its details.
//
// MOVED HERE UNCHANGED from the old campaigns section (memql#6033, Task 8) so
// the list and Add a campaign could be rebuilt around it; the next task
// rewrites it around one action bar. Delivery records and server-computed
// statistics stay fresh through an `AutoRefresh` beside each reading, without
// broadcasting recipient-volume events, and the series and testing settings are
// the ones the app root retains.

/** A campaign's page: its name over everything about it, and the way back to the list. */
export function CampaignPage({ campaign, feeds, recordFeeds, writes, onBack }: {
  campaign: CampaignRow;
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  writes: CampaignWrites;
  onBack: () => void;
}) {
  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const senders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  return (
    <div
      className="os-app-stack"
      data-os-page-context={JSON.stringify({
        page: "Campaign",
        name: campaignName(campaign),
        id: campaign.id,
      })}
    >
      <Head title={campaignName(campaign)} back={{ label: "Campaigns", onSelect: onBack }} />
      <CampaignDetail
        key={campaign.id}
        campaign={campaign}
        audiences={audiences}
        templates={templates}
        templateFeed={feeds.templates}
        recordFeeds={recordFeeds}
        senders={senders}
        writes={writes}
      />
    </div>
  );
}

// ---------------------------------------------------------------------------
// One campaign
// ---------------------------------------------------------------------------

function CampaignDetail({
  campaign,
  audiences,
  templates,
  templateFeed,
  recordFeeds,
  senders,
  writes,
}: {
  campaign: CampaignRow;
  audiences: AudienceRow[];
  templates: TemplateRow[];
  templateFeed: EmailFeeds["templates"];
  recordFeeds: EmailRecordFeeds;
  senders: SenderIdentityRow[];
  writes: CampaignWrites;
}) {
  const stats = useCampaignStats(campaign.id);
  const accounts = useAccountOptions();
  // ONE READ OF SENDING READINESS FOR THE WHOLE PAGE: the send controls and
  // the test panel both need the answer, and each asking was two identical
  // `campaignSendingReadiness` reads for one fact.
  const sending = useSendingReadiness(campaign.accountId, campaign.senderIdentityId, campaign.id);
  const [editing, setEditing] = useState(false);

  return (
    <div className="os-campaign-detail">
      <AutoRefresh onRefresh={stats.reload} busy={stats.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
      <Panel label={`${campaignName(campaign)} progress`}>
        <div className="os-campaign-detail-head">
          <Subhead>Campaign results</Subhead>
          <AccountChip name={accountNameFrom(accounts, campaign.accountId)} />
        </div>

        {/* THE BAR IS THE HEADLINE. It is fed by the campaign row's own
            counters, which arrive live, so it fills under somebody watching a
            send without this panel re-reading anything. */}
        <SendBar campaign={campaign} stats={stats.value} />
        <Caption>Results for this campaign’s audience. Test sends have their own results below.</Caption>

        {/* A SEND'S REFUSAL IS THE SERVER'S SENTENCE, verbatim and in place.
            "no email sender is registered on this node" names the thing to go
            and fix; a paraphrase would drop it. */}
        {campaign.lastError === "" ? null : (
          <Notice
            tone="error"
            sentence="The last attempt on this campaign ran into a problem."
            next={
              campaignIsRunning(campaign)
                ? "The send is still going; this is what it last reported."
                : "Nothing more will be sent until it is started again."
            }
            detail={campaign.lastError}
          />
        )}

        <Facts>
          <Fact label="Audience" value={nameOfAudience(audiences, campaign.audienceId)} />
          <Fact label="Template" value={nameOfTemplate(templates, campaign.templateId)} />
          <Fact
            label="Sends as"
            value={labelOfSender(senders, campaign.senderIdentityId)}
            mono
            title="Empty means this cluster's configured default mailbox."
          />
          <Fact label="Reply to" value={campaign.replyTo} mono />
          <Fact
            label="Scheduled"
            value={campaign.scheduledAt === "" ? "" : formatMoment(campaign.scheduledAt)}
          />
          <Fact
            label="Started"
            value={campaign.startedAt === "" ? "" : formatMoment(campaign.startedAt)}
          />
          <Fact
            label="Finished"
            value={campaign.completedAt === "" ? "" : formatMoment(campaign.completedAt)}
          />
        </Facts>
      </Panel>

      <TestSendPanel key={campaign.id} campaign={campaign} audiences={audiences}
        testSettings={recordFeeds.testSettings} sending={sending} />

      <SendControls campaign={campaign} writes={writes}
        template={templates.find(template => template.id === campaign.templateId)}
        templateFeed={templateFeed} sending={sending} />
      {/* Only a DRAFT sources a series, and only drafts are read for one. */}
      {campaign.status === "draft" ? <RecurrencePanel campaignId={campaign.id} series={recordFeeds.series} /> : null}

      <StatsPanel stats={stats} />

      <DeliveriesPanel campaignId={campaign.id} />

      {campaign.status !== "draft" ? null : (
        <Panel label="Edit this campaign">
          <div className="os-campaign-detail-head">
            <Subhead>Details</Subhead>
            <Button onClick={() => setEditing((v) => !v)}>{editing ? "Cancel" : "Edit"}</Button>
          </div>
          {editing ? (
            <CampaignForm
              campaign={campaign}
              audiences={audiences}
              templates={templates}
              senders={senders}
              trackByDefault={campaign.trackOpens}
              writes={writes}
              onDone={() => setEditing(false)}
            />
          ) : (
            <Caption>
              A draft can still be changed. Once a send starts, the audience and template it went
              out with stay on the record.
            </Caption>
          )}
        </Panel>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// The lifecycle controls
// ---------------------------------------------------------------------------

/**
 * Start, schedule, pause, resume, cancel -- and the confirm before the one
 * that cannot be taken back.
 *
 * THE CONFIRM IS IN SURFACE, never `window.confirm`: a modal the desktop did
 * not draw is the loudest tell that this is a tab, and a refusal inside a
 * dialog that then closes is a refusal nobody can re-read.
 *
 * IT NAMES THE AUDIENCE SIZE, because that is the fact somebody is checking
 * when they hesitate. "Send to 4,182 people?" is a different question from
 * "Send?" and it is the one being asked.
 */
function SendControls({ campaign, writes, template, templateFeed, sending }: {
  campaign: CampaignRow; writes: CampaignWrites; template?: TemplateRow;
  templateFeed: EmailFeeds["templates"]; sending: SendingReadiness;
}) {
  const { config } = useSession();
  const [editorNotice, setEditorNotice] = useState("");
  const templatesCurrent = templateFeed.snapshot.state === "live" && !templateFeed.snapshot.error;
  const ownTemplate = template?.accountId === campaign.accountId;
  const templateReady = templatesCurrent && ownTemplate && template?.status === "ready";
  const readyToSend = sending.ready;
  const readyToStart = readyToSend && templateReady;
  const controls = writes.sendControls;
  const [asking, setAsking] = useState<"" | "start" | "cancel">("");
  const [when, setWhen] = useState("");

  useEffect(() => {
    // A publication or a later edit must not reuse an earlier send confirmation.
    setAsking(held => held === "start" ? "" : held);
    controls.reset();
    // reset belongs to this campaign's write state; its wrapper changes each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [campaign.templateId, template?.createdAt, template?.status, templatesCurrent]);

  const draft = campaign.status === "draft";
  const scheduled = campaign.status === "scheduled";
  const running = campaignIsRunning(campaign);
  const paused = campaign.status === "paused";
  const finished = campaignIsFinished(campaign);

  if (finished) {
    return (
      <Panel label="This campaign is finished">
        <Subhead>Finished</Subhead>
        <Caption>
          This campaign is {campaign.status}. Running it again means writing a new one -- the record
          of what went out stays as it is.
        </Caption>
      </Panel>
    );
  }

  return (
    <Panel label="Send controls">
      <Subhead>Sending</Subhead>
      {sending.capture ? <Caption>Messages will appear in the Email app. External delivery is disabled.</Caption> : null}
      {!readyToSend ? (
        <Notice
          tone="warn"
          sentence="Sending setup is incomplete or unconfirmed."
          next="Review this organization’s email connection in Settings. You can still edit the draft or stop an existing send."
          detail={sending.reason}
        ><Button onClick={sending.reload} busy={sending.state === "loading"}>Check again</Button></Notice>
      ) : null}

      {(draft || scheduled) && readyToSend && !templateReady ? (
        !templatesCurrent ? templateFeed.snapshot.state === "seeding" ? (
          <InlineSkeleton label="Checking template publication" />
        ) : (
          <Notice tone="warn" sentence="Template status could not be confirmed." detail={templateFeed.snapshot.error}>
            <Button onClick={templateFeed.reseed}>Try again</Button>
          </Notice>
        ) : template && ownTemplate && template.status === "draft" ? (
          <Notice tone="info" sentence={`Publish “${template.name || "Untitled template"}” before sending.`}
            next="Review it in the editor, then choose Use in Campaigns → Publish template for campaigns.">
            <Button onClick={() => openHandoff(editorTemplateURL(config.domain, template.id, template.name), () => setEditorNotice(VSCODE_NO_ANSWER_MESSAGE))}>Review template</Button>
          </Notice>
        ) : (
          <Notice tone="warn" sentence="Choose a published template from this organization before sending."
            next={scheduled ? "Restore and publish the template in Templates, or cancel this schedule to change it." : "Select another template under Details → Edit."} />
        )
      ) : null}
      {editorNotice ? <Notice sentence={editorNotice} /> : null}

      {asking === "start" && readyToStart ? (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">
            Send {campaignName(campaign)} now
            {campaign.recipientCount > 0 ? ` to ${campaign.recipientCount} people` : ""}?
          </p>
          <Caption>
            {sending.capture
              ? "Messages are captured for review in the Email app. Anyone on the do-not-mail list is skipped."
              : "Mail starts leaving immediately. Anyone on the do-not-mail list is skipped and recorded as skipped; everybody else gets it. There is no unsend."}
          </Caption>
          <div className="os-campaign-actions">
            <Button
              tone="primary"
              busy={controls.busy}
              busyLabel="Starting"
              onClick={async () => {
                const ok = await controls.start(campaign.id);
                if (ok) setAsking("");
              }}
            >
              Send now
            </Button>
            <Button
              onClick={() => {
                setAsking("");
                controls.reset();
              }}
            >
              Not yet
            </Button>
          </div>
        </div>
      ) : asking === "cancel" ? (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">Cancel {campaignName(campaign)}?</p>
          <Caption>
            Whatever has already gone out stays sent, and the record of it stays readable. A
            cancelled campaign cannot be restarted -- running it again means writing a new one.
          </Caption>
          <div className="os-campaign-actions">
            <Button
              tone="danger"
              busy={controls.busy}
              busyLabel="Cancelling"
              onClick={async () => {
                const ok = await controls.cancel(campaign.id);
                if (ok) setAsking("");
              }}
            >
              Cancel this campaign
            </Button>
            <Button
              onClick={() => {
                setAsking("");
                controls.reset();
              }}
            >
              Keep it
            </Button>
          </div>
        </div>
      ) : (
        <div className="os-campaign-actions">
          {(draft || scheduled) && readyToStart ? (
            <Button tone="primary" onClick={() => setAsking("start")}>
              Send now
            </Button>
          ) : null}
          {/* A PAUSED SEND RESUMES WITHOUT A CONFIRM. The irreversible step
              was starting it; resuming picks up a run that is already
              underway, and asking again would train somebody to click
              through the question that matters. */}
          {paused && readyToSend ? (
            <Button
              tone="primary"
              busy={controls.busy}
              busyLabel="Resuming"
              onClick={() => controls.resume(campaign.id)}
            >
              Resume
            </Button>
          ) : null}
          {running ? (
            <Button
              busy={controls.busy}
              busyLabel="Pausing"
              onClick={() => controls.pause(campaign.id)}
            >
              Pause
            </Button>
          ) : null}
          <Button tone="danger" onClick={() => setAsking("cancel")}>
            Cancel
          </Button>
        </div>
      )}

      {(draft || scheduled) && readyToStart ? (
        <div className="os-campaign-schedule">
          <Field label="Or send it at">
            <Input
              id={`os-campaign-when-${campaign.id}`}
              label="When the send should begin"
              value={when === "" ? campaign.scheduledAt : when}
              onChange={setWhen}
              placeholder="2026-09-08T09:00:00Z"
            />
            <Button
              busy={controls.busy}
              busyLabel="Scheduling"
              onClick={() =>
                controls.schedule(campaign.id, when === "" ? campaign.scheduledAt : when)
              }
            >
              Schedule
            </Button>
          </Field>
          <Caption>
            A time with no offset is read as UTC. The same checks run now as they would at send
            time, so a missing sender or an unfinished template is caught here rather than at 3am.
          </Caption>
        </div>
      ) : null}

      {paused ? (
        <Caption>
          Paused. Resuming picks up exactly where it stopped -- everybody who has already been
          written to the ledger is skipped, so nobody is mailed twice.
        </Caption>
      ) : null}

      {controls.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The cluster refused that."
          next="Nothing changed. What it says below is the check that stopped it."
          detail={controls.error}
        />
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Test send
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// The full breakdown
// ---------------------------------------------------------------------------

/** Server-computed totals refresh automatically, including late engagement. */
function StatsPanel({ stats }: { stats: ReturnType<typeof useCampaignStats> }) {
  const value = stats.value;
  return (
    <Panel label="Full breakdown">
      <div className="os-campaign-detail-head">
        <Subhead>Breakdown</Subhead>
      </div>

      {stats.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return the breakdown."
          next="The bar above is from the campaign's own counters and is still current."
          detail={stats.error}
        />
      ) : value === null ? (stats.state === "loading" ? <ContentSkeleton kind="metrics" label="Reading the breakdown" /> : <Caption>{"No breakdown yet."}</Caption>) : (
        <>
          <Facts>
            <Fact label="Recipients" value={formatFigure(value.recipients)} />
            <Fact label="Sent" value={formatFigure(value.sent)} />
            <Fact label="Not yet sent" value={formatFigure(value.pending)} />
            <Fact label="Failed" value={formatFigure(value.failed)} />
            <Fact label="Skipped" value={formatFigure(value.skipped)} />
            <Fact
              label="Skipped: on the do-not-mail list"
              value={formatFigure(value.skippedSuppressed)}
            />
            <Fact label="Skipped: unsubscribed" value={formatFigure(value.skippedUnsubscribed)} />
            <Fact label="Skipped: some other reason" value={formatFigure(value.skippedOther)} />
            <Fact label="Hard bounces" value={formatFigure(value.hardBounces)} />
            <Fact label="Spam reports" value={formatFigure(value.complaints)} />
            <Fact label="Unsubscribed from this" value={formatFigure(value.unsubscribes)} />
          </Facts>
          {/* AN ABSENT FIGURE IS AN EM DASH WITH A REASON. Soft bounces are
              not measured per campaign at all, and saying nothing about that
              would let a reader take the missing row for a zero. */}
          <Caption>
            Soft bounces are not counted per campaign -- nothing measures them at that grain, so
            there is no figure to show rather than a zero.
          </Caption>
        </>
      )}

      <Caption>Updates automatically</Caption>
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The ledger
// ---------------------------------------------------------------------------

/** How many delivery rows are named before the panel stops listing them. The
 *  read is capped at 100 by the query; showing all of them turns a detail
 *  panel into a scroll region competing with the bar above it. */
const LEDGER_ROWS = 25;

function DeliveriesPanel({ campaignId }: { campaignId: string }) {
  const ledger = useCampaignDeliveries(campaignId);
  const rows = useMemo(() => ledger.value.map(deliveryFromRow), [ledger.value]);

  return (
    <Panel label="Who got it">
      <AutoRefresh onRefresh={ledger.reload} busy={ledger.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
      <div className="os-campaign-detail-head">
        <Subhead meta={ledger.state === "ready" && !ledger.error ? `${rows.length} read` : undefined}>Who got it</Subhead>
      </div>

      {ledger.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return the delivery record."
          next="Nothing is listed below -- that is silence, not an empty send."
          detail={ledger.error}
        />
      ) : rows.length === 0 ? (ledger.state === "loading" ? <RecordListSkeleton label="Reading the delivery record" /> : <Caption>{"Nothing has been written to the record yet."}</Caption>) : (
        <RecordList as="ul" label="Per-recipient outcomes">
          {rows.slice(0, LEDGER_ROWS).map((delivery) => (
            <RecordRow key={delivery.id}
              name={<span className="os-mono">{delivery.email || "--"}</span>}
              state={delivery.status || "unknown"}
              tone={delivery.status === "sent" ? "accent" : delivery.lastError ? "warn" : "muted"}
              secondary={delivery.lastError || (delivery.status === "skipped" ? skipReasonSentence(delivery.skipReason) : undefined)}
            >
              {delivery.sentAt === "" ? null : <span className="os-caption">{formatMoment(delivery.sentAt)}</span>}
            </RecordRow>
          ))}
          {rows.length > LEDGER_ROWS ? <p className="os-caption">and {rows.length - LEDGER_ROWS} more in this page</p> : null}
        </RecordList>
      )}

      <Caption>Latest 100 · Updates automatically</Caption>
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The form
// ---------------------------------------------------------------------------

/**
 * Write a campaign, or edit a draft.
 *
 * ONE FORM FOR BOTH, because they take the same fields and `updateCampaign`
 * accepts the same required three -- a separate edit form would be a second
 * place for the audience picker's archived-row behaviour to drift.
 *
 * AN AUDIENCE AND A TEMPLATE ARE REQUIRED, and the form says so by refusing
 * rather than by sending and being refused: both are `string!` on the concept.
 * Everything else is correctable later.
 */
export function CampaignForm({
  campaign,
  initial,
  audiences,
  templates,
  senders,
  trackByDefault,
  writes,
  onDone,
}: {
  campaign?: CampaignRow;
  initial?: {
    audienceId: string;
    templateId: string;
    senderIdentityId: string;
    accountId?: string;
  };
  audiences: AudienceRow[];
  templates: TemplateRow[];
  senders: SenderIdentityRow[];
  trackByDefault: boolean;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  // The id a new campaign is created under, held for as long as the form is
  // open: a create tried again after a refusal is the same campaign.
  const [createId] = useState(newShortId);
  const editing = campaign !== undefined;
  const write = editing ? writes.updateCampaign : writes.createCampaign;
  const [draft, setDraft] = useState(() => ({
    name: campaign?.name ?? "",
    audienceId: campaign?.audienceId ?? initial?.audienceId ?? "",
    templateId: campaign?.templateId ?? initial?.templateId ?? "",
    fromName: campaign?.fromName ?? "",
    replyTo: campaign?.replyTo ?? "",
    scheduledAt: campaign?.scheduledAt ?? "",
    accountId: campaign?.accountId ?? initial?.accountId ?? "",
    senderIdentityId: campaign?.senderIdentityId ?? initial?.senderIdentityId ?? "",
    trackOpens: campaign?.trackOpens ?? trackByDefault,
    trackClicks: campaign?.trackClicks ?? trackByDefault,
  }));

  useEffect(() => {
    if (!initial || campaign) return;
    setDraft((previous) => ({ ...previous, ...initial }));
  }, [initial?.audienceId, initial?.templateId, initial?.senderIdentityId, initial?.accountId, campaign]);

  const accountId = draft.accountId || (campaign === undefined ? defaultAccountId : "");

  const ready = organizationChosen(accounts, accountId) &&
    draft.name.trim() !== "" &&
    audiences.some((a) => a.id === draft.audienceId && a.accountId === accountId) &&
    templates.some((t) => t.id === draft.templateId && t.accountId === accountId) &&
    ((accountId === "self" && draft.senderIdentityId === "") ||
      senders.some((s) => s.id === draft.senderIdentityId && s.accountId === accountId));

  async function submit() {
    if (!ready) return;
    if (editing && campaign) {
      const ok = await writes.updateCampaign.update(campaign.id, { ...draft, accountId });
      if (ok) onDone(campaign.id);
      return;
    }
    if (await writes.createCampaign.create({ ...draft, accountId }, createId)) onDone(createId);
  }

  // Only the audiences and templates somebody could sensibly send: archived
  // ones are kept for their history and are not offered here. A campaign
  // ALREADY naming one keeps it (the value is on the draft and the option is
  // synthesized below), so an edit never silently re-points a campaign.
  const pickable = {
    audiences: audiences.filter((a) => a.accountId === accountId && (a.status !== "archived" || a.id === draft.audienceId)),
    templates: templates.filter((t) => t.accountId === accountId && (t.status !== "archived" || t.id === draft.templateId)),
    senders: senders.filter((s) => s.accountId === accountId && (s.status !== "disabled" || s.id === draft.senderIdentityId)),
  };

  return (
    <Panel label={editing ? "Edit campaign" : "New campaign"}>
      <Field label="Organization">
          <AccountPicker
            id="os-campaign-account"
            label="Organization this campaign is for"
            required
            value={accountId}
            onChange={(v) => setDraft({ ...draft, accountId: v, audienceId: "", templateId: "", senderIdentityId: "" })}
            accounts={accounts}
          />
        </Field>
      <div className="os-campaign-form">
        <Field label="Name">
          <Input
            id="os-campaign-name"
            label="Campaign name"
            value={draft.name}
            onChange={(v) => setDraft({ ...draft, name: v })}
            placeholder="August product update"
          />
        </Field>
        <Field label="Audience">
          <Select
            id="os-campaign-audience"
            label="Audience to send to"
            value={draft.audienceId}
            onChange={(v) => setDraft({ ...draft, audienceId: v })}
          >
            <option value="">Choose an audience</option>
            {pickable.audiences.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name || a.id}
                {a.status === "archived" ? " (archived)" : ""}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Template">
          <Select
            id="os-campaign-template"
            label="Template to send"
            value={draft.templateId}
            onChange={(v) => setDraft({ ...draft, templateId: v })}
          >
            <option value="">Choose a template</option>
            {pickable.templates.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name || t.id}
                {t.status === "ready" ? "" : ` (${t.status || "draft"})`}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Sends as">
          <Select
            id="os-campaign-sender"
            label="Sending mailbox"
            value={draft.senderIdentityId}
            onChange={(v) => setDraft({ ...draft, senderIdentityId: v })}
          >
            <option value="" disabled={accountId !== "self"}>{accountId === "self" ? "This cluster's default mailbox" : "Choose an organization mailbox"}</option>
            {pickable.senders.map((s) => (
              <option key={s.id} value={s.id}>
                {s.address}
                {s.status === "disabled" ? " (retired)" : ""}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="Reply to">
          <Input
            id="os-campaign-replyto"
            label="Reply-to address"
            value={draft.replyTo}
            onChange={(v) => setDraft({ ...draft, replyTo: v })}
          />
        </Field>

      </div>

      <div className="os-campaign-tracking">
        <Check checked={draft.trackOpens} onChange={(v) => setDraft({ ...draft, trackOpens: v })}>
          Count who opens it
        </Check>
        <Check checked={draft.trackClicks} onChange={(v) => setDraft({ ...draft, trackClicks: v })}>
          Count who clicks a link
        </Check>
        <Caption>
          Tracking adds an invisible image and rewrites links so they pass through this cluster.
          Turn it off and the breakdown will say so rather than reporting zero.
        </Caption>
      </div>

      {write.error === "" ? null : (
        <Notice
          tone="error"
          sentence={editing ? "This did not save." : "This campaign was not created."}
          next="Nothing was written; what is above is still as you typed it."
          detail={write.error}
        />
      )}

      <div className="os-campaign-actions">
        <Button
          tone="primary"
          busy={write.busy}
          busyLabel="Saving"
          onClick={submit}
          disabled={!ready}
        >
          {editing ? "Save" : "Create campaign"}
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
        <Caption>A name, an audience and a template are needed before this can be created.</Caption>
      )}
    </Panel>
  );
}
