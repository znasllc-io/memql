import { useMemo, useRef, useState, type ReactNode } from "react";
import { Send, Users, Workflow, Zap } from "lucide-react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, Caption, EmptyState, Fact, Facts, HeadNote, Measure, Notice, RecordList, RecordRow, Subhead } from "../../../kit";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { formatCount } from "../../../kit/format";
import { absent, figureOf, type AbsentFigure } from "../../../kit/measure";
import { Overview, OverviewBreakdown } from "../../../kit/Overview";
import { PageHelp } from "../../../kit/pageHelp";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { EMAIL_PERIODS, PeriodControl, RankedList, StatFigures, TimeChart, TimeChartSkeleton, type RankedRow } from "../../../kit/statistics";
import { feedIsBehind } from "../../../live/useLiveCollection";
import { useClientScope } from "../../accounts/clientScope";
import { accountName, untitledAccountName } from "../../accounts/rows";
import { ScopeControl } from "../../accounts/ScopeControl";
import { useAccountOptionsFeed } from "../../accounts/tie";
import { useEmailCatalogs } from "../catalogs";
import { useOpenRecord } from "../openRequest";
import { audienceIsArchived, audienceName, audienceProjection, campaignFromRow, campaignName, emailRuleFromRow, journeyFromRow, journeyName, ruleName } from "../rows";
import type { EmailSettings } from "../settings";
import { useSubscriberCounts, type EmailFeeds } from "../useEmail";
import {
  DELIVERY_FIGURES,
  EMAIL_FIGURES,
  activeAutomationsFact,
  automationLine,
  campaignDistribution,
  campaignLine,
  chartPoints,
  clientLine,
  formatOf,
  growthLine,
  listsMeasured,
  previousPoints,
  providerReportsOutcomes,
  refusalOf,
  statFigures,
  subscriptionsFact,
  type FactReading,
  type FigureSpec,
} from "./figures";
import { STATISTICS_REFRESH_MS, useEmailStatistics, type EmailStatistics } from "./statistics";

// OVERVIEW: the email statistics of a period, for one client or every client in
// scope (spec 9, D13, D18), read from `emailStatistics`.
//
// THE SHAPE OF THE PAGE, top to bottom: the Head with the client (only when
// there is a choice, D13) and the period; five figures, the one pressed drawn
// as the chart beneath them; what is true NOW -- Subscriptions and Active
// automations -- as facts, because they are about now, not the period; then
// the period's subjects: Delivery problems (its own figures and chart, and
// Delivered only where the provider reports outcomes), Audience growth, Top
// campaigns, Automations; the campaigns by state; and Clients when the read
// covers more than one. Each row of a list opens the record it names.
//
// THE FIGURES SAY WHAT THE READ SAID. While it is on its way, every figure, the
// chart's place and every list is its shape, never a word. Refused with nothing
// to show, the figures are dashes carrying the engine's sentence, said once in
// the chart's place, and nothing else of the period is drawn. A period nothing
// counted (no `measuredSince`) is dashes that say why and a chart that says
// "Not measured", and no lists: a list of what happened in a period nobody
// counted would be a claim about it. The read lists only what it measured, so
// a list that arrives empty says "Nothing in this period." only where its
// figures were counted through the period, and "Not measured." otherwise
// (`listsMeasured`).
//
// A REFUSED REFRESH IS NOT A REFUSAL WHILE THE LAST ANSWER IS HELD. A read that
// could not be made again keeps its last answer (`useReading`, the feeds), and
// the page says so ONCE, on the heading's line (kit `HeadNote`, DESIGN.md "A
// feed that falls behind"): updates are interrupted, these are the last figures
// received. A block above the content would move the page under the pointer.
//
// CAMPAIGN AND CLIENT ROWS COUNT OPENS BY SEND COHORT, while nothing on the page
// totals unique opens, so no row is laid out as a part of a figure above it.
//
// TWO CADENCES. The statistics are read again every fifteen minutes
// (`STATISTICS_REFRESH_MS`, spec 7): a day's figures move over hours. What is
// true now -- the subscriber counts -- is read again each minute, as before;
// it has no subscription to follow. Both stop while the pane is not seen. The
// lists' names come from the root's feeds, which are live.

/** How often the subscriber counts are read again while this pane is seen. */
export const OVERVIEW_REFRESH_MS = 60_000;

/** What a list says once the read has answered with nothing for it, its figures counted. */
const NOTHING_IN_THE_PERIOD = "Nothing in this period.";

/** What a list says when its figures were not counted through the period, so the read could not list it. */
const NOT_MEASURED = "Not measured.";

/** Whether a feed has something true to say: it answered, or it holds the last answer it had. */
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

/** Whether a record belongs to the client in scope: every record for All, else the chosen organization's. */
function inScope(accountIds: readonly string[], accountId: string): boolean {
  return accountIds.length === 0 || accountIds.includes(accountId);
}

function factValue(fact: FactReading, label: string): ReactNode {
  return fact.loading ? <InlineSkeleton label={`Loading ${label.toLowerCase()}`} /> : <Measure figure={fact.figure} format={formatCount} />;
}

/** The spec a figure row has chosen, or its first when the choice is not one of its own. */
function chosen(specs: readonly FigureSpec[], id: string): FigureSpec {
  return specs.find((spec) => spec.id === id) ?? specs[0]!;
}

/** A chart of one figure, its shape while the read is on its way, and nothing without an answer to draw. */
function FigureChart({ spec, stats, reading }: { spec: FigureSpec; stats: EmailStatistics | null; reading: boolean }) {
  if (reading) return <TimeChartSkeleton label={`Loading ${spec.label.toLowerCase()} over time`} />;
  if (stats === null) return null;
  return <TimeChart label={spec.label} bucket={stats.bucket} points={chartPoints(stats.series, spec.id)} previous={previousPoints(stats, spec.id)} format={formatOf(spec)} />;
}

/**
 * A row's quiet line, each part kept whole: in a narrow window it wraps at a
 * separator, never between a figure and its word ("1,204" over "subscribed").
 */
function Parts({ line }: { line: string }) {
  return (
    <>
      {line.split(" · ").map((part, index) => (
        <span key={index}>
          {index > 0 ? " · " : null}
          <span className="email-overview-part">{part}</span>
        </span>
      ))}
    </>
  );
}

/**
 * A subject of the period under its own heading line, named to assistive
 * technology as it reads.
 *
 * THE SHELL'S OWN GRAMMAR, no spacing of this app's: the heading line is the
 * shell's list heading (`.os-record-heading`, the control line's height with
 * the heading's margin taken off), the box the kit's ranked lists give theirs,
 * so Audience growth stands like Top campaigns beside it. A list is the
 * shell's record section, its rows as close under the heading as a ranked
 * list's; Delivery problems -- a figure row, a chart and a fact -- stacks as
 * the page itself does.
 */
function Subject({ title, list = false, children }: { title: string; list?: boolean; children: ReactNode }) {
  return (
    <section className={list ? "os-record-section" : "os-app-stack"} aria-label={title}>
      <div className="os-record-heading">
        <Subhead>{title}</Subhead>
      </div>
      {children}
    </section>
  );
}

export function OverviewSection({ feeds, settings, update }: { feeds: EmailFeeds; settings: EmailSettings; update: (patch: Partial<EmailSettings>) => void }) {
  const openRecord = useOpenRecord();
  useEmailCatalogs(["campaigns", "audiences", "rules", "journeys"]);

  // WHOSE MAIL: the client the person chose, remembered in Email's settings,
  // and offered only when there is a choice (D13). What a read sends is the
  // scope's own `accountIds`, as it is.
  const scope = useClientScope(settings.statisticsScope, (value) => update({ statisticsScope: value }));
  const accountIds = scope.accountIds;
  const period = settings.statisticsPeriod;
  const stats = useEmailStatistics({ accountIds, period });
  const value = stats.value;
  const reading = value === null && (stats.state === "idle" || stats.state === "loading");
  const measured = value !== null && value.measuredSince !== null;
  const accounts = useAccountOptionsFeed();

  const campaignRows = feeds.campaigns.snapshot.rows;
  const allCampaigns = useMemo(() => campaignRows.map(campaignFromRow).filter((campaign) => campaign.id !== ""), [campaignRows]);
  const audienceRows = feeds.audiences.snapshot.rows;
  const allAudiences = useMemo(() => audienceProjection(audienceRows), [audienceRows]);
  const ruleRows = feeds.rules.snapshot.rows;
  const allRules = useMemo(() => ruleRows.map(emailRuleFromRow).filter((rule) => rule.id !== ""), [ruleRows]);
  const journeyRows = feeds.journeys.snapshot.rows;
  const allJourneys = useMemo(() => journeyRows.map(journeyFromRow).filter((journey) => journey.id !== ""), [journeyRows]);

  // `accountIds` keeps its identity until the scope changes (useClientScope memoizes it).
  const campaigns = useMemo(() => allCampaigns.filter((campaign) => inScope(accountIds, campaign.accountId)), [allCampaigns, accountIds]);
  const subscribed = useMemo(
    () => allAudiences.filter((audience) => !audienceIsArchived(audience) && inScope(accountIds, audience.accountId)).map((audience) => audience.id),
    [allAudiences, accountIds],
  );
  const activeCount =
    allRules.filter((rule) => rule.status === "active" && inScope(accountIds, rule.accountId)).length +
    allJourneys.filter((journey) => journey.status === "active" && inScope(accountIds, journey.accountId)).length;

  // THE AUDIENCES WHOSE SIZE IS ASKED: the active ones in scope (Subscriptions),
  // and the ones the growth list names that the audiences list holds (an
  // audience it does not hold is not asked about). The growth list's are the
  // LAST ANSWER's, kept while a new period or client is read, so the counts are
  // not read again for a set that only blinked.
  const named = useRef<readonly string[]>([]);
  if (value !== null) named.current = value.audiences.map((audience) => audience.id);
  const known = new Set(allAudiences.map((audience) => audience.id));
  const counted = [...new Set([...subscribed, ...named.current.filter((id) => known.has(id))])];
  const { counts, refusals: countRefusals, reload: reloadCounts } = useSubscriberCounts(counted);

  const campaignsStory = story(feeds.campaigns.snapshot);
  const audiencesStory = story(feeds.audiences.snapshot);
  const rulesStory = story(feeds.rules.snapshot);
  const journeysStory = story(feeds.journeys.snapshot);

  const subscriptions = subscriptionsFact({ settled: answered(feeds.audiences.snapshot), error: audiencesStory.refusal, counts, audienceIds: subscribed });
  const active = activeAutomationsFact({
    count: activeCount,
    settled: answered(feeds.rules.snapshot) && answered(feeds.journeys.snapshot),
    error: rulesStory.refusal || journeysStory.refusal,
  });

  // THE FIGURES, and the one each row's chart draws.
  const [selected, setSelected] = useState<string>("sent");
  const [problem, setProblem] = useState<string>("bounces");
  const figures = statFigures(EMAIL_FIGURES, stats);
  const problems = statFigures(DELIVERY_FIGURES, stats);
  const refused = value === null && stats.state === "error" ? stats.error : "";

  // WHAT THE CLUSTER REFUSED THAT LEFT SOMETHING NOTHING TO SHOW: its sentence,
  // in the engine's own words, once. The statistics' stands in the chart's
  // place, under the figures it explains; the lists' stand under the facts.
  // A count refused for an audience counted only for its growth row (an
  // archived one) leaves that row's size out, and says nothing about the
  // Subscriptions figure: only the audiences that figure adds up are asked.
  const refusedCount = subscribed.map((id) => counts.get(id)).find((figure): figure is AbsentFigure => figure?.kind === "absent" && figure.reason === "failed");
  const refusalNotices = [
    { sentence: "This cluster did not return your campaigns.", detail: campaignsStory.refusal },
    { sentence: "This cluster did not return your subscriptions.", detail: audiencesStory.refusal || (refusedCount?.detail ?? "") },
    { sentence: "This cluster did not return your automations.", detail: rulesStory.refusal || journeysStory.refusal },
  ].filter((notice) => notice.detail !== "");

  // WHAT STOPPED ARRIVING WHILE THE LAST FIGURES ARE HELD: said once, on the
  // heading's line, with the engine's sentences on hover.
  const feedsKept = [campaignsStory.kept, audiencesStory.kept, rulesStory.kept, journeysStory.kept].filter((sentence) => sentence !== "");
  const statsKept = value !== null && stats.state === "error" ? [stats.error] : [];
  const countsKept = [...countRefusals].filter(([id]) => counts.get(id)?.kind === "measured").map(([, sentence]) => sentence);
  const kept = [...new Set([...statsKept, ...feedsKept, ...countsKept])];
  const behind = [feeds.campaigns, feeds.audiences, feeds.rules, feeds.journeys].filter((feed) => feedIsBehind(feed.snapshot.state));
  const showing = kept.length > 0 || behind.some((feed) => feed.snapshot.rows.length > 0);
  // Reconnecting is claimed only where it is true: the connection is what
  // stopped, and the SDK reconnects and reads again. A refusal is not retried
  // by anything but the next refresh.
  const reconnecting = behind.length > 0 && feedsKept.length === 0;
  const next = showing ? `Showing the last figures received.${reconnecting ? " Reconnecting automatically." : ""}` : "Reconnecting automatically.";
  const interrupted = kept.length > 0 || behind.length > 0;

  // FIRST USE is only ever said once the cluster has answered every list: a
  // cluster that could not say what it holds has not said it holds nothing.
  // A journey is an automation like a rule: a welcome series alone sends mail
  // the period counts, and "Nothing sent yet" beside it would contradict Sent.
  const everyListAnswered = [feeds.campaigns, feeds.audiences, feeds.rules, feeds.journeys].every((feed) => answered(feed.snapshot) && feed.snapshot.error === "");
  const firstUse = everyListAnswered && allCampaigns.length === 0 && allRules.length === 0 && allJourneys.length === 0;
  // The period's subjects: their shapes while the read is on its way, and drawn
  // once it answered for a period something counted.
  const subjects = !firstUse && (reading || measured);

  // THE LISTS' NAMES come from the feeds; a row waits for its feed's answer, and
  // one the feed does not hold is unavailable and opens nothing.
  const campaignNames = useMemo(() => new Map(allCampaigns.map((campaign) => [campaign.id, campaignName(campaign)])), [allCampaigns]);
  const audienceNames = useMemo(() => new Map(allAudiences.map((audience) => [audience.id, audienceName(audience)])), [allAudiences]);
  const automationNames = useMemo(
    () => new Map([...allRules.map((rule) => [`rule:${rule.id}`, ruleName(rule)] as const), ...allJourneys.map((journey) => [`journey:${journey.id}`, journeyName(journey)] as const)]),
    [allRules, allJourneys],
  );
  // An empty list is "nothing in this period" only where the read counted the
  // figures its rows carry; otherwise it could not list them (`listsMeasured`).
  const listed = value === null ? null : listsMeasured(value.totals);
  const emptyWords = (measuredList: boolean | undefined) => (measuredList === false ? NOT_MEASURED : NOTHING_IN_THE_PERIOD);
  const growth = value?.audiences ?? [];
  const growthReading = reading || (growth.length > 0 && !answered(feeds.audiences.snapshot));
  const top = value?.campaigns ?? [];
  const topReading = reading || (top.length > 0 && !answered(feeds.campaigns.snapshot));
  const automations = value?.automations ?? [];
  const automationsReading = reading || (automations.length > 0 && !(answered(feeds.rules.snapshot) && answered(feeds.journeys.snapshot)));

  const topRows: RankedRow[] = topReading ? [] : top.map((row) => {
    const name = campaignNames.get(row.id);
    return {
      key: row.id,
      label: name ?? "Unavailable campaign",
      value: row.uniqueOpens,
      secondary: campaignLine(row),
      onOpen: name === undefined ? undefined : () => openRecord("campaigns", { id: row.id }),
    };
  });

  // THE CLIENTS, under All clients only (spec 9): while there is a choice and
  // every client is in scope. A person who reaches one client has none to
  // compare, and a chosen client is one, whatever the read lists. A choice is
  // offered only once the names are known (the scope's own rule), so a row is
  // named as the client control names it, else as the account list does (an
  // archived organization is not among the choices).
  const clients = scope.showControl && scope.value === "" && value !== null && value.accounts.length > 1 ? value.accounts : [];
  const clientName = (id: string): string => {
    const option = scope.options.find((candidate) => candidate.value === id && candidate.value !== "");
    if (option !== undefined) return option.label;
    const account = accounts.accounts.find((candidate) => candidate.id === id);
    return account === undefined ? untitledAccountName(id) : accountName(account);
  };
  const clientRows: RankedRow[] = clients.map((row) => ({ key: row.id, label: clientName(row.id), value: row.sent, secondary: clientLine(row) }));

  const mainSpec = chosen(EMAIL_FIGURES, selected);
  const problemSpec = chosen(DELIVERY_FIGURES, problem);
  const delivered = value !== null && providerReportsOutcomes(value.totals) ? value.totals.delivered : null;

  return (
    <Overview
      // In first use there is no chart for a figure to choose, so none is a choice.
      figures={<StatFigures label="Email figures" figures={figures} selected={selected} onSelect={firstUse ? undefined : setSelected} />}
      actions={
        <>
          <ScopeControl id="email-overview-client" scope={scope} />
          <PeriodControl id="email-overview-period" value={period} periods={EMAIL_PERIODS} onChange={(next) => update({ statisticsPeriod: next })} />
        </>
      }
      note={interrupted ? <HeadNote sentence="Updates are interrupted." next={next} detail={kept.join(" ")} /> : undefined}
    >
      {refused !== "" ? (
        <Notice tone="error" sentence="This cluster did not return the email statistics." detail={refused} />
      ) : firstUse ? null : (
        <FigureChart spec={mainSpec} stats={value} reading={reading} />
      )}
      <Facts>
        <Fact label="Subscriptions" value={factValue(subscriptions, "Subscriptions")} />
        <Fact label="Active automations" value={factValue(active, "Active automations")} />
      </Facts>
      {refusalNotices.map((notice) => (
        <Notice key={notice.sentence} tone="error" sentence={notice.sentence} detail={notice.detail} />
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
      {subjects ? (
        <>
          <Subject title="Delivery problems">
            <StatFigures label="Delivery problem figures" figures={problems} selected={problem} onSelect={setProblem} />
            <FigureChart spec={problemSpec} stats={value} reading={reading} />
            {delivered !== null ? (
              <Facts>
                <Fact label="Delivered" value={<Measure figure={figureOf(delivered)} format={formatCount} />} />
              </Facts>
            ) : null}
          </Subject>
          <Subject title="Audience growth" list>
            {growthReading ? (
              <RecordListSkeleton label="Loading audience growth" rows={3} />
            ) : growth.length === 0 ? (
              <Caption>{emptyWords(listed?.audiences)}</Caption>
            ) : (
              <RecordList as="ul">
                {growth.map((row) => {
                  const name = audienceNames.get(row.id);
                  return (
                    <RecordRow
                      key={row.id}
                      icon={<Users size={16} aria-hidden />}
                      name={name ?? "Unavailable audience"}
                      secondary={<Parts line={growthLine(row, counts.get(row.id) ?? absent("unread"))} />}
                      onOpen={name === undefined ? undefined : () => openRecord("audiences", { id: row.id })}
                    />
                  );
                })}
              </RecordList>
            )}
          </Subject>
          <RankedList title="Top campaigns" rows={topRows} loading={topReading} empty={emptyWords(listed?.campaigns)} />
          <Subject title="Automations" list>
            {automationsReading ? (
              <RecordListSkeleton label="Loading automations" rows={3} />
            ) : automations.length === 0 ? (
              <Caption>{emptyWords(listed?.automations)}</Caption>
            ) : (
              <RecordList as="ul">
                {automations.map((row) => {
                  const name = automationNames.get(`${row.kind}:${row.id}`);
                  return (
                    <RecordRow
                      key={`${row.kind}:${row.id}`}
                      icon={row.kind === "journey" ? <Workflow size={16} aria-hidden /> : <Zap size={16} aria-hidden />}
                      name={name ?? "Unavailable automation"}
                      secondary={<Parts line={automationLine(row)} />}
                      onOpen={name === undefined ? undefined : () => openRecord("automations", { id: row.id })}
                    />
                  );
                })}
              </RecordList>
            )}
          </Subject>
        </>
      ) : null}
      {!firstUse && answered(feeds.campaigns.snapshot) && campaignsStory.refusal === "" ? (
        <OverviewBreakdown title="Campaigns" segments={campaignDistribution(campaigns)} />
      ) : null}
      {subjects && clients.length > 0 ? <RankedList title="Clients" rows={clientRows} /> : null}
      <PageHelp title="Overview">
        <p>A rate is the share of the campaign messages sent in the period that have been opened or clicked.</p>
        <p>Automations show no opens or clicks, because automated mail is not tracked for them.</p>
        <p>A recipient counts once per campaign, however often they open or click it.</p>
        <p>Mail privacy proxies open messages before their readers do, so opens are an upper bound.</p>
        <p>Days are counted in UTC.</p>
      </PageHelp>
      <AutoRefresh onRefresh={stats.reload} intervalMs={STATISTICS_REFRESH_MS} />
      <AutoRefresh onRefresh={reloadCounts} intervalMs={OVERVIEW_REFRESH_MS} />
    </Overview>
  );
}
