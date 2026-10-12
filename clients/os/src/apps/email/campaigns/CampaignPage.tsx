import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { newShortId, type Row } from "@znasllc-io/memql-sdk-core/client";

import { useSession } from "../../../chrome/access";
import { AutoRefresh, Button, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, Subhead, canConfigure, useNow } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { InlineSkeleton } from "../../../kit/ContentSkeleton";
import { absent } from "../../../kit/measure";
import { accessAdmits } from "../../../system/registry";
import { useAccountOptions } from "../../accounts/tie";
import { useDuplicateCampaign, useSendControls, useSeriesControls, useTestSend } from "../actions";
import { toKitActs, type ActSpec } from "../acts";
import { useOpenInbox, useOpenRecord, useOpenSettings } from "../openRequest";
import { isOperatorOrganization, organizationName } from "../organizations";
import {
  audienceIsArchived,
  audienceName,
  audienceProjection,
  campaignFromRow,
  campaignName,
  isPublished,
  senderLabel,
  senderProjection,
  sendBreakdown,
  seriesFor,
  templateName,
  templateProjection,
  testSettingFor,
  useProjected,
  type AudienceRow,
  type CampaignRow,
  type CampaignStats,
  type TestSettingRow,
} from "../rows";
import { EMAIL_SECTIONS } from "../settings";
import {
  EMAIL_REFRESH_MS,
  knowledgeOf,
  listKnowledge,
  useCampaignStats,
  useSendingReadiness,
  useSubscriberCounts,
  useTestRuns,
  type EmailFeeds,
  type EmailRecordFeeds,
  type Knowledge,
  type Reading,
} from "../useEmail";
import { browserZone, momentWords, shortMoment } from "../when";
import { WORDS } from "../words";
import { CampaignEditPage } from "./CampaignEditPage";
import { campaignActs, campaignReading, confirmSentence, type CampaignFacts } from "./campaignActs";
import { useLanding } from "./landing";
import { Breakdown, Results, WhoGotIt } from "./Results";
import { SendBar } from "./SendBar";
import { TestRunPage } from "./TestRunPage";

// ONE CAMPAIGN'S PAGE, which replaces the list it was opened from (DESIGN.md
// rule 11): its facts, what happened when it went out, its tests -- and ONE
// ActionBar that carries everything that changes it (rule 12). Nothing that
// sends, pauses, resumes, cancels or copies a campaign lives anywhere else.
//
// A SECTION APPEARS ONCE IT HAS SOMETHING TO SHOW. A draft opens on its facts
// alone: no band of nothing, no breakdown of zeros, no empty ledger. Results,
// the breakdown and who got it appear once a send has begun; tests once there
// is one.
//
// ITS EDIT PAGE AND A TEST'S PAGE REPLACE IT, and it stays mounted behind them,
// parked, so coming back finds it as it was -- its readings stop while it is
// out of sight, because a parked pane's AutoRefresh does.

/** The resource the Inbox tab is gated on: who may read what a capture cluster kept. */
const INBOX_REQUIRES = EMAIL_SECTIONS.find((section) => section.id === "inbox")?.requires;

/** A sentence's first letter in capitals: a fact or a line starts with it. */
const capitalized = (words: string) => words.charAt(0).toUpperCase() + words.slice(1);

type View = { kind: "page" } | { kind: "edit" } | { kind: "run"; id: string };

export function CampaignPage({ campaign, feeds, recordFeeds, onBack, onOpenCampaign }: {
  campaign: CampaignRow;
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  /** Back to the campaigns list. */
  onBack: () => void;
  /** Open another campaign's page once its row arrives: the copy Duplicate made. */
  onOpenCampaign: (campaignId: string) => void;
}) {
  const [view, setView] = useState<View>({ kind: "page" });
  // The tests are read here, once, for the page and a test's page alike.
  const runs = useTestRuns(campaign.id);
  const runRows = useMemo(() => runs.value.map(campaignFromRow).filter((run) => run.id !== ""), [runs.value]);
  const run = view.kind === "run" ? (runRows.find((candidate) => candidate.id === view.id) ?? null) : null;
  const series = seriesFor(recordFeeds.series.snapshot.rows, campaign.id);
  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  // Only a draft is edited: one that started while its Edit page was open goes back to its page.
  const editing = view.kind === "edit" && campaign.status === "draft";
  const toPage = () => setView({ kind: "page" });

  return (
    <>
      <div className="email-pane" hidden={editing || run !== null}>
        <CampaignView
          campaign={campaign}
          feeds={feeds}
          recordFeeds={recordFeeds}
          runs={runs}
          runRows={runRows}
          onBack={onBack}
          onEdit={() => setView({ kind: "edit" })}
          onOpenRun={(id) => setView({ kind: "run", id })}
          onOpenCampaign={onOpenCampaign}
        />
      </div>
      {editing ? <CampaignEditPage campaign={campaign} series={series} feeds={feeds} recordFeeds={recordFeeds} onBack={toPage} onBackToList={onBack} /> : null}
      {run === null ? null : <TestRunPage run={run} campaign={campaign} audiences={audiences} runs={runs} onBack={toPage} onBackToList={onBack} />}
    </>
  );
}

/** Where a test goes (Ruling 10): the organization's testing audience, else the person's own address, else nowhere. */
type TestTarget = { kind: "audience"; audience: AudienceRow } | { kind: "me"; address: string };

/**
 * The testing audience when one is chosen, is active and is not the audience
 * this campaign goes to -- a "test" to that would be the send. Otherwise the
 * person's own address. Nowhere while what decides it is still being read; and a
 * testing audience the cluster no longer confirms is never tested.
 */
function testTargetFor(
  campaign: CampaignRow,
  testing: Knowledge | null,
  setting: TestSettingRow | null,
  audiences: readonly AudienceRow[],
  audiencesKnown: Knowledge,
  address: string,
): TestTarget | null {
  if (campaign.accountId === "" || testing === null || testing === "reading") return null;
  const chosen = testing === "refused" ? "" : (setting?.audienceId ?? "");
  if (chosen !== "" && chosen !== campaign.audienceId) {
    const audience = audiences.find((a) => a.id === chosen && a.accountId === campaign.accountId && !audienceIsArchived(a));
    if (audience !== undefined) return { kind: "audience", audience };
    if (audiencesKnown === "reading") return null;
  }
  return address !== "" ? { kind: "me", address } : null;
}

/** How many delivery records exist: the server's count when it has one, else the campaign's own counters. */
function ledgerTotal(campaign: CampaignRow, stats: CampaignStats | null): number | null {
  if (stats !== null) {
    const parts = [stats.pending, stats.sent, stats.failed, stats.skipped];
    if (parts.every((part) => part.value !== null)) return parts.reduce((sum, part) => sum + (part.value ?? 0), 0);
  }
  const counted = campaign.sentCount + campaign.failedCount + campaign.skippedCount;
  return counted > 0 ? counted : null;
}

/** A write that changes what the campaign -- or the series it repeats from -- is. */
type Lifecycle = "send" | "schedule" | "pause" | "resume" | "cancel" | "series-pause" | "series-resume";

/** A write that did not go as asked, which says what its notice reads. */
type Refused = Lifecycle | "duplicate" | "test";

const onSeries = (act: Refused): boolean => act === "series-pause" || act === "series-resume";

const REFUSED: Record<Exclude<Refused, "test">, string> = {
  send: "The campaign wasn't sent.",
  schedule: "The campaign wasn't scheduled.",
  pause: "The send wasn't paused.",
  resume: "The send wasn't resumed.",
  cancel: "The campaign wasn't cancelled.",
  "series-pause": "Repeating wasn't paused.",
  "series-resume": "Repeating wasn't resumed.",
  duplicate: "The campaign wasn't duplicated.",
};

/** A write whose answer never came back: it may have been done, and the page cannot say it was not. */
const LOST: Record<Exclude<Refused, "test">, string> = {
  send: "Couldn't confirm the send.",
  schedule: "Couldn't confirm the schedule.",
  pause: "Couldn't confirm the pause.",
  resume: "Couldn't confirm the resume.",
  cancel: "Couldn't confirm that it was cancelled.",
  "series-pause": "Couldn't confirm that repeating paused.",
  "series-resume": "Couldn't confirm that repeating resumed.",
  duplicate: "Couldn't confirm the copy.",
};

/** The acts an ask turns the bar into, and the act each confirms. */
const ASKING: Record<"send" | "schedule" | "cancel", ActSpec[]> = {
  send: [
    { id: "not-yet", label: "Not yet", text: true },
    { id: "send-confirmed", label: "Send now", tone: "primary" },
  ],
  schedule: [
    { id: "not-yet", label: "Not yet", text: true },
    { id: "schedule-confirmed", label: "Schedule", tone: "primary" },
  ],
  cancel: [
    { id: "keep", label: "Keep", text: true },
    { id: "cancel-confirmed", label: "Cancel campaign", tone: "danger" },
  ],
};

function CampaignView({ campaign, feeds, recordFeeds, runs, runRows, onBack, onEdit, onOpenRun, onOpenCampaign }: {
  campaign: CampaignRow;
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  runs: Reading<Row[]>;
  runRows: CampaignRow[];
  onBack: () => void;
  onEdit: () => void;
  onOpenRun: (runId: string) => void;
  onOpenCampaign: (campaignId: string) => void;
}) {
  const { access } = useSession();
  const openRecord = useOpenRecord();
  const openSettings = useOpenSettings();
  const openInbox = useOpenInbox();
  const zone = browserZone();
  const now = useNow();
  const accounts = useAccountOptions();
  const organization = organizationName(accounts, campaign.accountId);

  // ---- what it names -------------------------------------------------------
  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const templates = useProjected(feeds.templates.snapshot.rows, templateProjection);
  const senders = useProjected(feeds.senders.snapshot.rows, senderProjection);
  const audience = audiences.find((a) => a.id === campaign.audienceId);
  const template = templates.find((t) => t.id === campaign.templateId);
  const sender = senders.find((s) => s.id === campaign.senderIdentityId);
  const audiencesKnown = listKnowledge(feeds.audiences);
  const templatesKnown = listKnowledge(feeds.templates);
  const series = seriesFor(recordFeeds.series.snapshot.rows, campaign.id);
  const seriesKnown = knowledgeOf(recordFeeds.series, campaign.id);

  // ---- what decides what it can do ----------------------------------------
  // One readiness read for the page, by the campaign: its organization and its sender.
  const sending = useSendingReadiness(campaign.accountId, campaign.senderIdentityId, campaign.id);
  const sendingAnswered = sending.state === "ready" || sending.state === "error";
  // Counted only while a send could still be asked for; a finished one went to whoever it went to.
  const counting = campaign.status === "draft" || campaign.status === "scheduled";
  const { counts, reload: recount } = useSubscriberCounts(counting ? [campaign.audienceId] : []);
  const subscribers = counts.get(campaign.audienceId) ?? absent("unread");

  // ---- what happened -------------------------------------------------------
  const started = !sendBreakdown(campaign).empty;
  const stats = useCampaignStats(started ? campaign.id : "");

  // ---- tests (Ruling 10) ---------------------------------------------------
  const testing = campaign.accountId === "" ? null : knowledgeOf(recordFeeds.testSettings, campaign.accountId);
  const setting = testSettingFor(recordFeeds.testSettings.snapshot.rows, campaign.accountId);
  const target = testTargetFor(campaign, testing?.knowledge ?? null, setting, audiences, audiencesKnown, access?.primaryEmail ?? "");
  // A run to the testing audience sends what a real send sends; a copy to an
  // address reads a draft template too. Either needs sending set up.
  const canTest = target !== null && sending.ready && (target.kind === "me" || (template !== undefined && isPublished(template)));

  const facts: CampaignFacts = {
    campaign,
    series,
    template,
    sendingReady: sending.ready,
    sendingReason: sending.reason,
    canTest,
    organizationName: organization,
    now,
    // Known once the cluster answered, either way: before that (still reading,
    // or nothing to read with) nothing is said about sending and nothing offered.
    sendingKnown: sendingAnswered,
    sendingFailed: sending.failed,
    templateKnown: templatesKnown === "known" || templatesKnown === "unconfirmed",
    seriesKnown: seriesKnown.knowledge === "known" || seriesKnown.knowledge === "unconfirmed",
    subscribers,
    audienceName: audience ? audienceName(audience) : "",
    zone,
  };
  const reading = campaignReading(facts);
  const specs = campaignActs(facts);

  // ---- the writes ----------------------------------------------------------
  const controls = useSendControls();
  const repeats = useSeriesControls();
  const copy = useDuplicateCampaign();
  const testSend = useTestSend();
  // The copy's id is held until a copy is made: Duplicate tried again is the same copy.
  const [copyId, setCopyId] = useState(newShortId);
  const [asking, setAsking] = useState<"" | "send" | "schedule" | "cancel">("");
  const [busyId, setBusyId] = useState("");
  const [tested, setTested] = useState<TestTarget | null>(null);
  /** Where the last test was going, so a refusal says what it means for that kind of test. */
  const [testedTo, setTestedTo] = useState<TestTarget["kind"]>("me");
  const working = useRef(false);

  // A question about an act that stopped being legal (the send started
  // somewhere else, the template went back to draft) is dropped, not left to
  // come back when the act does.
  const askingNow = asking !== "" && specs.some((spec) => spec.id === asking) ? asking : "";
  useEffect(() => {
    if (asking !== "" && askingNow === "") setAsking("");
  }, [asking, askingNow]);

  // ---- what a write moves --------------------------------------------------
  /** What a write moves, as it stands now: the series' revision, or the campaign's status. */
  const markerOf = (act: Refused): string => (onSeries(act) ? (series?.createdAt ?? "") : campaign.status);
  /** Read again what a write moves: that one series, or the campaigns. */
  const reread = (act: Refused) => (onSeries(act) ? recordFeeds.series.refresh(campaign.id) : feeds.campaigns.reseed());
  // A CHANGE ON ITS WAY HOLDS THE BAR until it lands (`useLanding`).
  const flight = useLanding<Lifecycle>({ marker: markerOf, reread });
  const landing = flight.landing;

  /**
   * The last write that did not go as asked: refused, in the engine's words, or
   * with its answer lost. It is about the state it was pressed in, so once that
   * moves -- the send started after all, somebody else cancelled it -- it says
   * nothing true any more and is not drawn.
   */
  const [outcome, setOutcome] = useState<{ act: Refused; lost: boolean; from: string } | null>(null);
  const missed = outcome !== null && markerOf(outcome.act) === outcome.from ? outcome : null;

  /** A write that did not go as asked; one whose answer was lost has its subject read again. */
  function note(act: Refused, from: string, lost: boolean) {
    setOutcome({ act, lost, from });
    if (lost && act !== "test" && act !== "duplicate") reread(act);
  }

  /** One write at a time: a second press while the first is on its way is the same press. */
  async function once(id: string, work: () => Promise<void>) {
    if (working.current) return;
    working.current = true;
    setBusyId(id);
    setOutcome(null);
    try {
      await work();
    } finally {
      working.current = false;
      setBusyId("");
    }
  }

  /**
   * A write that changes what the campaign or its series is. The act pressed
   * stays busy, alone in the bar, until what it moves has moved; a refusal or
   * a lost answer lets go at once and says which.
   */
  const lifecycle = (spec: ActSpec, act: Lifecycle, write: () => Promise<boolean>, lost: () => boolean) =>
    once(spec.id, async () => {
      setAsking("");
      const from = markerOf(act);
      flight.begin(act, spec);
      if (await write()) {
        flight.taken();
        // A series change is one series' row: read that one again, not every id.
        if (onSeries(act)) recordFeeds.series.refresh(campaign.id);
        return;
      }
      flight.drop();
      note(act, from, lost());
    });

  const sendTest = () =>
    once("test", async () => {
      if (target === null) return;
      setTested(null);
      setTestedTo(target.kind);
      const from = markerOf("test");
      const sent = target.kind === "audience" ? await testSend.send(campaign.id, target.audience.id) : await testSend.sendToMe(campaign.id, target.address);
      if (!sent) {
        note("test", from, testSend.lost());
        return;
      }
      setTested(target);
      void runs.reload();
    });

  const duplicate = () =>
    once("duplicate", async () => {
      const from = markerOf("duplicate");
      if (!(await copy.duplicate(campaign, copyId))) {
        note("duplicate", from, copy.lost());
        return;
      }
      onOpenCampaign(copyId);
      setCopyId(newShortId());
    });

  /** The act a bar press named, as the bar drew it. */
  const specOf = (id: string): ActSpec => barSpecs.find((spec) => spec.id === id) ?? { id, label: id };

  function run(id: string) {
    switch (id) {
      case "edit":
        onEdit();
        return;
      case "test":
        void sendTest();
        return;
      case "send":
      case "schedule":
      case "cancel":
        setOutcome(null);
        setAsking(id);
        return;
      case "not-yet":
      case "keep":
        setAsking("");
        return;
      case "send-confirmed":
        void lifecycle(specOf(id), "send", () => controls.start(campaign.id), controls.lost);
        return;
      case "schedule-confirmed":
        void lifecycle(specOf(id), "schedule", () => controls.schedule(campaign.id, campaign.scheduledAt), controls.lost);
        return;
      case "cancel-confirmed":
        void lifecycle(specOf(id), "cancel", () => controls.cancel(campaign.id), controls.lost);
        return;
      // A send in flight pauses and resumes itself; anything else that offers
      // them is a series, changed under the revision it was read at.
      case "pause":
        if (campaign.status === "sending") void lifecycle(specOf(id), "pause", () => controls.pause(campaign.id), controls.lost);
        else if (series !== null) {
          const revision = series.createdAt;
          void lifecycle(specOf(id), "series-pause", () => repeats.pause(campaign.id, revision), repeats.lost);
        }
        return;
      case "resume":
        if (campaign.status === "paused") void lifecycle(specOf(id), "resume", () => controls.resume(campaign.id), controls.lost);
        else if (series !== null) {
          const revision = series.createdAt;
          void lifecycle(specOf(id), "series-resume", () => repeats.resume(campaign.id, revision), repeats.lost);
        }
        return;
      case "duplicate":
        void duplicate();
        return;
    }
  }

  // A change on its way holds the bar: the act it was, busy, and nothing else.
  const barSpecs = landing !== null ? [landing.spec] : askingNow === "" ? specs : ASKING[askingNow];
  const question = landing !== null || askingNow === "" ? null : confirmSentence(askingNow, facts, subscribers);
  // Before anything goes out on a cluster that keeps mail rather than delivering it, the question says so.
  const detail = (askingNow === "send" || askingNow === "schedule") && sending.capture ? "This cluster keeps email in Inbox instead of delivering it." : reading.detail;
  const barState =
    landing === null
      ? { word: reading.word, detail, tone: reading.tone }
      : { word: WORDS.changing[landing.act === "series-pause" ? "pause" : landing.act === "series-resume" ? "resume" : landing.act], detail: "", tone: "busy" as const };

  // ---- what each part of the page says ---------------------------------------
  const mayConfigure = canConfigure(access?.role ?? "");
  // Sending matters where something still has to be sent: a draft with no
  // series, a schedule, a paused send.
  const sendingMatters = (campaign.status === "draft" && series === null) || campaign.status === "scheduled" || campaign.status === "paused";

  function outcomeNotice(): ReactNode {
    if (missed === null || missed.act === "test") return null;
    const detailOf: Record<Exclude<Refused, "test">, string> = {
      send: controls.error,
      schedule: controls.error,
      pause: controls.error,
      resume: controls.error,
      cancel: controls.error,
      "series-pause": repeats.error,
      "series-resume": repeats.error,
      duplicate: copy.error,
    };
    if (missed.lost) {
      // Not a refusal: it may have been done. The page reads it again and shows what it finds.
      const next = missed.act === "duplicate" ? "Try again: it makes the same copy, never a second one." : "Its status will update here.";
      return <Notice tone="warn" sentence={LOST[missed.act]} next={next} detail={detailOf[missed.act]} />;
    }
    return <Notice tone="error" sentence={REFUSED[missed.act]} detail={detailOf[missed.act]} />;
  }

  function pageNotices(): ReactNode {
    const notices: ReactNode[] = [];
    if (campaign.accountId === "") {
      notices.push(
        <Notice key="organization" tone="warn" sentence={campaign.status === "draft" ? "This campaign has no organization. Edit it to choose one." : "This campaign has no organization."} />,
      );
    } else if (sendingMatters && sendingAnswered) {
      if (sending.failed) {
        notices.push(<Notice key="sending" tone="warn" sentence={WORDS.sendingNotChecked(organization)} detail={sending.reason} />);
      } else if (!sending.ready) {
        // ONE SENTENCE FOR IT (Ruling 6), the engine's reason beneath, and the
        // way to fix it beside it. The bar does not say it again.
        notices.push(
          <Notice key="sending" tone="warn" sentence={WORDS.sendingNotSetUp(organization)} detail={sending.reason}>
            {mayConfigure ? (
              <p className="os-caption">
                <button type="button" className="os-link" onClick={() => openSettings(campaign.accountId)}>
                  Open Settings
                </button>
              </p>
            ) : (
              <p className="os-caption">An owner or developer sets it up.</p>
            )}
          </Notice>,
        );
      }
    }
    if (templatesKnown === "refused" && (campaign.status === "draft" || campaign.status === "scheduled")) {
      notices.push(
        <Notice key="templates" tone="error" sentence="This cluster did not return your templates." detail={feeds.templates.snapshot.error}>
          <Button onClick={feeds.templates.reseed}>Try again</Button>
        </Notice>,
      );
    }
    if (seriesKnown.knowledge === "refused") {
      notices.push(
        <Notice key="series" tone="error" sentence="This cluster did not return its repeating schedule." detail={seriesKnown.refusal}>
          <Button onClick={() => recordFeeds.series.refresh(campaign.id)}>Try again</Button>
        </Notice>,
      );
    }
    // The engine's reason, whole: the bar's detail is one line, and cuts a long one off.
    if (campaign.lastError !== "") {
      notices.push(
        campaign.status === "failed" ? (
          <Notice key="last-error" tone="error" sentence="The send failed." detail={campaign.lastError} />
        ) : (
          <Notice key="last-error" tone="warn" sentence="The send reported a problem." detail={campaign.lastError} />
        ),
      );
    }
    return notices;
  }

  /** A fact that names a record: a link to its page, a shape while it is read, an em dash when it is not there. */
  function recordFact(name: string | null, known: Knowledge, open: () => void): ReactNode {
    if (name !== null) {
      return (
        <button type="button" className="os-link" onClick={open}>
          {name}
        </button>
      );
    }
    return known === "reading" ? <InlineSkeleton label="Loading" /> : "";
  }

  // No sender is this cluster's own only for the operator organization; any other organization has to name one.
  const senderFact = campaign.senderIdentityId === "" ? (isOperatorOrganization(campaign.accountId) ? capitalized(WORDS.clusterSender) : "") : sender ? senderLabel(sender) : "";
  // A time still to come names its zone; one that has passed is when it went, on this clock.
  const tz = series?.timeZone || zone;
  // A draft that repeats has its bar say how often and when it goes next: the
  // facts do not say it again. Paused or stopped, nothing goes next, and the
  // facts are where its schedule is said.
  const barSaysTheSchedule = campaign.status === "draft" && series !== null && series.status === "active";
  const whenFact = series
    ? `${WORDS.every(series.intervalWeeks)} from ${momentWords(series.anchorAt, tz)} (${tz})`
    : campaign.startedAt !== ""
      ? momentWords(campaign.startedAt, zone)
      : campaign.scheduledAt !== ""
        ? `${momentWords(campaign.scheduledAt, zone)} (${zone})`
        : campaign.status === "draft"
          ? "Not scheduled"
          : "";

  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Campaign", name: campaignName(campaign), id: campaign.id })}>
      <div className="os-action-body os-app-stack">
        <Head title={campaignName(campaign)} back={{ label: "Campaigns", onSelect: onBack }} />
        {/* Kept current while the page is seen: an import, a signup or a domain
            set up somewhere else changes what can be sent. */}
        <AutoRefresh onRefresh={sending.reload} busy={sending.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
        {counting ? <AutoRefresh onRefresh={recount} intervalMs={EMAIL_REFRESH_MS} /> : null}
        {outcomeNotice()}
        {pageNotices()}

        <Panel label="Campaign details">
          <Facts>
            <Fact
              label="Audience"
              value={recordFact(audience ? audienceName(audience) : null, audiencesKnown, () => openRecord("audiences", { id: campaign.audienceId }))}
            />
            <Fact
              label="Template"
              value={recordFact(template ? templateName(template) : null, templatesKnown, () => openRecord("templates", { id: campaign.templateId }))}
            />
            <Fact label="Sender" value={senderFact} />
            <Fact label="Organization" value={organization} />
            {barSaysTheSchedule ? null : <Fact label="When" value={whenFact} />}
            {campaign.replyTo === "" ? null : <Fact label="Reply to" value={campaign.replyTo} />}
          </Facts>
        </Panel>

        {started ? (
          <>
            <AutoRefresh onRefresh={stats.reload} busy={stats.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
            <Results campaign={campaign} stats={stats} />
            <Breakdown campaign={campaign} stats={stats} />
            <WhoGotIt campaignId={campaign.id} total={ledgerTotal(campaign, stats.value)} />
          </>
        ) : null}

        <Tests
          runs={runs}
          runRows={runRows}
          audiences={audiences}
          tested={tested}
          refusedTest={missed?.act === "test" ? testedTo : null}
          testLost={missed?.act === "test" && missed.lost}
          testError={testSend.error}
          unresolved={testSend.unresolved}
          capture={sending.capture}
          testing={testing}
          testingChosen={setting !== null && setting.audienceId !== "" && setting.audienceId !== campaign.audienceId}
          audiencesKnown={audiencesKnown}
          audiencesError={feeds.audiences.snapshot.error}
          onRetryTesting={() => recordFeeds.testSettings.refresh(campaign.accountId)}
          onRetryAudiences={feeds.audiences.reseed}
          onOpenRun={onOpenRun}
          onOpenInbox={openInbox}
        />
      </div>

      <ActionBar
        state={barState.word}
        detail={barState.detail}
        tone={barState.tone}
        live
        acts={toKitActs(barSpecs, run, landing !== null ? landing.spec.id : busyId)}
      >
        {question === null ? null : <span className="os-actbar-confirm">{question}</span>}
      </ActionBar>
    </div>
  );
}

/**
 * The campaign's tests: what the latest did, every recent run (each opens on a
 * page of its own), and what the last test sent here came to. Drawn once there
 * is something to say.
 */
function Tests({
  runs,
  runRows,
  audiences,
  tested,
  refusedTest,
  testLost,
  testError,
  unresolved,
  capture,
  testing,
  testingChosen,
  audiencesKnown,
  audiencesError,
  onRetryTesting,
  onRetryAudiences,
  onOpenRun,
  onOpenInbox,
}: {
  runs: Reading<Row[]>;
  runRows: CampaignRow[];
  audiences: readonly AudienceRow[];
  tested: TestTarget | null;
  /** Where the refused test was going, when the last one was refused. */
  refusedTest: TestTarget["kind"] | null;
  /** The last test's answer never came back: it may have gone. */
  testLost: boolean;
  testError: string;
  unresolved: string[];
  capture: boolean;
  testing: { knowledge: Knowledge; refusal: string } | null;
  /** The organization names a testing audience other than this campaign's. */
  testingChosen: boolean;
  audiencesKnown: Knowledge;
  audiencesError: string;
  onRetryTesting: () => void;
  onRetryAudiences: () => void;
  onOpenRun: (runId: string) => void;
  onOpenInbox: () => void;
}) {
  const notices: ReactNode[] = [];
  if (refusedTest !== null) {
    notices.push(
      refusedTest === "audience" ? (
        <Notice key="refused" tone="error" sentence="The test wasn't confirmed." next="Send it again to pick up the same test. It won't go out twice." detail={testError} />
      ) : testLost ? (
        // A copy to an address has no request id to recover by: it may have gone, and trying again sends another.
        <Notice key="refused" tone="warn" sentence="Couldn't confirm the test was sent." detail={testError} />
      ) : (
        <Notice key="refused" tone="error" sentence="The test wasn't sent." detail={testError} />
      ),
    );
  } else if (tested !== null) {
    const missed = tested.kind === "me" ? unresolved : [];
    notices.push(
      <Notice key="tested" tone="info" sentence={tested.kind === "audience" ? `Test queued for ${audienceName(tested.audience)}.` : `Test sent to ${tested.address}.`}>
        {missed.length === 0 ? null : <p className="os-caption">{`These merge tags didn't resolve: ${missed.join(", ")}.`}</p>}
        {!capture ? null : accessAdmits(INBOX_REQUIRES) ? (
          <p className="os-caption">
            <button type="button" className="os-link" onClick={onOpenInbox}>
              View it in Inbox
            </button>
          </p>
        ) : (
          <p className="os-caption">An owner or developer can read it in Email → Inbox.</p>
        )}
      </Notice>,
    );
  }
  // Where a test goes cannot be read: said in the engine's words, never as a missing setting.
  if (testing?.knowledge === "refused") {
    notices.push(
      <Notice key="testing" tone="error" sentence="This cluster did not return the testing audience." detail={testing.refusal}>
        <Button onClick={onRetryTesting}>Try again</Button>
      </Notice>,
    );
  } else if (testingChosen && audiencesKnown === "refused") {
    notices.push(
      <Notice key="audiences" tone="error" sentence="This cluster did not return your audiences." detail={audiencesError}>
        <Button onClick={onRetryAudiences}>Try again</Button>
      </Notice>,
    );
  }

  const latest = runRows[0];
  const visible = notices.length > 0 || runRows.length > 0 || runs.state === "error";
  return (
    <>
      <AutoRefresh onRefresh={runs.reload} busy={runs.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
      {!visible ? null : (
        <Panel label="Tests">
          <Subhead>Tests</Subhead>
          {notices}
          {latest === undefined ? null : (
            <div role="group" aria-label="Test results">
              <SendBar campaign={latest} />
            </div>
          )}
          {runs.state === "error" ? <Notice tone="error" sentence="This cluster did not return the tests." detail={runs.error} /> : null}
          {runRows.length === 0 ? null : (
            <RecordList as="ul" label="Test runs">
              {runRows.map((testRun) => {
                const to = audiences.find((a) => a.id === testRun.audienceId);
                return (
                  <RecordRow
                    key={testRun.id}
                    name={shortMoment(testRun.startedAt || testRun.createdAt)}
                    secondary={to ? audienceName(to) : undefined}
                    state={WORDS.campaign[testRun.status] ?? ""}
                    tone={testRun.status === "sending" ? "accent" : testRun.status === "failed" || testRun.lastError !== "" ? "warn" : "muted"}
                    onOpen={() => onOpenRun(testRun.id)}
                  >
                    {/* The latest run's figures are the band's, just above: its row does not say them again. */}
                    {testRun === latest ? null : (
                      <span className="os-caption">{`${testRun.sentCount.toLocaleString("en-US")} sent, ${testRun.skippedCount.toLocaleString("en-US")} skipped, ${testRun.failedCount.toLocaleString("en-US")} failed`}</span>
                    )}
                  </RecordRow>
                );
              })}
            </RecordList>
          )}
        </Panel>
      )}
    </>
  );
}
