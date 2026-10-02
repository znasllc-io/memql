import { useMemo, useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Field, Notice, Panel, RecordList, RecordRow, Select, Subhead, formatMoment } from "../../kit";
import { flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { useLiveCollection, type LiveCollectionHandle } from "../../live/useLiveCollection";
import { AccountPicker } from "../accounts/AccountPicker";
import { useAccountOptions } from "../accounts/tie";
import { useDefaultOrganization } from "../accounts/organization";
import { audienceFromRow, campaignFromRow, deliveryFromRow, type AudienceRow, type CampaignRow } from "./rows";
import { useCampaignDeliveries, useReading, useSendingReadiness } from "./useCampaigns";
import { useTestSend } from "./actions";

const concept = "v1:campaigns:testSettings";
const noRows: Row[] = [];
const text = (row: Record<string, unknown> | null, key: string) => typeof row?.[key] === "string" ? row[key] as string : "";
function useTestSettings(accountId: string) {
  return useLiveCollection<Row>(`campaigns:test-settings:${accountId}`, connection => ({
    concept,
    seed: async (_cursor, signal) => ({ rows: accountId ? (await connection.query.campaignTestSettings({ accountId }, { signal })).rows() : [], nextCursor: "" }),
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, concept, id, { signal })) as Row | null,
    inScope: row => text(flatten(row), "accountId") === accountId,
    paged: false,
  }));
}
const unavailable = (feed: LiveCollectionHandle<Row>) => feed.snapshot.state !== "live" || !!feed.snapshot.error;

export function TestAudienceSettings({ audiences }: { audiences: LiveCollectionHandle<Row> }) {
  const accounts = useAccountOptions();
  const defaultAccountId = useDefaultOrganization(accounts);
  const [chosen, setChosen] = useState("");
  const accountId = chosen || defaultAccountId;
  return <Panel label="Testing audience"><Subhead>Testing audience</Subhead>
    <Caption>Choose the internal audience that receives campaign tests for each organization. Manage its addresses in Audiences.</Caption>
    <Field label="Organization"><AccountPicker id="test-organization" label="Testing organization" required accounts={accounts} value={accountId} onChange={setChosen} /></Field>
    {accountId ? <TestAudienceChoice key={accountId} accountId={accountId} audiences={audiences} /> : null}
  </Panel>;
}

function TestAudienceChoice({ accountId, audiences }: { accountId: string; audiences: LiveCollectionHandle<Row> }) {
  const connection = useOsConnection();
  const feed = useTestSettings(accountId);
  const row = feed.snapshot.rows[0] ? flatten(feed.snapshot.rows[0]) : null;
  const saved = text(row, "audienceId");
  const [choice, setChoice] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const selected = choice ?? saved;
  const options = audiences.snapshot.rows.map(audienceFromRow).filter(a => a.accountId === accountId && a.status === "active");
  const blocked = unavailable(feed) || unavailable(audiences);
  const save = async () => {
    if (!connection || busy || blocked) return;
    setBusy(true); setError("");
    try {
      await connection.query.campaignConfigureTestAudience({ accountId, audienceId: selected, expectedRevision: text(row, "createdAt") || undefined });
      setChoice(null); feed.reseed();
    } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    finally { setBusy(false); }
  };
  return <>
    <Field label="Audience"><Select id={`test-audience-${accountId}`} label="Testing audience" value={selected} onChange={setChoice}>
      <option value="">None selected</option>
      {options.map(a => <option key={a.id} value={a.id}>{a.name}</option>)}
      {selected && !options.some(a => a.id === selected) ? <option value={selected}>Unavailable audience</option> : null}
    </Select></Field>
    <Button busy={busy} disabled={blocked || selected === saved} onClick={() => void save()}>Save testing audience</Button>
    <Caption>Saved in the cluster and shared with this organization. Use a separate audience from the campaign’s live subscribers.</Caption>
    {blocked ? <Notice tone="warn" sentence="Testing settings are not confirmed."><Button onClick={() => { feed.reseed(); audiences.reseed(); }}>Refresh</Button></Notice> : null}
    {error ? <Notice tone="error" sentence="Testing audience was not saved." detail={error} /> : null}
  </>;
}

export function TestSendPanel({ campaign, audiences }: { campaign: CampaignRow; audiences: AudienceRow[] }) {
  const testSend = useTestSend();
  const connection = useOsConnection();
  const settings = useTestSettings(campaign.accountId);
  const row = settings.snapshot.rows[0] ? flatten(settings.snapshot.rows[0]) : null;
  const audienceId = text(row, "audienceId");
  const audience = audiences.find(a => a.id === audienceId && a.accountId === campaign.accountId && a.status === "active");
  const sending = useSendingReadiness("", "", campaign.id);
  const read = useMemo(() => connection ? async (signal: AbortSignal) => (await connection.query.campaignTestRuns({ campaignId: campaign.id }, { signal })).rows() : null, [connection, campaign.id]);
  const history = useReading<Row[]>(noRows, read, [read]);
  const [expanded, setExpanded] = useState("");
  const ready = sending.ready && !unavailable(settings) && !!audience && audienceId !== campaign.audienceId;
  const send = async () => { if (ready && await testSend.send(campaign.id, audienceId)) history.reload(); };
  const runs = history.value.map(campaignFromRow);
  return <Panel label="Send a test"><Subhead>Send a test</Subhead>
    <Caption>{audience ? `Testing audience: ${audience.name}` : "Choose a testing audience for this organization in Settings."}</Caption>
    {audienceId === campaign.audienceId ? <Notice tone="warn" sentence="Choose a separate testing audience in Settings." /> : null}
    {unavailable(settings) ? <Notice tone="warn" sentence="Testing audience is not confirmed."><Button onClick={settings.reseed}>Refresh settings</Button></Notice> : null}
    {!sending.ready ? <Notice tone="warn" sentence="Finish sending setup before sending a test." detail={sending.reason}><Button onClick={sending.reload}>Check again</Button></Notice> : null}
    {sending.capture ? <Caption>View the test in the Email app. No email will reach the recipient’s inbox.</Caption> : null}
    <Button busy={testSend.busy} busyLabel="Queuing test" disabled={!ready} onClick={() => void send()}>Send test</Button>
    <Caption>Uses the normal sending process and a published template. Repeating campaigns use their saved settings. Results belong to the test; the live campaign and schedule stay unchanged.</Caption>
    {testSend.error ? <Notice tone="error" sentence="The test could not be confirmed." next="Retry to recover the same run without sending it twice." detail={testSend.error} /> : null}
    {testSend.runId ? <Notice tone="info" sentence="Test run queued. Check its delivery results below." /> : null}
    <div className="os-campaign-detail-head"><Subhead>Recent tests</Subhead><Button busy={history.state === "loading"} onClick={history.reload}>Refresh tests</Button></div>
    {history.error ? <Notice tone="error" sentence="Test results could not be read." detail={history.error} /> : null}
    <RecordList>{runs.map(run => <RecordRow key={run.id} name={formatMoment(run.startedAt || run.createdAt)} state={run.status} secondary={`${run.sentCount} accepted · ${run.skippedCount} skipped · ${run.failedCount} failed`} open={expanded === run.id} onOpen={() => setExpanded(expanded === run.id ? "" : run.id)}>
      {expanded === run.id ? <TestDeliveries run={run} /> : null}
    </RecordRow>)}</RecordList>
    {history.state === "ready" && !runs.length ? <Caption>No tests yet.</Caption> : null}
    {history.readAt ? <Caption>Latest 50 · Read {formatMoment(history.readAt)}. Accepted means the sending service accepted the message, not that it reached the inbox.</Caption> : null}
  </Panel>;
}
function TestDeliveries({ run }: { run: CampaignRow }) {
  const deliveries = useCampaignDeliveries(run.id);
  return <>
    {run.lastError ? <Notice tone="warn" sentence="This test needs attention." detail={run.lastError} /> : null}
    <Button busy={deliveries.state === "loading"} onClick={deliveries.reload}>Refresh deliveries</Button>
    {deliveries.error ? <Notice tone="error" sentence="Deliveries could not be read." detail={deliveries.error} /> : null}
    <RecordList>{deliveries.value.map(deliveryFromRow).map(delivery => <RecordRow key={delivery.id} name={delivery.email} state={delivery.status} secondary={delivery.lastError || delivery.skipReason} />)}</RecordList>
    <Caption>Read {formatMoment(deliveries.readAt)}</Caption>
  </>;
}
