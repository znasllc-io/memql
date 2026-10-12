import { useMemo } from "react";
import { Send } from "lucide-react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, EmptyState, Notice, useNow } from "../../../kit";
import { Overview, OverviewBreakdown } from "../../../kit/Overview";
import type { AbsentFigure } from "../../../kit/measure";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { useOpenRecord } from "../openRequest";
import { audienceIsArchived, audienceProjection, campaignFromRow, emailRuleFromRow } from "../rows";
import { useCampaignStatsFor, useSubscriberCounts, type EmailFeeds } from "../useEmail";
import { campaignDistribution, overviewMetrics, refusalOf, statsWanted, type OverviewInputs } from "./figures";

// OVERVIEW: what was sent, who it can reach, and what runs on its own
// (memql#6033, Ruling 22).
//
// FIVE MEASURED FIGURES AND THE SHAPE OF THE CAMPAIGNS, and nothing else: no
// activity history (no time series exists to draw one from) and no standing
// advice about setup (a decision that needs one is made on the page it is made
// on). Every number comes from a read the app already makes, and `figures.ts`
// is where each one is worked out.
//
// A FIGURE IS A NUMBER, A REASON THERE IS NONE, OR A SHAPE. While a read is on
// its way the figure is the shape of a figure, never a warning about nothing
// being wrong; a read the cluster refused with nothing to fall back on is a dash
// with the engine's own sentence beneath the figures; and a count of nothing is
// a zero only when the cluster said there was nothing.
//
// A REFUSED REFRESH IS NOT A REFUSAL WHILE THE LAST ANSWER IS HELD. A feed that
// holds rows and could not read them again, a breakdown or a count that has been
// read before and was refused this time: each keeps its last value, and the page
// tells ONE story, in one notice, with the engine's sentence in it -- updates are
// interrupted, these are the last figures received. It never dashes a figure it
// can still draw, and it never says "this cluster did not return" beside a
// number it is showing.
//
// IT READS ONCE A MINUTE, NOT ON THE CAMPAIGNS' FIVE SECONDS. This is the tab the
// app opens on, so its cost is paid by everybody every time. What must be live
// (what was sent, what is switched on) is live through the root's feeds; the
// breakdowns and subscriber counts have no subscription to follow and move over
// hours and days, so a minute is as fresh as they can be said to be. The
// `AutoRefresh` below reads them only while this pane is seen: a parked tab, a
// hidden browser tab and an offline window stop reading.

/** How often the breakdowns and subscriber counts are read again while this pane is seen. */
export const OVERVIEW_REFRESH_MS = 60_000;

/** Whether a feed has something true to count: it answered, or it holds the last answer it had. */
function answered(snapshot: LiveSnapshot<Row>): boolean {
  return snapshot.state === "live" || snapshot.state === "degraded" || (snapshot.state === "disconnected" && snapshot.rows.length > 0);
}

/**
 * What a feed has to say about itself: a refusal that left it nothing to show,
 * or a refresh it could not make while it kept the last rows (`kept`). The SDK
 * holds the rows and the error together, so which one it is depends on the rows.
 */
function story(snapshot: LiveSnapshot<Row>): { refusal: string; kept: string } {
  return { refusal: refusalOf(snapshot.error, snapshot.rows.length), kept: snapshot.rows.length > 0 ? snapshot.error : "" };
}

export function OverviewSection({ feeds }: { feeds: EmailFeeds }) {
  const openRecord = useOpenRecord();
  // One clock for the 30-day window. A minute is the finest it can matter at.
  const now = useNow(60_000);

  const campaignRows = feeds.campaigns.snapshot.rows;
  const campaigns = useMemo(() => campaignRows.map(campaignFromRow).filter((campaign) => campaign.id !== ""), [campaignRows]);
  const audienceRows = feeds.audiences.snapshot.rows;
  const activeAudiences = useMemo(
    () => audienceProjection(audienceRows).filter((audience) => !audienceIsArchived(audience)).map((audience) => audience.id),
    [audienceRows],
  );
  const ruleRows = feeds.rules.snapshot.rows;
  const rules = useMemo(() => ruleRows.map(emailRuleFromRow).filter((rule) => rule.id !== ""), [ruleRows]);

  // The breakdowns of the campaigns a rate can be made of, and the subscribers
  // of the audiences that can be mailed, both counted by the engine. A
  // subscriber count that reached its window comes back a floor and is drawn as
  // one; a breakdown cannot say so yet (figures.ts, "Open / Click rate").
  const wanted = useMemo(() => statsWanted(campaigns, now).map((campaign) => campaign.id), [campaigns, now]);
  const { stats, reload: reloadStats } = useCampaignStatsFor(wanted);
  const { counts, refusals: countRefusals, reload: reloadCounts } = useSubscriberCounts(activeAudiences);

  const campaignsStory = story(feeds.campaigns.snapshot);
  const audiencesStory = story(feeds.audiences.snapshot);
  const rulesStory = story(feeds.rules.snapshot);

  const inputs: OverviewInputs = {
    campaigns: { rows: campaigns, settled: answered(feeds.campaigns.snapshot), error: campaignsStory.refusal },
    stats,
    subscribers: counts,
    audiencesSettled: answered(feeds.audiences.snapshot),
    audiencesError: audiencesStory.refusal,
    activeAutomations: {
      count: rules.filter((rule) => rule.status === "active").length,
      settled: answered(feeds.rules.snapshot),
      error: rulesStory.refusal,
    },
    now,
  };
  const metrics = overviewMetrics(inputs);

  // WHAT THE CLUSTER REFUSED THAT LEFT A FIGURE NOTHING TO SHOW: its sentence, in
  // the engine's own words, once however many breakdowns or counts were refused
  // (the dashes carry it on hover too).
  const statsRefusal = [...stats.values()].find((reading) => reading.state === "error" && reading.value === null)?.error ?? "";
  const refusedCount = [...counts.values()].find((figure): figure is AbsentFigure => figure.kind === "absent" && figure.reason === "failed");
  const refusalNotices = [
    { sentence: "This cluster did not return your campaigns.", detail: campaignsStory.refusal },
    { sentence: "This cluster did not return the opens and clicks.", detail: statsRefusal },
    { sentence: "This cluster did not return your subscriptions.", detail: audiencesStory.refusal || (refusedCount?.detail ?? "") },
    { sentence: "This cluster did not return your automations.", detail: rulesStory.refusal },
  ].filter((notice) => notice.detail !== "");

  // WHAT THE CLUSTER REFUSED WHILE THE LAST FIGURES WERE HELD: the sentences,
  // each said once, for the one notice below.
  const feedsKept = [campaignsStory.kept, audiencesStory.kept, rulesStory.kept].filter((sentence) => sentence !== "");
  const statsKept = [...stats.values()].filter((reading) => reading.state === "error" && reading.value !== null).map((reading) => reading.error);
  const countsKept = [...countRefusals].filter(([id]) => counts.get(id)?.kind === "measured").map(([, sentence]) => sentence);
  const kept = [...new Set([...feedsKept, ...statsKept, ...countsKept])];

  const behind = [feeds.campaigns, feeds.audiences, feeds.rules].filter((feed) => feedIsBehind(feed.snapshot.state));
  const interrupted = behind.length > 0 || kept.length > 0;
  // Figures are on screen if a feed that is behind holds its rows, or a refresh was refused over a value.
  const showing = kept.length > 0 || behind.some((feed) => feed.snapshot.rows.length > 0);
  // Reconnecting is claimed only where it is true: the connection is what stopped,
  // and the SDK reconnects and reads again. A feed's refusal is not retried by anything.
  const reconnecting = behind.length > 0 && feedsKept.length === 0;
  const next = showing ? `Showing the last figures received.${reconnecting ? " Reconnecting automatically." : ""}` : "Reconnecting automatically.";

  // FIRST USE is only ever said once the cluster has answered every list: a
  // cluster that could not say what it holds has not said it holds nothing.
  const everyListAnswered = [feeds.campaigns, feeds.audiences, feeds.rules].every((feed) => answered(feed.snapshot) && feed.snapshot.error === "");
  const firstUse = everyListAnswered && campaigns.length === 0 && rules.length === 0;

  // A FIRST USE SAYS WHY THERE IS NO RATE ONCE, in its one sentence below: the
  // same reason is not repeated under each dash.
  const shown = firstUse ? metrics.map(({ detail: _detail, ...metric }) => metric) : metrics;

  return (
    <Overview metrics={shown}>
      {interrupted ? <Notice tone="warn" sentence="Updates are interrupted." next={next} detail={kept.join(" ")} /> : null}
      {refusalNotices.map((notice) => (
        <Notice key={`${notice.sentence}|${notice.detail}`} tone="error" sentence={notice.sentence} detail={notice.detail} />
      ))}
      {firstUse ? (
        <EmptyState
          icon={Send}
          title="Nothing sent yet"
          action={<Button tone="primary" onClick={() => openRecord("campaigns", { add: true })}>Add a campaign</Button>}
        >
          Add a campaign to send a template to an audience.
        </EmptyState>
      ) : null}
      {inputs.campaigns.settled && inputs.campaigns.error === "" ? (
        <OverviewBreakdown title="Campaigns" segments={campaignDistribution(campaigns)} />
      ) : null}
      <AutoRefresh onRefresh={() => Promise.all([reloadStats(), reloadCounts()])} intervalMs={OVERVIEW_REFRESH_MS} />
    </Overview>
  );
}
