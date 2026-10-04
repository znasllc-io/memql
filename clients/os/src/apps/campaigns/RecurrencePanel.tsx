import { useState } from "react";
import { getRowByConceptAndId, type Row } from "@znasllc-io/memql-sdk-core/client";
import { AttentionDestination, AttentionMarker } from "../../attention/Attention";
import { useLiveCollection, feedIsBehind } from "../../live/useLiveCollection";
import { Button, Caption, Chip, Field, Notice, Panel, Select, Subhead } from "../../kit";
import { flatten } from "../../kit/rows";
import { useConfigureSeries } from "./actions";

const concept = "v1:campaigns:campaignSeries";
const text = (row: Record<string, unknown>, key: string) => typeof row[key] === "string" ? row[key] as string : "";

export function RecurrencePanel({ campaignId }: { campaignId: string }) {
  const feed = useLiveCollection<Row>(`campaigns:series:${campaignId}`, connection => ({
    concept,
    seed: async (_cursor, signal) => ({ rows: (await connection.query.campaignSeriesForCampaign({ campaignId }, { signal })).rows(), nextCursor: "" }),
    reread: async (id, signal) => (await getRowByConceptAndId(connection.query, concept, id, { signal })) as Row | null,
    inScope: row => text(flatten(row), "sourceCampaignId") === campaignId,
    paged: false,
  }));
  const write = useConfigureSeries();
  const [editing, setEditing] = useState(false);
  const [weeks, setWeeks] = useState("2");
  const [first, setFirst] = useState("");
  const row = feed.snapshot.rows[0] ? flatten(feed.snapshot.rows[0]) : null;
  const zone = row ? text(row, "timeZone") : Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  const status = row ? text(row, "status") : "";
  const unavailable = feed.snapshot.state === "seeding" || feedIsBehind(feed.snapshot.state) || !!feed.snapshot.error;
  const busy = write.busy || unavailable;
  const act = async (action: "save" | "pause" | "resume") => {
    const saved = await write.call({ campaignId, action,
      expectedRevision: row ? text(row, "createdAt") : undefined,
      ...(action === "save" ? { intervalWeeks: Number(weeks), timeZone: zone, firstSendAt: row ? text(row, "anchorAt") : new Date(first).toISOString() } : {}),
    });
    if (saved) { setEditing(false); feed.reseed(); }
  };
  const date = row && text(row, "nextAt") ? new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short", timeZone: zone }).format(new Date(text(row, "nextAt"))) : "";
  return <Panel label="Repeating campaign">
    <div className="os-campaign-detail-head">
      <Subhead>Repeat</Subhead>
      {row ? <Chip>{status}</Chip> : !editing ? <Button disabled={busy} onClick={() => setEditing(true)}>
        Set up repeating sends<AttentionMarker appId="campaigns" sectionId="campaigns" target="recurring" />
      </Button> : null}
    </div>
    {feed.snapshot.error || feedIsBehind(feed.snapshot.state) ? <Notice tone="warn" sentence="The repeating schedule could not be confirmed."><Button onClick={feed.reseed}>Try again</Button></Notice> : null}
    {row || editing ? <AttentionDestination appId="campaigns" sectionId="campaigns" target="recurring">
      {row ? <>
        <Caption>Every {String(row.intervalWeeks)} weeks · {zone}{status === "active" ? ` · Next: ${date}` : ""}</Caption>
        {text(row, "lastError") ? <Notice tone="warn" sentence="Review this schedule before resuming." detail={text(row, "lastError")} /> : null}
        {!editing ? <div className="os-campaign-actions">
          <Button disabled={busy} onClick={() => { setWeeks(String(row.intervalWeeks)); setEditing(true); }}>Edit schedule</Button>
          <Button busy={write.busy} disabled={unavailable} onClick={() => void act(status === "active" ? "pause" : "resume")}>{status === "active" ? "Pause future sends" : "Resume future sends"}</Button>
        </div> : null}
      </> : null}
      {editing ? <div className="os-form">
        <Field label="Repeat every"><Select id={`repeat-weeks-${campaignId}`} label="Repeat interval" value={weeks} onChange={setWeeks}>
          {Array.from({ length: 52 }, (_, n) => n + 1).map(n => <option key={n} value={String(n)}>{n} {n === 1 ? "week" : "weeks"}</option>)}
        </Select></Field>
        {!row ? <Field label={`First send · ${zone}`}>
          <input className="os-input" type="datetime-local" aria-label="First recurring send" value={first} onChange={event => setFirst(event.target.value)} />
        </Field> : null}
        <Caption>Uses this campaign’s saved settings and the latest published template. Each send has its own history.</Caption>
        <div className="os-campaign-actions">
          <Button busy={write.busy} disabled={unavailable || (!row && (!first || !Number.isFinite(Date.parse(first))))} onClick={() => void act("save")}>{row ? "Save schedule" : "Start repeating schedule"}</Button>
          <Button disabled={write.busy} onClick={() => setEditing(false)}>Cancel</Button>
        </div>
      </div> : null}
      {row ? <Caption>Changes apply to future occurrences. Manage queued sends in their campaigns. Resume skips missed dates.</Caption> : null}
    </AttentionDestination> : null}
    {write.error ? <Notice tone="error" sentence="The schedule was not changed." detail={write.error} /> : null}
  </Panel>;
}
