import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Fact, Facts, Head, Notice, Panel, Subhead } from "../../../kit";
import { ActionBar } from "../../../kit/ActionBar";
import { useOpenRecord } from "../openRequest";
import { audienceName, campaignName, type AudienceRow, type CampaignRow } from "../rows";
import { EMAIL_REFRESH_MS, type Reading } from "../useEmail";
import { browserZone, momentWords } from "../when";
import { WORDS } from "../words";
import { WhoGotIt } from "./Results";
import { SendBar } from "./SendBar";

// ONE TEST OF A CAMPAIGN, ON ITS OWN PAGE (memql#6033).
//
// A test is a send of its own -- its own audience, counters and ledger -- so it
// reads like one: what happened to its audience, and who got it. It REPLACES
// the campaign's page (rule 11), with the way back to it first in the trail.
// Nothing here changes anything, so its bar carries the state and no act.

/** "Test on Tue, Oct 14 at 10:00": a run is told apart from the others by when it went. */
export function testRunTitle(run: CampaignRow): string {
  return `Test on ${momentWords(run.startedAt || run.createdAt, browserZone())}`;
}

export function TestRunPage({ run, campaign, audiences, runs, onBack, onBackToList }: {
  run: CampaignRow;
  /** The campaign the test is of, whose page this replaced. */
  campaign: CampaignRow;
  audiences: readonly AudienceRow[];
  /** The campaign's test runs, which this page keeps fresh while it is seen. */
  runs: Reading<Row[]>;
  onBack: () => void;
  onBackToList: () => void;
}) {
  const openRecord = useOpenRecord();
  const title = testRunTitle(run);
  const name = campaignName(campaign);
  const audience = audiences.find((candidate) => candidate.id === run.audienceId);
  const zone = browserZone();
  const counted = run.sentCount + run.failedCount + run.skippedCount;
  const detail =
    run.status === "sending" && run.recipientCount > 0
      ? `${run.sentCount.toLocaleString("en-US")} of ${run.recipientCount.toLocaleString("en-US")} sent`
      : run.status === "sent" && run.completedAt !== ""
        ? momentWords(run.completedAt, zone)
        : "";

  return (
    <div className="os-action-pane" data-os-page-context={JSON.stringify({ page: "Campaign test", campaign: name, id: run.id })}>
      <div className="os-action-body os-app-stack">
        <Head
          title={title}
          breadcrumbs={[{ label: "Campaigns", onSelect: onBackToList }, { label: name, onSelect: onBack }, { label: title }]}
          back={{ label: name, onSelect: onBack }}
        />
        <AutoRefresh onRefresh={runs.reload} busy={runs.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
        {run.lastError === "" ? null : <Notice tone="warn" sentence="The test reported a problem." detail={run.lastError} />}
        {/* When it went is its title; what is left to say is who it went to. */}
        <Panel label="Test details">
          <Facts>
            <Fact
              label="Audience"
              value={
                audience ? (
                  <button type="button" className="os-link" onClick={() => openRecord("audiences", { id: audience.id })}>
                    {audienceName(audience)}
                  </button>
                ) : (
                  ""
                )
              }
            />
          </Facts>
        </Panel>
        <Panel label="Results">
          <Subhead>Results</Subhead>
          <SendBar campaign={run} />
        </Panel>
        <WhoGotIt campaignId={run.id} total={counted > 0 ? counted : null} />
      </div>
      <ActionBar state={WORDS.campaign[run.status] ?? ""} detail={detail} tone={run.status === "sending" ? "live" : "none"} live />
    </div>
  );
}
