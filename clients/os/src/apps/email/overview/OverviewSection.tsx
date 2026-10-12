import { useMemo } from "react";
import { Send } from "lucide-react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, EmptyState, Notice, useNow } from "../../../kit";
import { Overview, OverviewBreakdown } from "../../../kit/Overview";
import type { AbsentFigure } from "../../../kit/measure";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { useOpenRecord } from "../openRequest";
import { audienceIsArchived, audienceProjection, campaignFromRow, emailRuleFromRow } from "../rows";
import { EMAIL_REFRESH_MS, useCampaignStatsFor, useSubscriberCounts, type EmailFeeds } from "../useEmail";
import { campaignDistribution, overviewMetrics, statsWanted, type OverviewInputs } from "./figures";

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
// being wrong; a read the cluster refused is a dash with the engine's own
// sentence beneath the figures; and a count of nothing is a zero only when the
// cluster said there was nothing.
//
// IT READS NOTHING ON ITS OWN CLOCK. The campaigns, audiences and rules are the
// root's live feeds; the breakdowns and subscriber counts are readings with no
// subscription to follow, refreshed by the `AutoRefresh` below only while this
// pane is seen -- a parked tab, a hidden browser tab and an offline window stop
// reading.

/** Whether a feed has something true to count: it answered, or it holds the last answer it had. */
function answered(snapshot: LiveSnapshot<Row>): boolean {
  return snapshot.state === "live" || snapshot.state === "degraded" || (snapshot.state === "disconnected" && snapshot.rows.length > 0);
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
  // of the audiences that can be mailed. Both are the engine's exact counts.
  const wanted = useMemo(() => statsWanted(campaigns, now).map((campaign) => campaign.id), [campaigns, now]);
  const { stats, reload: reloadStats } = useCampaignStatsFor(wanted);
  const { counts, reload: reloadCounts } = useSubscriberCounts(activeAudiences);

  const inputs: OverviewInputs = {
    campaigns: { rows: campaigns, settled: answered(feeds.campaigns.snapshot), error: feeds.campaigns.snapshot.error },
    stats,
    subscribers: counts,
    audiencesSettled: answered(feeds.audiences.snapshot),
    audiencesError: feeds.audiences.snapshot.error,
    activeAutomations: {
      count: rules.filter((rule) => rule.status === "active").length,
      settled: answered(feeds.rules.snapshot),
      error: feeds.rules.snapshot.error,
    },
    now,
  };
  const metrics = overviewMetrics(inputs);

  // THE ENGINE'S SENTENCE FOR EACH READ IT REFUSED, once, in the engine's own
  // words (the dashes carry it on hover too). However many campaigns' breakdowns
  // or audiences' counts were refused, it is one sentence about one read.
  const openRefusal = [...stats.values()].find((reading) => reading.state === "error")?.error ?? "";
  const refusedCount = [...counts.values()].find((figure): figure is AbsentFigure => figure.kind === "absent" && figure.reason === "failed");
  const subscribersRefusal = feeds.audiences.snapshot.error || (refusedCount?.detail ?? "");
  const refusals = [
    { sentence: "This cluster did not return your campaigns.", detail: inputs.campaigns.error },
    { sentence: "This cluster did not return the opens and clicks.", detail: openRefusal },
    { sentence: "This cluster did not return your subscribers.", detail: subscribersRefusal },
    { sentence: "This cluster did not return your automations.", detail: inputs.activeAutomations.error },
  ].filter((refusal) => refusal.detail !== "");

  const behind = [feeds.campaigns, feeds.audiences, feeds.rules].filter((feed) => feedIsBehind(feed.snapshot.state));
  const received = behind.some((feed) => feed.snapshot.rows.length > 0);

  // FIRST USE is only ever said once the cluster has answered every list: a
  // cluster that could not say what it holds has not said it holds nothing.
  const everyListAnswered = [feeds.campaigns, feeds.audiences, feeds.rules].every((feed) => answered(feed.snapshot) && feed.snapshot.error === "");
  const firstUse = everyListAnswered && campaigns.length === 0 && rules.length === 0;

  // A FIRST USE SAYS WHY THERE IS NO RATE ONCE, in its one sentence below: the
  // same reason is not repeated under each dash.
  const shown = firstUse ? metrics.map(({ detail: _detail, ...metric }) => metric) : metrics;

  return (
    <Overview metrics={shown}>
      {behind.length > 0 ? (
        <Notice
          tone="warn"
          sentence="Updates are interrupted."
          next={received ? "Showing the last figures received. Reconnecting automatically." : "Reconnecting automatically."}
        />
      ) : null}
      {refusals.map((refusal) => (
        <Notice key={`${refusal.sentence}|${refusal.detail}`} tone="error" sentence={refusal.sentence} detail={refusal.detail} />
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
      <AutoRefresh onRefresh={() => Promise.all([reloadStats(), reloadCounts()])} intervalMs={EMAIL_REFRESH_MS} />
    </Overview>
  );
}
