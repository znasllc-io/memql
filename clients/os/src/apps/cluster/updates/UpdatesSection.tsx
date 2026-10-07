import { useEffect, useRef, useState } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../../chrome/access";
import { useConnectionStatus } from "../../../chrome/connection";
import { useOsConnection } from "../../../live/connection";
import { Button, Caption, ContentSkeleton, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, Subhead } from "../../../kit";
import { RecordRow } from "../../../kit/RecordRow";
import { useReleaseRead } from "../releases/useReleaseRead";
import { detail, page, sources, title, type Release, type Source } from "./model";

export type UpdateQueries = Pick<QueryClient, "releaseSources" | "releaseDiscoverPublishedCandidates" | "releaseReadDiscoveredCandidate">;

export function UpdatesSection({ visible = true }: { visible?: boolean }) {
  const connection = useOsConnection(), status = useConnectionStatus();
  const { access } = useSession();
  if (!access || !["owner", "admin", "developer"].includes(access.role)) return <div className="os-cluster"><Head title="Updates" /><Notice sentence="A developer, admin or owner can check for updates." /></div>;
  return <UpdateBrowser key={access.userId} query={connection?.query ?? null} connected={status === "connected"} visible={visible} />;
}

export function UpdateBrowser({ query, connected, visible = true }: { query: UpdateQueries | null; connected: boolean; visible?: boolean }) {
  const [source, selectSource] = useState<Source | null>(null);
  const [selected, selectRelease] = useState<Release | null>(null);
  const [cursors, setCursors] = useState<string[]>([""]);
  const cursor = cursors[cursors.length - 1]!;
  const available = connected && query !== null;
  const publishers = useReleaseRead("sources", available ? async signal => sources(await query.releaseSources({}, { signal })) : null, visible && !source, query, 0);
  const releases = useReleaseRead(`page:${source?.id}:${cursor}`, available && source ? async signal => page(await query.releaseDiscoverPublishedCandidates({ sourceId: source.id, cursor, limit: 10 }, { signal }), source) : null, visible && !!source && !selected, query, 0);
  const release = useReleaseRead(`release:${source?.id}:${selected?.candidateId}:${selected?.catalogDigest}`, available && source && selected ? async signal => detail(await query.releaseReadDiscoveredCandidate({ sourceId: source.id, candidateId: selected.candidateId, catalogDigest: selected.catalogDigest }, { signal }), source, selected) : null, visible && !!selected, query, 0);
  const container = useRef<HTMLDivElement>(null), restore = useRef("");
  useEffect(() => {
    if (restore.current) {
      const row = Array.from(container.current?.querySelectorAll<HTMLElement>("[data-update-key]") ?? []).find(row => row.dataset.updateKey === restore.current);
      row?.querySelector<HTMLButtonElement>("button")?.focus();
      restore.current = "";
    }
  }, [source, selected]);
  const current = selected ? release : source ? releases : publishers;
  const back = selected ? { label: source!.publisher, onSelect: () => { restore.current = selected.candidateId; selectRelease(null); } } : source ? { label: "Updates", onSelect: () => { restore.current = source.id; selectSource(null); setCursors([""]); } } : undefined;
  return <div className="os-cluster" ref={container}>
    <Head title={selected ? "Release" : source ? source.publisher : "Updates"} back={back}>{available && !current.error ? <Button onClick={current.retry} busy={visible && !current.fresh}>Check again</Button> : null}</Head>
    {!available ? <Notice sentence="Disconnected. Update information will refresh when the cluster reconnects." /> : null}
    {current.error ? <Notice tone="error" sentence={current.error}><Button onClick={current.retry}>Try again</Button></Notice> : null}
    {current.value === null && available && !current.error ? selected ? <ContentSkeleton label="Verifying release" /> : <RecordListSkeleton label="Checking updates" /> : null}
    {!source ? <>
      {publishers.fresh && publishers.value?.length === 0 ? <Caption>No release publishers are configured.</Caption> : null}
      <RecordList as="ul" label="Release publishers">{publishers.value?.map(s => <li key={s.id} data-update-key={s.id}><RecordRow name={s.publisher} secondary={s.id === s.publisher ? undefined : s.id} onOpen={() => { selectSource(s); setCursors([""]); }} label={`Check updates from ${s.publisher}`} /></li>)}</RecordList>
    </> : !selected ? <>
      {releases.fresh && releases.value?.releases.length === 0 ? <Caption>No published releases are available on this page.</Caption> : null}
      <RecordList as="ul" label="Published releases">{releases.value?.releases.map(r => <li key={r.candidateId} data-update-key={r.candidateId}><RecordRow name={title(r)} state={releases.fresh ? "Verified" : undefined} onOpen={() => selectRelease(r)} label={`Inspect ${title(r)}`} /></li>)}</RecordList>
      {cursors.length > 1 || releases.value?.nextCursor ? <div className="os-panel-actions">
        {cursors.length > 1 ? <Button onClick={() => setCursors(old => old.slice(0, -1))}>Newer</Button> : null}
        {releases.fresh && releases.value?.nextCursor ? <Button onClick={() => setCursors(old => [...old, releases.value!.nextCursor!])}>Older</Button> : null}
      </div> : null}
    </> : release.value ? <>
      {release.value.components.map(c => <Panel key={c.name} label={c.name}><Subhead>{c.name}</Subhead><Facts><Fact label="Version" value={c.version} /><Fact label="Source" value={c.repository} /><Fact label="Commit" value={c.commit} /></Facts></Panel>)}
      <Caption>Installation requires a separate review of this cluster and its rollback plan.</Caption>
    </> : null}
  </div>;
}
