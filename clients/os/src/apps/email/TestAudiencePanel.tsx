import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, Caption, InlineSkeleton, Notice, Panel, RecordList, RecordRow, Subhead, formatMoment } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { campaignFromRow, deliveryFromRow, testSettingFor, type AudienceRow, type CampaignRow } from "./rows";
import { EMAIL_REFRESH_MS, knowledgeOf, useCampaignDeliveries, useReading, type ComposedFeed, type SendingReadiness } from "./useEmail";
import { useTestSend } from "./actions";
import { SendBar } from "./SendBar";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";

const noRows: Row[] = [];

/**
 * What the engine says of a test for a campaign that has no organization
 * (component/campaigns/test_audience.go), said before anybody asks. The testing
 * audience is an organization's, and the app's feed is never asked about no
 * organization, so there is nothing to read and nothing to wait for.
 */
const NO_ORGANIZATION = "select an accessible original campaign with an organization";

// The Settings half of this file (choosing an organization's testing audience)
// is `settings/TestingPanel.tsx`. What stays is the campaign page's: the audience
// a test goes to, and its results.

/** `sending` is the campaign page's own reading, asked for once and handed to
 *  every panel that needs it. */
export function TestSendPanel({ campaign, audiences, testSettings: settings, sending }: {
  campaign: CampaignRow; audiences: AudienceRow[]; testSettings: ComposedFeed; sending: SendingReadiness;
}) {
  const testSend = useTestSend();
  const connection = useOsConnection();
  const setting = testSettingFor(settings.snapshot.rows, campaign.accountId);
  // READING IS NOT A FAULT: a skeleton while the organization's testing audience
  // is being read, a notice only when it was refused or cannot be confirmed.
  const { knowledge, refusal } = campaign.accountId === ""
    ? { knowledge: "refused" as const, refusal: NO_ORGANIZATION }
    : knowledgeOf(settings, campaign.accountId);
  const audienceId = setting?.audienceId ?? "";
  const audience = audiences.find(a => a.id === audienceId && a.accountId === campaign.accountId && a.status === "active");
  const read = useMemo(() => connection ? async (signal: AbortSignal) => (await connection.query.campaignTestRuns({ campaignId: campaign.id }, { signal })).rows() : null, [connection, campaign.id]);
  const history = useReading<Row[]>(noRows, read, [read]);
  const [expanded, setExpanded] = useState("");
  const ready = sending.ready && knowledge === "known" && !!audience && audienceId !== campaign.audienceId;
  const send = async () => { if (ready && await testSend.send(campaign.id, audienceId)) history.reload(); };
  const runs = history.value.map(campaignFromRow);
  return <Panel label="Send a test"><Subhead>Test results</Subhead>
    <AutoRefresh onRefresh={history.reload} busy={history.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
    {knowledge === "reading" ? <InlineSkeleton label="Loading the testing audience" />
      : audience ? <Caption>{`Testing audience: ${audience.name}`}</Caption>
      : knowledge === "known" ? <Caption>Choose a testing audience for this organization in Settings.</Caption> : null}
    {audienceId === campaign.audienceId ? <Notice tone="warn" sentence="Choose a separate testing audience in Settings." /> : null}
    {knowledge === "refused" ? <Notice tone="warn" sentence="Testing audience is not confirmed." detail={refusal} /> : null}
    {knowledge === "unconfirmed" ? <Notice tone="warn" sentence="Testing audience is not confirmed." next="Waiting for the connection to recover." /> : null}
    {!sending.ready ? <Notice tone="warn" sentence="Finish sending setup before sending a test." detail={sending.reason}><Button onClick={sending.reload}>Check again</Button></Notice> : null}
    {sending.capture ? <Caption>View the test in the Email app. No email will reach the recipient’s inbox.</Caption> : null}
    {runs[0] ? <div aria-label="Latest test results"><Subhead meta={formatMoment(runs[0].startedAt || runs[0].createdAt)}>Latest test</Subhead><SendBar campaign={runs[0]} stats={null} showEngagement={false} /></div> : null}
    <Button busy={testSend.busy} busyLabel="Queuing test" disabled={!ready} onClick={() => void send()}>Send test</Button>
    <Caption>Uses the normal sending process and a published template. Repeating campaigns use their saved settings. Results belong to the test; the live campaign and schedule stay unchanged.</Caption>
    {testSend.error ? <Notice tone="error" sentence="The test could not be confirmed." next="Retry to recover the same run without sending it twice." detail={testSend.error} /> : null}
    {testSend.runId && !runs.some(run => run.id === testSend.runId) ? <Notice tone="info" sentence="Test run queued. Check its delivery results below." /> : null}
    <Subhead>Recent tests</Subhead>
    {history.state === "loading" && !runs.length ? <RecordListSkeleton rows={2} label="Recent tests" /> : null}
    {history.error ? <Notice tone="error" sentence="Test results could not be read." detail={history.error} /> : null}
    <RecordList>{runs.map(run => <RecordRow key={run.id} name={formatMoment(run.startedAt || run.createdAt)} state={run.status} secondary={`${run.sentCount} accepted · ${run.skippedCount} skipped · ${run.failedCount} failed`} open={expanded === run.id} onOpen={() => setExpanded(expanded === run.id ? "" : run.id)}>
      {expanded === run.id ? <TestDeliveries run={run} /> : null}
    </RecordRow>)}</RecordList>
    {history.state === "ready" && !runs.length ? <Caption>No tests yet.</Caption> : null}
    {history.readAt ? <Caption>Latest 50 · Updates automatically. Accepted means the sending service accepted the message, not that it reached the inbox.</Caption> : null}
  </Panel>;
}
function TestDeliveries({ run }: { run: CampaignRow }) {
  const deliveries = useCampaignDeliveries(run.id);
  return <>
    <AutoRefresh onRefresh={deliveries.reload} busy={deliveries.state === "loading"} intervalMs={EMAIL_REFRESH_MS} />
    {run.lastError ? <Notice tone="warn" sentence="This test needs attention." detail={run.lastError} /> : null}
    {deliveries.state === "loading" && !deliveries.value.length ? <RecordListSkeleton rows={2} label="Test deliveries" /> : null}
    {deliveries.error ? <Notice tone="error" sentence="Deliveries could not be read." detail={deliveries.error} /> : null}
    <RecordList>{deliveries.value.map(deliveryFromRow).map(delivery => <RecordRow key={delivery.id} name={delivery.email} state={delivery.status} secondary={delivery.lastError || delivery.skipReason} />)}</RecordList>
    <Caption>Updates automatically</Caption>
  </>;
}
