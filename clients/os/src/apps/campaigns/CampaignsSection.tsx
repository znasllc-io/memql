import { useEffect, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { Plus, Send } from "lucide-react";

import { AccountChip, AccountPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { accountNameFrom } from "../accounts/rows";
import {
  Button,
  Caption,
  Check,
  Chip,
  Fact,
  Facts,
  Field,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Row as ListRow,
  Select,
  Subhead,
  formatMoment,
} from "../../kit";
import { useLiveView } from "../../live/liveView";
import type { CampaignWrites, TestSendState } from "./actions";
import { SendBar } from "./SendBar";
import {
  audienceFromRow,
  campaignFingerprint,
  campaignFromRow,
  campaignIsFinished,
  campaignIsRunning,
  campaignName,
  deliveryFromRow,
  formatFigure,
  senderIdentityFromRow,
  skipReasonSentence,
  templateFromRow,
  type AudienceRow,
  type CampaignRow,
  type SenderIdentityRow,
  type TemplateRow,
} from "./rows";
import { useCampaignDeliveries, useCampaignStats, type CampaignFeeds } from "./useCampaigns";

// The campaigns list and one campaign in full.
//
// The list is LIVE (`v1:campaigns:campaign` broadcasts both verbs). The
// detail's two heavy readings -- the delivery ledger and the server-computed
// stats -- are NOT, deliberately, and they say when they were read. See
// useCampaigns.ts for the volume argument.

export function CampaignsSection({
  feeds,
  writes,
  showFiled,
  trackByDefault,
  onToggleFiled,
}: {
  feeds: CampaignFeeds;
  writes: CampaignWrites;
  showFiled: boolean;
  trackByDefault: boolean;
  onToggleFiled: (next: boolean) => void;
}) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);

  // PROJECT, then narrow, in one pass -- the collection holds RAW wire rows
  // (the fold upserts an event payload with no projection hook), so every
  // predicate below runs on a campaignFromRow result.
  const source = useLiveView<Row, CampaignRow>(
    feeds.campaigns.source,
    `filed:${showFiled}`,
    (rows) => {
      const campaigns = rows.map(campaignFromRow).filter((c) => c.id !== "");
      return showFiled ? campaigns : campaigns.filter((c) => !campaignIsFinished(c));
    },
  );

  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const senders = useProjected(feeds.senders.snapshot.rows, senderProjection);

  const open = useMemo(
    () => source?.snapshot.rows.find((c) => c.id === openId) ?? null,
    [source, source?.snapshot, openId],
  );

  return (
    <div className="os-app-stack">
      <Head title="Campaigns">
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus size={14} aria-hidden /> New campaign
        </Button>
      </Head>

      {feeds.campaigns.snapshot.error ? (
        <Notice
          tone="error"
          sentence="This cluster did not return your campaigns."
          next="Nothing below is current."
        >
          <Button onClick={feeds.campaigns.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {adding ? (
        <CampaignForm
          audiences={audiences}
          templates={templates}
          senders={senders}
          trackByDefault={trackByDefault}
          writes={writes}
          onDone={(id) => {
            setAdding(false);
            if (id !== "") setOpenId(id);
          }}
        />
      ) : null}

      <div className="os-campaign-filters">
        <Check checked={showFiled} onChange={onToggleFiled}>
          Show finished campaigns
        </Check>
      </div>

      {/* Keyed on the filter so flipping it RE-BASELINES the arrival cues.
          Revealing rows the browser already had is not the cluster sending
          them. */}
      <LiveList<CampaignRow>
        key={`campaigns:${showFiled}`}
        source={source}
        rowId={(c) => c.id}
        // THE COUNTERS ARE NOT NAMED HERE. See campaignFingerprint: the drain
        // worker moves sentCount / failedCount / skippedCount on every batch,
        // so ringing on one would strobe this whole list for the duration of
        // every send in the cluster. The bar below still fills live.
        fingerprint={campaignFingerprint}
        label="Your campaigns"
        emptyText={
          showFiled
            ? "No campaigns yet. Write one above -- you will need an audience and a template first."
            : "Nothing in flight. Start a campaign above, or show finished ones to see what has already gone out."
        }
        renderRow={(campaign, tick) => (
          <CampaignLine
            campaign={campaign}
            audiences={audiences}
            tick={tick}
            open={openId === campaign.id}
            onToggle={() => setOpenId((held) => (held === campaign.id ? "" : campaign.id))}
          />
        )}
      />

      {open === null ? null : (
        <CampaignDetail
          key={open.id}
          campaign={open}
          audiences={audiences}
          templates={templates}
          senders={senders}
          writes={writes}
        />
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Projections shared with the other sections' pickers
// ---------------------------------------------------------------------------

export const campaignProjection = (rows: readonly Row[]): CampaignRow[] =>
  rows.map((r) => campaignFromRow(r)).filter((c) => c.id !== "");
export const audienceProjection = (rows: readonly Row[]): AudienceRow[] =>
  rows.map((r) => audienceFromRow(r)).filter((a) => a.id !== "");
export const templateProjection = (rows: readonly Row[]): TemplateRow[] =>
  rows.map((r) => templateFromRow(r)).filter((t) => t.id !== "");
export const senderProjection = (rows: readonly Row[]): SenderIdentityRow[] =>
  rows.map((r) => senderIdentityFromRow(r)).filter((s) => s.id !== "");

/** Memoise a projection on the SNAPSHOT ARRAY's identity, which the collection
 *  already holds stable until something actually changes. */
export function useProjected<T>(rows: readonly Row[], project: (rows: readonly Row[]) => T[]): T[] {
  // eslint-disable-next-line react-hooks/exhaustive-deps
  return useMemo(() => project(rows), [rows]);
}

export function nameOfAudience(audiences: AudienceRow[], id: string): string {
  return audiences.find((a) => a.id === id)?.name ?? "";
}
export function nameOfTemplate(templates: TemplateRow[], id: string): string {
  return templates.find((t) => t.id === id)?.name ?? "";
}
export function labelOfSender(senders: SenderIdentityRow[], id: string): string {
  return senders.find((s) => s.id === id)?.address ?? "";
}

// ---------------------------------------------------------------------------
// The list line
// ---------------------------------------------------------------------------

/** The tone a status reads in. Only two states are worth colouring: one that
 *  is happening and one that went wrong. Everything else is a plain chip --
 *  seven coloured statuses is a list with no emphasis at all. */
function statusTone(status: string): "neutral" | "accent" | "muted" {
  if (status === "sending") return "accent";
  if (status === "draft") return "muted";
  return "neutral";
}

function CampaignLine({
  campaign,
  audiences,
  tick,
  open,
  onToggle,
}: {
  campaign: CampaignRow;
  audiences: AudienceRow[];
  tick: "added" | "updated" | null;
  open: boolean;
  onToggle: () => void;
}) {
  const audience = nameOfAudience(audiences, campaign.audienceId);
  return (
    <ListRow
      icon={<Send size={16} aria-hidden />}
      name={campaignName(campaign)}
      // `current` is the row's own liveness, and for a campaign that is
      // whether it is still work: a finished send keeps its facts and loses
      // its ink.
      current={!campaignIsFinished(campaign)}
      dim={campaignIsFinished(campaign)}
      open={open}
      onOpen={onToggle}
      state={
        <>
          <Chip tone={statusTone(campaign.status)}>{campaign.status || "unknown"}</Chip>
          {tick === "added" ? <span className="os-livelist-tick">new</span> : null}
        </>
      }
    >
      {audience === "" ? null : <span className="os-caption">{audience}</span>}
      {campaign.scheduledAt === "" || campaign.status !== "scheduled" ? null : (
        <span className="os-caption">{formatMoment(campaign.scheduledAt)}</span>
      )}
      {/* THE COUNTERS RENDER, and they move live. What they do not do is ring
          -- the fingerprint above is what separates re-rendering from
          announcing. */}
      {campaign.recipientCount === 0 ? null : (
        <span className="os-caption os-mono">
          {campaign.sentCount}/{campaign.recipientCount}
        </span>
      )}
      {campaign.lastError === "" ? null : <Chip tone="neutral">problem</Chip>}
    </ListRow>
  );
}

// ---------------------------------------------------------------------------
// One campaign
// ---------------------------------------------------------------------------

function CampaignDetail({
  campaign,
  audiences,
  templates,
  senders,
  writes,
}: {
  campaign: CampaignRow;
  audiences: AudienceRow[];
  templates: TemplateRow[];
  senders: SenderIdentityRow[];
  writes: CampaignWrites;
}) {
  const stats = useCampaignStats(campaign.id);
  const accounts = useAccountOptions();
  const [editing, setEditing] = useState(false);

  return (
    <div className="os-campaign-detail">
      <Panel label={`${campaignName(campaign)} progress`}>
        <div className="os-campaign-detail-head">
          <Subhead>{campaignName(campaign)}</Subhead>
          <AccountChip name={accountNameFrom(accounts, campaign.accountId)} />
        </div>

        {/* THE BAR IS THE HEADLINE. It is fed by the campaign row's own
            counters, which arrive live, so it fills under somebody watching a
            send without this panel re-reading anything. */}
        <SendBar campaign={campaign} stats={stats.value} />

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

      <SendControls campaign={campaign} writes={writes} />

      <TestSendPanel campaignId={campaign.id} testSend={writes.testSend} />

      <StatsPanel campaign={campaign} stats={stats} />

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
              A draft can still be changed. Once a send starts, the audience and template it went out
              with stay on the record.
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
function SendControls({ campaign, writes }: { campaign: CampaignRow; writes: CampaignWrites }) {
  const controls = writes.sendControls;
  const [asking, setAsking] = useState<"" | "start" | "cancel">("");
  const [when, setWhen] = useState("");

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

      {asking === "start" ? (
        <div className="os-campaign-confirm">
          <p className="os-campaign-confirm-line">
            Send {campaignName(campaign)} now
            {campaign.recipientCount > 0 ? ` to ${campaign.recipientCount} people` : ""}?
          </p>
          <Caption>
            Mail starts leaving immediately. Anyone on the do-not-mail list is skipped and recorded
            as skipped; everybody else gets it. There is no unsend.
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
          {draft || scheduled ? (
            <Button tone="primary" onClick={() => setAsking("start")}>
              Send now
            </Button>
          ) : null}
          {/* A PAUSED SEND RESUMES WITHOUT A CONFIRM. The irreversible step
              was starting it; resuming picks up a run that is already
              underway, and asking again would train somebody to click
              through the question that matters. */}
          {paused ? (
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

      {draft || scheduled ? (
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
              onClick={() => controls.schedule(campaign.id, when === "" ? campaign.scheduledAt : when)}
            >
              Schedule
            </Button>
          </Field>
          <Caption>
            A time with no offset is read as UTC. The same checks run now as they would at send time,
            so a missing sender or an unfinished template is caught here rather than at 3am.
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

/**
 * One test copy, and the merge tags it could not resolve.
 *
 * IT LIVES WITH THE CAMPAIGN because the builtin does: a test renders the
 * campaign's template through the campaign's resolved sending identity, so
 * there is no such thing as testing a template on its own. The template editor
 * mounts this same panel against a campaign that uses the template, which is
 * how the check lands where somebody is actually writing copy.
 *
 * THE UNRESOLVED LIST IS THE FEATURE. A typo'd `{{fields.compnay}}` renders as
 * its own literal text into somebody's inbox and looks like nothing from this
 * side; this is the only thing that catches it before the whole audience does.
 * A clean test says so out loud rather than showing nothing, because "no
 * warnings" and "the check did not run" look identical when both are silent.
 */
export function TestSendPanel({
  campaignId,
  testSend,
  label = "Send a test",
}: {
  campaignId: string;
  testSend: TestSendState;
  label?: string;
}) {
  const [to, setTo] = useState("");

  // The result belongs to the campaign it was run against. Switching campaigns
  // with a stale "Test sent" on screen would credit one campaign with another's
  // check.
  useEffect(() => {
    testSend.reset();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [campaignId]);

  return (
    <Panel label={label}>
      <Subhead>{label}</Subhead>
      <Field label="Send one copy to">
        <Input
          id={`os-campaign-test-${campaignId}`}
          label="Test recipient address"
          value={to}
          onChange={setTo}
          placeholder="you@example.com"
          onEnter={() => testSend.send(campaignId, to)}
        />
        <Button
          busy={testSend.busy}
          busyLabel="Sending"
          onClick={() => testSend.send(campaignId, to)}
        >
          Send test
        </Button>
      </Field>
      <Caption>
        Renders the real template against a stand-in recipient, using the merge data of the first
        real person in the audience so {"{{fields.*}}"} show the shape they will actually have. It
        writes no delivery record and moves no counter.
      </Caption>

      {testSend.error === "" ? null : (
        <Notice
          tone="error"
          sentence="The test did not go out."
          next="Nothing was sent and no counter moved."
          detail={testSend.error}
        />
      )}

      {!testSend.sent ? null : testSend.unresolved.length === 0 ? (
        <Notice tone="info" sentence="Test sent. Every merge tag in this template resolved." />
      ) : (
        <Notice
          tone="warn"
          sentence="Test sent -- but these merge tags did not resolve."
          next="They will appear as their own text in the message. Check the spelling, or the column name on the audience."
        >
          <div className="os-campaign-tags" role="list" aria-label="Merge tags that did not resolve">
            {testSend.unresolved.map((tag) => (
              <span key={tag} className="os-campaign-tag" role="listitem" data-unresolved>
                <span className="os-mono">{tag}</span>
              </span>
            ))}
          </div>
        </Notice>
      )}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// The full breakdown
// ---------------------------------------------------------------------------

/**
 * Everything the server counted, under the bar that summarises it.
 *
 * ON DEMAND AND IT SAYS SO. `campaignStats` reads the delivery ledger and the
 * consent stream, neither of which broadcasts, so this panel prints when it
 * looked and offers to look again. During a live send the BAR above moves and
 * this does not, which is exactly the honest split: the counters are on the
 * campaign row and arrive live, and the breakdown is a computation over rows
 * nothing announces.
 */
function StatsPanel({
  campaign,
  stats,
}: {
  campaign: CampaignRow;
  stats: ReturnType<typeof useCampaignStats>;
}) {
  const value = stats.value;
  return (
    <Panel label="Full breakdown">
      <div className="os-campaign-detail-head">
        <Subhead>Breakdown</Subhead>
        <Button busy={stats.state === "loading"} busyLabel="Reading" onClick={stats.reload}>
          {campaignIsRunning(campaign) ? "Read again" : "Re-read"}
        </Button>
      </div>

      {stats.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return the breakdown."
          next="The bar above is from the campaign's own counters and is still current."
          detail={stats.error}
        />
      ) : value === null ? (
        <Caption>
          {stats.state === "loading" ? "Reading the breakdown" : "No breakdown yet."}
        </Caption>
      ) : (
        <>
          <Facts>
            <Fact label="Recipients" value={formatFigure(value.recipients)} />
            <Fact label="Sent" value={formatFigure(value.sent)} />
            <Fact label="Not yet sent" value={formatFigure(value.pending)} />
            <Fact label="Failed" value={formatFigure(value.failed)} />
            <Fact label="Skipped" value={formatFigure(value.skipped)} />
            <Fact label="Skipped: on the do-not-mail list" value={formatFigure(value.skippedSuppressed)} />
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

      {stats.readAt === "" ? null : (
        <Caption>
          Read at {new Date(stats.readAt).toLocaleTimeString()}. This is not live -- read again to
          see what has happened since.
        </Caption>
      )}
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
      <div className="os-campaign-detail-head">
        <Subhead>Who got it</Subhead>
        <Button busy={ledger.state === "loading"} busyLabel="Reading" onClick={ledger.reload}>
          Read again
        </Button>
      </div>

      {ledger.state === "error" ? (
        <Notice
          tone="error"
          sentence="This cluster did not return the delivery record."
          next="Nothing is listed below -- that is silence, not an empty send."
          detail={ledger.error}
        />
      ) : rows.length === 0 ? (
        <Caption>
          {ledger.state === "loading"
            ? "Reading the delivery record"
            : "Nothing has been written to the record yet."}
        </Caption>
      ) : (
        <ul className="os-campaign-ledger" aria-label="Per-recipient outcomes">
          {rows.slice(0, LEDGER_ROWS).map((delivery) => (
            <li key={delivery.id} className="os-campaign-ledger-row" data-outcome={delivery.status}>
              <span className="os-mono os-campaign-ledger-address">{delivery.email || "--"}</span>
              <span className="os-campaign-ledger-outcome">
                {delivery.status === "skipped"
                  ? skipReasonSentence(delivery.skipReason)
                  : delivery.status || "unknown"}
              </span>
              {delivery.lastError === "" ? null : (
                <span className="os-caption os-mono">{delivery.lastError}</span>
              )}
              {delivery.sentAt === "" ? null : (
                <span className="os-caption">{formatMoment(delivery.sentAt)}</span>
              )}
            </li>
          ))}
          {rows.length > LEDGER_ROWS ? (
            <li className="os-caption">and {rows.length - LEDGER_ROWS} more in this page</li>
          ) : null}
        </ul>
      )}

      {ledger.readAt === "" ? null : (
        <Caption>
          Read at {new Date(ledger.readAt).toLocaleTimeString()}. Delivery records are not broadcast
          -- there is one per recipient per send, so they are read when you ask rather than streamed.
        </Caption>
      )}
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
function CampaignForm({
  campaign,
  audiences,
  templates,
  senders,
  trackByDefault,
  writes,
  onDone,
}: {
  campaign?: CampaignRow;
  audiences: AudienceRow[];
  templates: TemplateRow[];
  senders: SenderIdentityRow[];
  trackByDefault: boolean;
  writes: CampaignWrites;
  onDone: (createdId: string) => void;
}) {
  const accounts = useAccountOptions();
  const editing = campaign !== undefined;
  const write = editing ? writes.updateCampaign : writes.createCampaign;
  const [draft, setDraft] = useState(() => ({
    name: campaign?.name ?? "",
    audienceId: campaign?.audienceId ?? "",
    templateId: campaign?.templateId ?? "",
    fromName: campaign?.fromName ?? "",
    replyTo: campaign?.replyTo ?? "",
    scheduledAt: campaign?.scheduledAt ?? "",
    accountId: campaign?.accountId ?? "",
    senderIdentityId: campaign?.senderIdentityId ?? "",
    trackOpens: campaign?.trackOpens ?? trackByDefault,
    trackClicks: campaign?.trackClicks ?? trackByDefault,
  }));

  const ready =
    draft.name.trim() !== "" && draft.audienceId !== "" && draft.templateId !== "";

  async function submit() {
    if (editing && campaign) {
      const ok = await writes.updateCampaign.update(campaign.id, draft);
      if (ok) onDone(campaign.id);
      return;
    }
    const id = await writes.createCampaign.create(draft);
    if (id !== "") onDone(id);
  }

  // Only the audiences and templates somebody could sensibly send: archived
  // ones are kept for their history and are not offered here. A campaign
  // ALREADY naming one keeps it (the value is on the draft and the option is
  // synthesized below), so an edit never silently re-points a campaign.
  const pickable = {
    audiences: audiences.filter((a) => a.status !== "archived" || a.id === draft.audienceId),
    templates: templates.filter((t) => t.status !== "archived" || t.id === draft.templateId),
    senders: senders.filter((s) => s.status !== "disabled" || s.id === draft.senderIdentityId),
  };

  return (
    <Panel label={editing ? "Edit campaign" : "New campaign"}>
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
            <option value="">This cluster's default mailbox</option>
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
        <Field label="Client">
          <AccountPicker
            id="os-campaign-account"
            label="Client this campaign is for"
            value={draft.accountId}
            onChange={(v) => setDraft({ ...draft, accountId: v })}
            accounts={accounts}
          />
        </Field>
      </div>

      <div className="os-campaign-tracking">
        <Check
          checked={draft.trackOpens}
          onChange={(v) => setDraft({ ...draft, trackOpens: v })}
        >
          Count who opens it
        </Check>
        <Check
          checked={draft.trackClicks}
          onChange={(v) => setDraft({ ...draft, trackClicks: v })}
        >
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
        <Button tone="primary" busy={write.busy} busyLabel="Saving" onClick={submit} disabled={!ready}>
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
