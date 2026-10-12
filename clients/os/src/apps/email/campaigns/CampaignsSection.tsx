import { useMemo, useState } from "react";
import { Send } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, EmptyState, Head, LiveList, Notice, RecordRow, Refine, Select, type RefineChip } from "../../../kit";
import { AddButton } from "../../../kit/AddButton";
import { listCount, type RecordTone } from "../../../kit/RecordRow";
import { flatten } from "../../../kit/rows";
import type { ArrivalKind } from "../../../live/arrival";
import { useLiveView } from "../../../live/liveView";
import type { CampaignWrites } from "../actions";
import { recordNamed, useOpenRequest, type OpenRequest } from "../openRequest";
import {
  audienceProjection,
  campaignFingerprint,
  campaignFromRow,
  campaignIsFinished,
  campaignName,
  nameOfAudience,
  useProjected,
  type CampaignRow,
} from "../rows";
import type { EmailFeeds, EmailRecordFeeds } from "../useEmail";
import { shortDay, shortMoment } from "../when";
import { WORDS } from "../words";
import { AddCampaignWizard } from "./AddCampaignWizard";
import { CampaignPage } from "./CampaignPage";

// CAMPAIGNS: every campaign, the page of one, and the wizard that adds one.
//
// THE LIST IS EVERY CAMPAIGN. A finished one keeps its row, dimmed, because the
// question "what went out" is asked of this list too; narrowing to what is in
// progress, sent, cancelled or failed is a question asked through Refine, not a
// preference that hides rows somewhere else (Ruling 5). A row says the campaign's
// name, who it goes to, the one time or figure that matters in its state, and its
// state in one word.
//
// The feed is LIVE (`v1:campaigns:campaign` broadcasts both verbs) and seeded
// unfiltered: every question here is a view over rows already in the browser,
// so asking one never re-reads, and the arrival cue starts again from whatever
// the question shows.
//
// A PAGE REPLACES THE LIST (DESIGN.md rule 11), and so does the wizard.

type StatusFacet = "" | "progress" | "sent" | "cancelled" | "failed";

const FACETS: readonly { value: StatusFacet; label: string }[] = [
  { value: "", label: "Any status" },
  { value: "progress", label: "In progress" },
  { value: "sent", label: "Sent" },
  { value: "cancelled", label: "Cancelled" },
  { value: "failed", label: "Failed" },
];

/** In progress is everything not yet finished: drafts, schedules, sends and pauses. */
function inFacet(campaign: CampaignRow, facet: StatusFacet): boolean {
  if (facet === "") return true;
  if (facet === "progress") return !campaignIsFinished(campaign);
  return campaign.status === facet;
}

const text = (row: Record<string, unknown>, key: string): string => (typeof row[key] === "string" ? (row[key] as string) : "");

export function CampaignsSection({
  feeds,
  recordFeeds,
  writes,
  trackByDefault,
  openRequest,
}: {
  feeds: EmailFeeds;
  recordFeeds: EmailRecordFeeds;
  writes: CampaignWrites;
  /** What Add a campaign's tracking switches start at. */
  trackByDefault: boolean;
  /** A campaign to open, or the Add flow to start, from search, Ask or another page. */
  openRequest?: OpenRequest;
}) {
  const [openId, setOpenId] = useState("");
  const [adding, setAdding] = useState(false);
  const [search, setSearch] = useState("");
  const [facet, setFacet] = useState<StatusFacet>("");
  useOpenRequest(openRequest, (target) => {
    if ("add" in target) {
      setOpenId("");
      setAdding(true);
    } else {
      setAdding(false);
      setOpenId(target.id);
    }
  });

  const audiences = useProjected(feeds.audiences.snapshot.rows, audienceProjection);
  const needle = search.trim().toLowerCase();

  // PROJECT, then narrow, in one pass: the collection holds raw wire rows. A
  // search reads the audience's name as well as the campaign's, so it is asked
  // again when an audience is renamed.
  const source = useLiveView<Row, CampaignRow>(
    feeds.campaigns.source,
    `${facet}|${needle}|${needle === "" ? "" : feeds.audiences.snapshot.version}`,
    (rows) =>
      rows
        .map(campaignFromRow)
        .filter(
          (c) =>
            c.id !== "" &&
            inFacet(c, facet) &&
            (needle === "" ||
              campaignName(c).toLowerCase().includes(needle) ||
              nameOfAudience(audiences, c.audienceId).toLowerCase().includes(needle)),
        ),
  );

  // The series a draft repeats from, by the draft's id: the app root reads them
  // for every draft in view, so a row reads "Repeating" without a read of its own.
  const seriesRows = recordFeeds.series.snapshot.rows;
  const seriesByCampaign = useMemo(() => {
    const held = new Map<string, Record<string, unknown>>();
    for (const row of seriesRows) {
      const series = flatten(row);
      held.set(text(series, "sourceCampaignId"), series);
    }
    return held;
  }, [seriesRows]);

  // A PAGE OPENS WHICHEVER CAMPAIGN ITS ID NAMES in the whole feed, whatever the
  // list is asking. One that has not arrived yet leaves the list on screen, and
  // the page opens when it does.
  const open = useMemo(
    () => recordNamed(feeds.campaigns.snapshot.rows, campaignFromRow, openId),
    [feeds.campaigns.snapshot.rows, openId],
  );

  if (adding) {
    return (
      <AddCampaignWizard
        feeds={feeds}
        recordFeeds={recordFeeds}
        trackByDefault={trackByDefault}
        onBack={() => setAdding(false)}
        onDone={(id) => {
          setAdding(false);
          setOpenId(id);
        }}
      />
    );
  }
  if (open) {
    return <CampaignPage campaign={open} feeds={feeds} recordFeeds={recordFeeds} writes={writes} onBack={() => setOpenId("")} />;
  }

  const failed = feeds.campaigns.snapshot.error !== "";
  const narrowed = facet !== "" || needle !== "";
  // A list the cluster says is empty has nothing to ask a question of (rule 2).
  // A question that emptied it stays, so it can be taken back.
  const settledEmpty = feeds.campaigns.snapshot.state === "live" && !failed && feeds.campaigns.snapshot.rows.length === 0;
  const chips: RefineChip[] =
    facet === "" ? [] : [{ id: "status", label: FACETS.find((f) => f.value === facet)?.label ?? facet, onRemove: () => setFacet("") }];

  return (
    <div className="os-app-stack">
      <Head title="Campaigns" meta={listCount(source?.snapshot)}>
        {settledEmpty && !narrowed ? null : (
          <Refine iconOnly search={search} onSearch={setSearch} chips={chips} label="Refine campaigns">
            <Select id="email-campaigns-status" label="Status" value={facet} onChange={(next) => setFacet(next as StatusFacet)}>
              {FACETS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </Select>
          </Refine>
        )}
        <AddButton label="Add a campaign" onClick={() => setAdding(true)} />
      </Head>

      {/* Keyed on the question, so asking another one RE-BASELINES the arrival
          cues: rows the browser already had are not the cluster sending them. */}
      <LiveList<CampaignRow>
        key={`${facet}|${needle}`}
        source={source}
        rowId={(c) => c.id}
        // THE COUNTERS ARE NOT NAMED HERE (campaignFingerprint): the drain worker
        // moves them on every batch, so ringing on one would strobe the list for
        // the length of every send. The row still fills live.
        fingerprint={campaignFingerprint}
        label="Campaigns"
        errorContent={
          <Notice tone="error" sentence="This cluster did not return your campaigns." next="Nothing below is current." detail={feeds.campaigns.snapshot.error}>
            <Button onClick={feeds.campaigns.reseed}>Try again</Button>
          </Notice>
        }
        emptyContent={
          failed ? null : narrowed ? (
            <EmptyState icon={Send} title="No matching campaigns">
              Try another name or status.
            </EmptyState>
          ) : (
            <EmptyState icon={Send} title="No campaigns yet">
              Add a campaign to send a template to an audience.
            </EmptyState>
          )
        }
        renderRow={(campaign, tick) => (
          <CampaignLine
            campaign={campaign}
            audience={nameOfAudience(audiences, campaign.audienceId)}
            series={seriesByCampaign.get(campaign.id) ?? null}
            tick={tick}
            onOpen={() => setOpenId(campaign.id)}
          />
        )}
      />
    </div>
  );
}

/** The one time or figure a campaign's state is about, or nothing. */
function summaryOf(campaign: CampaignRow, series: Record<string, unknown> | null): string {
  switch (campaign.status) {
    case "scheduled":
      return campaign.scheduledAt === "" ? "" : `Scheduled ${shortMoment(campaign.scheduledAt)}`;
    case "sending":
    case "paused":
      return campaign.recipientCount === 0
        ? ""
        : `${campaign.sentCount.toLocaleString("en-US")} of ${campaign.recipientCount.toLocaleString("en-US")} sent`;
    case "sent":
      return campaign.completedAt === "" ? "" : `Sent ${shortDay(campaign.completedAt)}`;
    case "draft":
      return series !== null && text(series, "status") === "active" && text(series, "nextAt") !== "" ? `Next ${shortMoment(text(series, "nextAt"))}` : "";
    default:
      return "";
  }
}

function CampaignLine({
  campaign,
  audience,
  series,
  tick,
  onOpen,
}: {
  campaign: CampaignRow;
  audience: string;
  series: Record<string, unknown> | null;
  tick: ArrivalKind | null;
  onOpen: () => void;
}) {
  const finished = campaignIsFinished(campaign);
  // A draft that a series sends from reads as the series does (Ruling 6).
  const seriesStatus = campaign.status === "draft" && series !== null ? text(series, "status") : "";
  const state = (seriesStatus === "" ? undefined : WORDS.repeating[seriesStatus]) ?? WORDS.campaign[campaign.status] ?? "";
  // Two states are worth colour: one that is happening, and one that went wrong.
  const tone: RecordTone =
    campaign.status === "sending" ? "accent" : campaign.status === "failed" || campaign.lastError !== "" || seriesStatus === "blocked" ? "warn" : "muted";
  const summary = summaryOf(campaign, series);
  return (
    <RecordRow
      icon={<Send size={16} aria-hidden />}
      name={campaignName(campaign)}
      secondary={audience}
      state={state}
      tone={tone}
      // A finished campaign keeps its facts and loses its ink.
      current={!finished}
      dim={finished}
      onOpen={onOpen}
      stateExtra={tick === "added" ? <span className="os-livelist-tick">new</span> : null}
    >
      {summary === "" ? null : <span className="os-caption">{summary}</span>}
    </RecordRow>
  );
}
