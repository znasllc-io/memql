import { useMemo, useState } from "react";

import { AutoRefresh, Caption, Fact, Facts, Notice, Panel, RecordList, RecordRow, Refine, Select, Subhead, type RefineChip } from "../../../kit";
import { ContentSkeleton, InlineSkeleton } from "../../../kit/ContentSkeleton";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import {
  deliveryFromRow,
  formatRate,
  providerDeliverySentence,
  rateOf,
  sendBreakdown,
  skipReasonSentence,
  type CampaignRow,
  type CampaignStats,
  type Figure,
} from "../rows";
import { EMAIL_REFRESH_MS, useDeliveryLedger, type Reading } from "../useEmail";
import { shortMoment } from "../when";
import { WORDS } from "../words";
import { SendBar } from "./SendBar";

// WHAT HAPPENED WHEN IT WENT OUT (memql#6033): the band, what the people who got
// it did with it, the server's breakdown, and who got it.
//
// A CAMPAIGN THAT HAS SENT NOTHING DRAWS NONE OF THIS. A draft's page used to
// open on an empty band and a breakdown of zeros, which is a picture of nothing
// that reads as a picture of a failure. The page draws these once there is a
// send to describe.
//
// ABSENT IS NEVER ZERO. A figure the server could not measure is an em dash
// with its reason; a read the cluster refused says so, in its own words, and
// what was read before it stays on screen.

/** A measured figure as a person reads a count ("1,204"), or the em dash. */
function count(figure: Figure): string {
  return figure.value === null ? "--" : `${figure.atLeast ? "≥" : ""}${figure.value.toLocaleString("en-US")}`;
}

/** The band and the engagement beneath it. */
export function Results({ campaign, stats }: { campaign: CampaignRow; stats: Reading<CampaignStats | null> }) {
  return (
    <Panel label="Results">
      <Subhead>Results</Subhead>
      <SendBar campaign={campaign} />
      <Caption>Sent means accepted for sending.</Caption>
      <Engagement campaign={campaign} stats={stats} />
    </Panel>
  );
}

/**
 * Opens and clicks, BELOW the band and smaller than it.
 *
 * A DIFFERENT QUESTION, about the sent slice only, so it is not a fifth
 * segment: opens are a share of what was accepted for sending, not of the audience, and a
 * band whose slices meant two different things could not be read at all. The
 * rate leads because it is the comparable figure; the unique-of-total pair
 * follows because a rate with no counts behind it cannot be checked.
 *
 * TRACKING OFF IS NOT ZERO ENGAGEMENT: a campaign that never asked to be counted
 * has no figure, and "0% opened" would be a lie about its recipients.
 */
function Engagement({ campaign, stats }: { campaign: CampaignRow; stats: Reading<CampaignStats | null> }) {
  if (!campaign.trackOpens && !campaign.trackClicks) {
    return <Caption>Opens and clicks weren't counted for this campaign.</Caption>;
  }
  const value = stats.value;
  const sent = value?.sent.value ?? 0;
  const reading = value === null && stats.state === "loading";
  return (
    <div className="os-send-engagement">
      {campaign.trackOpens ? (
        <EngagementFigure label="Opened" unique={value?.opensUnique ?? null} total={value?.opensTotal ?? null} sent={sent} bounded={!!value?.sent.atLeast} reading={reading} />
      ) : null}
      {campaign.trackClicks ? (
        <EngagementFigure label="Clicked" unique={value?.clicksUnique ?? null} total={value?.clicksTotal ?? null} sent={sent} bounded={!!value?.sent.atLeast} reading={reading} />
      ) : null}
    </div>
  );
}

function EngagementFigure({ label, unique, total, sent, bounded, reading }: {
  label: string;
  /** Null when the breakdown has not been read (or was refused): no figure, and no sentence about one. */
  unique: Figure | null;
  total: Figure | null;
  sent: number;
  bounded: boolean;
  reading: boolean;
}) {
  return (
    <div className="os-send-engagement-item">
      <span className="os-send-engagement-rate">
        {reading ? <InlineSkeleton label={`Counting who ${label.toLowerCase()}`} /> : unique === null ? "--" : formatRate(rateOf(unique, sent, bounded))}
      </span>
      <span className="os-send-engagement-label">{label}</span>
      {total === null || total.value === null ? null : (
        // A unique count that could not be measured is left out, not shown as a dash in a sentence.
        <span className="os-caption">{unique === null || unique.value === null ? `${count(total)} times in all` : `${count(unique)} recipients, ${count(total)} times in all`}</span>
      )}
      {unique === null || unique.absentBecause === "" ? null : <span className="os-caption os-send-engagement-note">{unique.absentBecause}</span>}
    </div>
  );
}

/** One measured figure, or an em dash whose reason is on hover. */
function StatFact({ label, figure }: { label: string; figure: Figure }) {
  return <Fact label={label} value={count(figure)} title={figure.absentBecause || undefined} />;
}

/**
 * What the band's legend does not say, refreshed while it is seen: bounces,
 * spam reports and unsubscribes arrive after a send ends.
 *
 * EACH NUMBER IS SAID ONCE. How many were sent, skipped, failed and not yet
 * sent is the legend's, right above; a second copy here would be a second
 * place to read it and, between two refreshes, a second answer. The breakdown
 * keeps the whole (Recipients), why people were skipped, and what happened
 * after sending. RECIPIENTS IS THE BAND'S WHOLE -- the roster frozen for the
 * send -- not the audience as it is today, which grows and shrinks after.
 */
export function Breakdown({ campaign, stats }: { campaign: CampaignRow; stats: Reading<CampaignStats | null> }) {
  const value = stats.value;
  // Answered with no breakdown at all: there is nothing to show, and a box of
  // em dashes would read as one.
  if (value === null && stats.state !== "loading" && stats.state !== "error") return null;
  return (
    <Panel label="Breakdown">
      <Subhead>Breakdown</Subhead>
      {stats.state === "error" ? <Notice tone="error" sentence="This cluster did not return the breakdown." detail={stats.error} /> : null}
      {value === null ? (
        stats.state === "error" ? null : <ContentSkeleton kind="metrics" label="Loading the breakdown" />
      ) : (
        <>
          <Facts>
            <Fact label="Recipients" value={sendBreakdown(campaign).total.toLocaleString("en-US")} />
            <StatFact label="Skipped: on the do-not-mail list" figure={value.skippedSuppressed} />
            <StatFact label="Skipped: unsubscribed" figure={value.skippedUnsubscribed} />
            <StatFact label="Skipped: some other reason" figure={value.skippedOther} />
            <StatFact label="Confirmed by recipient server" figure={value.deliveryConfirmed} />
            <StatFact label="Failed after sending" figure={value.deliveryFailed} />
            <StatFact label="Expanded to a distribution list" figure={value.deliveryExpanded} />
            <StatFact label="Delivery unconfirmed" figure={value.deliveryUnconfirmed} />
            <StatFact label="Hard bounces" figure={value.hardBounces} />
            <StatFact label="Spam reports" figure={value.complaints} />
            <StatFact label="Unsubscribed from this" figure={value.unsubscribes} />
          </Facts>
          <Caption>Server confirmation does not confirm inbox placement. Expanded lists have no final delivery report.</Caption>
          {/* Said once: there is never a soft-bounce figure to show, and a line
              for one would be read as a zero. */}
          <Caption>{value.softBounces.absentBecause}</Caption>
        </>
      )}
    </Panel>
  );
}

type Outcome = "" | "sent" | "skipped" | "failed" | "pending";

const OUTCOMES: readonly { value: Outcome; label: string }[] = [
  { value: "", label: "Any outcome" },
  { value: "sent", label: WORDS.delivery.sent! },
  { value: "skipped", label: WORDS.delivery.skipped! },
  { value: "failed", label: WORDS.delivery.failed! },
  { value: "pending", label: WORDS.delivery.pending! },
];

/**
 * Who got it: one row per recipient, newest first, read a page at a time.
 *
 * The ledger is a routing exclusion (one row per recipient per send), so it is
 * READ, not followed: the newest page again while it is seen, and older pages
 * when somebody asks for them. Refine asks its questions of the rows already
 * read -- and says so when none of them answer.
 *
 * `total` is how many delivery records exist, when that is known: the heading
 * then says how many of them are listed ("Latest 100 of 4,182").
 */
export function WhoGotIt({ campaignId, total }: { campaignId: string; total: number | null }) {
  const ledger = useDeliveryLedger(campaignId);
  const [search, setSearch] = useState("");
  const [outcome, setOutcome] = useState<Outcome>("");
  const deliveries = useMemo(() => ledger.rows.map(deliveryFromRow), [ledger.rows]);
  const needle = search.trim().toLowerCase();
  const shown = deliveries.filter((delivery) => (needle === "" || delivery.email.toLowerCase().includes(needle)) && (outcome === "" || delivery.status === outcome));

  const first = ledger.state === "loading" && deliveries.length === 0;
  const refused = ledger.error !== "";
  // Read and empty: a send that has not written its first record yet. Nothing
  // to show -- but the reading keeps running, so the first one appears.
  const nothing = !first && !refused && deliveries.length === 0;
  const loaded = deliveries.length.toLocaleString("en-US");
  const meta = ledger.more && deliveries.length > 0 ? (total !== null && total > deliveries.length ? `Latest ${loaded} of ${total.toLocaleString("en-US")}` : `Latest ${loaded}`) : undefined;
  const chips: RefineChip[] = outcome === "" ? [] : [{ id: "outcome", label: WORDS.delivery[outcome] ?? outcome, onRemove: () => setOutcome("") }];

  return (
    <>
      <AutoRefresh onRefresh={ledger.reload} busy={ledger.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
      {nothing ? null : (
        <Panel label="Recipients">
          <Subhead
            meta={meta}
            actions={
              deliveries.length === 0 ? null : (
                <Refine iconOnly search={search} onSearch={setSearch} chips={chips} label="Refine recipients">
                  <Select id={`email-delivery-outcome-${campaignId}`} label="Sending" value={outcome} onChange={(next) => setOutcome(next as Outcome)}>
                    {OUTCOMES.map((option) => (
                      <option key={option.value} value={option.value}>
                        {option.label}
                      </option>
                    ))}
                  </Select>
                </Refine>
              )
            }
          >
            Recipients
          </Subhead>
          {refused ? <Notice tone="error" sentence="This cluster did not return the delivery record." detail={ledger.error} /> : null}
          {first ? <RecordListSkeleton label="Loading the delivery record" /> : null}
          {shown.length > 0 ? (
            <RecordList as="ul" label="Recipients">
              {shown.map((delivery) => (
                <RecordRow
                  key={delivery.id}
                  name={<span className="os-mono">{delivery.email || "--"}</span>}
                  state={WORDS.delivery[delivery.status] ?? delivery.status}
                  tone={delivery.status === "sent" ? "accent" : delivery.lastError !== "" ? "warn" : "muted"}
                  secondary={[delivery.status === "sent" || delivery.providerStatus !== "" ? providerDeliverySentence(delivery.providerStatus) : "", delivery.lastError || (delivery.status === "skipped" ? skipReasonSentence(delivery.skipReason) : "")].filter(Boolean).join(". ")}
                >
                  {delivery.sentAt === "" ? null : <span className="os-caption">Sent {shortMoment(delivery.sentAt)}</span>}
                  {delivery.providerAt === "" ? null : <span className="os-caption">Reported {shortMoment(delivery.providerAt)}</span>}
                </RecordRow>
              ))}
            </RecordList>
          ) : deliveries.length > 0 ? (
            <Caption>No match among the loaded recipients.</Caption>
          ) : null}
          {ledger.moreError !== "" ? <Notice tone="error" sentence="This cluster did not return more of the delivery record." detail={ledger.moreError} /> : null}
          {!ledger.more || deliveries.length === 0 ? null : ledger.loadingMore ? (
            <RecordListSkeleton rows={2} label="Loading more of the delivery record" />
          ) : (
            <p className="email-more">
              <button type="button" className="os-link" onClick={ledger.loadMore}>
                Show more
              </button>
            </p>
          )}
        </Panel>
      )}
    </>
  );
}
