import { useId, useRef, useState, type ReactNode } from "react";
import { Mail, MessageSquare } from "lucide-react";
import { rowString, type Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Check, Field, Input, Notice, Panel, RecordList, RecordListSkeleton, RecordRow, Select, Subhead, TextArea } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { AddButton } from "../../../kit/AddButton";
import { flatten, stringsOf } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { usePaneActive } from "../paneActivity";
import { usePipelineChannels } from "./feeds";
import { channelWords as words } from "./words";
import { sameId, type PipelineRow } from "./rows";

export interface ChannelRow { id: string; name: string; kind: "discord" | "email"; secretRef: string; recipients: string[]; status: string }
export interface ChannelDraft { name: string; kind: "discord" | "email"; secretRef: string; recipients: string }
export function channelFromRow(raw: Row): ChannelRow {
  const row = flatten(raw);
  return { id: rowString(row, "id"), name: rowString(row, "name"), kind: rowString(row, "kind") === "discord" ? "discord" : "email",
    secretRef: rowString(row, "secretRef"), recipients: stringsOf(row, "recipients"), status: rowString(row, "status") };
}
const draftOf = (row: ChannelRow): ChannelDraft => ({ ...row, recipients: row.recipients.join("\n") });
const NEW_CHANNEL: ChannelDraft = { name: "", kind: "email", secretRef: "", recipients: "" };

export function PipelineChannelsSettings({ active, pipelines, pipelinesReady, refreshPipelines, header }: {
  header?: ReactNode; active: boolean; pipelines: readonly PipelineRow[]; pipelinesReady: boolean; refreshPipelines: () => void;
}) {
  const paneActive = usePaneActive();
  const feed = usePipelineChannels(active && paneActive);
  const connection = useOsConnection();
  const query = () => { if (!connection) throw new Error(words.disconnected); return connection.query; };
  return <ChannelsSettingsView header={header} channels={feed.snapshot.rows.map(channelFromRow)} ready={feed.snapshot.state === "live" && !feed.snapshot.error}
    readError={feed.snapshot.error || (feed.snapshot.state === "disconnected" ? words.disconnected : feed.snapshot.state === "degraded" ? words.unavailable : "")} pipelines={pipelines} pipelinesReady={pipelinesReady} onRetry={feed.reseed}
    onSave={async draft => { await query().pipelinesChannelSave({ name: draft.name, kind: draft.kind,
      secretRef: draft.kind === "discord" ? draft.secretRef : "", recipients: draft.kind === "email" ? draft.recipients.split(/\r?\n/).filter(line => line.trim() !== "") : [] }); feed.reseed(); }}
    onArchive={async name => { await query().pipelinesChannelArchive({ name }); feed.reseed(); }}
    onAllow={async (pipelineId, name, allowed) => { await query().pipelinesChannelAllow({ pipelineId, name, allowed }); refreshPipelines(); }} />;
}

export function ChannelsSettingsView({ header, channels, ready, readError, pipelines, pipelinesReady, onRetry, onSave, onArchive, onAllow }: {
  header?: ReactNode; channels: readonly ChannelRow[]; ready: boolean; readError: string; pipelines: readonly PipelineRow[]; pipelinesReady: boolean;
  onRetry: () => void; onSave: (draft: ChannelDraft) => Promise<void>; onArchive: (name: string) => Promise<void>;
  onAllow: (pipelineId: string, name: string, allowed: boolean) => Promise<void>;
}) {
  const id = useId();
  const [draft, setDraft] = useState<ChannelDraft | null>(null);
  const [selected, setSelected] = useState<ChannelRow | null>(null);
  const [busy, setBusy] = useState(false);
  const held = useRef(false);
  const [problem, setProblem] = useState("");
  const [askArchive, setAskArchive] = useState(false);
  // An arriving feed never replaces the person's unsaved fields.
  const current = selected ? channels.find(row => row.id === selected.id) ?? selected : null;
  async function act(operation: () => Promise<void>, done?: () => void) {
    if (held.current) return;
    held.current = true; setBusy(true); setProblem("");
    try { await operation(); done?.(); } catch (error) { setProblem(error instanceof Error ? error.message : String(error)); }
    finally { held.current = false; setBusy(false); }
  }
  function close() { setDraft(null); setSelected(null); setAskArchive(false); setProblem(""); }
  function open(row: ChannelRow | null) { setSelected(row); setDraft(row ? draftOf(row) : { ...NEW_CHANNEL }); setProblem(""); setAskArchive(false); }
  const acts: Act[] = askArchive ? [
    { label: words.keep, text: true, onAct: () => setAskArchive(false) },
    { label: words.archive, tone: "danger", onAct: () => { if (current) void act(() => onArchive(current.name), close); } },
  ] : [
    { label: words.cancel, text: true, onAct: close },
    ...(current?.status === "active" ? [{ label: words.archive, text: true, onAct: () => setAskArchive(true) }] : []),
    { label: current?.status === "archived" ? words.resume : words.save, tone: "primary", onAct: () => { if (draft) void act(() => onSave(draft), close); } },
  ];
  if (!draft) return <section className="os-deploy-scroll os-settings os-settings-wide deployable-settings" aria-label="Notification channels">
    {header}
    <Subhead actions={ready ? <AddButton label="Add notification channel" onClick={() => open(null)} /> : null}>Channels</Subhead>
    {!ready && channels.length === 0 && !readError ? <RecordListSkeleton label="Reading notification channels" /> : null}
    {readError ? <Notice tone="warn" sentence="Channels could not be read." detail={readError}><Button onClick={onRetry}>Try again</Button></Notice> : null}
    {ready && channels.length === 0 ? <p>No notification channels.</p> : null}
    <RecordList as="ul" label="Notification channels">{channels.map(row => <RecordRow key={row.id}
      icon={row.kind === "email" ? <Mail size={18} aria-hidden /> : <MessageSquare size={18} aria-hidden />}
      name={row.name} secondary={row.kind === "email" ? "Email" : "Discord"} state={row.status === "active" ? words.active : words.archived}
      onOpen={() => open(row)} label={`Manage channel ${row.name}`} />)}</RecordList>
  </section>;
  return <div className="os-deploy-pane pipeline-settings-page">
    <section className="os-deploy-scroll os-settings os-settings-wide deployable-settings" aria-label={current ? `Channel ${current.name}` : "New notification channel"}>
      {header}
      <Subhead>{current?.name ?? "New channel"}</Subhead>
      <Panel><fieldset disabled={busy} className="os-field-group os-form"><legend className="os-sr-only">{words.delivery}</legend>
        {!current ? <Field label="Name"><Input id={`${id}-name`} label="Channel name" value={draft.name} onChange={name => setDraft({ ...draft, name })} code /></Field> : null}
        <Field label="Delivery"><Select id={`${id}-kind`} label="Channel delivery" value={draft.kind} onChange={kind => setDraft({ ...draft, kind: kind as ChannelDraft["kind"] })}><option value="email">Email</option><option value="discord">Discord</option></Select></Field>
        {draft.kind === "discord" ? <Field label="Webhook secret"><Input id={`${id}-secret`} label="Webhook secret name" value={draft.secretRef} onChange={secretRef => setDraft({ ...draft, secretRef })} code /></Field>
          : <Field label="Recipients"><TextArea id={`${id}-recipients`} label="Recipients, one address per line" value={draft.recipients} onChange={recipients => setDraft({ ...draft, recipients })} rows={2} /></Field>}
      </fieldset></Panel>
      {current ? <Panel><Subhead>Pipelines</Subhead>
        {!pipelinesReady ? <Notice sentence="Pipeline permissions are unavailable." /> : pipelines.length === 0 ? <p>No connected pipelines.</p>
          : pipelines.map(pipeline => { const allowed = (pipeline.channelIds ?? []).some(value => sameId(value, current.id));
            return <Check key={pipeline.id} checked={allowed} disabled={busy || (!allowed && current.status !== "active")} onChange={next => { void act(() => onAllow(pipeline.id, current.name, next)); }}>{pipeline.repository || pipeline.name}</Check>; })}
      </Panel> : <p>Save the channel, then choose which pipelines may send to it.</p>}
      {problem ? <Notice tone="error" sentence={problem} /> : null}
    </section>
    <ActionBar scope={current?.id ?? "new-channel"} state={busy ? words.saving : askArchive ? words.archiveQuestion : current?.status === "archived" ? words.archived : current ? words.active : words.newChannel} tone={busy ? "busy" : "none"} acts={busy ? [] : acts}>
      {askArchive ? <span>{words.archiveDetail}</span> : null}
    </ActionBar>
  </div>;
}
