import type { ProposedRelease } from "./InstallationProposals";
import { InstallationUpdates } from "./InstallationUpdates";
import { LocalTabs } from "../../../kit/LocalTabs";
import { useContext, useEffect, useRef, useState, type ReactNode } from "react";
import type { QueryClient } from "@znasllc-io/memql-sdk-core/client";
import { useSession } from "../../../chrome/access";
import { useConnectionStatus } from "../../../chrome/connection";
import { useOsConnection } from "../../../live/connection";
import { Button, Caption, ContentSkeleton, Fact, Facts, Head, Notice, Panel, RecordList, RecordListSkeleton, Refine, Subhead } from "../../../kit";
import { RecordRow } from "../../../kit/RecordRow";
import { useReleaseRead } from "../releases/useReleaseRead";
import { useReleasePages } from "../releases/useReleasePages";
import { LoadMore } from "../../../kit/LoadMore";
import { LiveReadVisibility } from "../../../live/useGraphRevision";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../../logs/useDebouncedValue";
import { detail, page, sources, title, type Release, type Source } from "./model";

export type UpdateQueries = Pick<QueryClient, "releaseSources" | "releaseDiscoverPublishedCandidates" | "releaseReadDiscoveredCandidate">;

export function UpdatesSection({ visible = true, releaseSelection, clearReleaseSelection }: { visible?: boolean; releaseSelection?: ProposedRelease | null; clearReleaseSelection?: () => void }) {
  const connection = useOsConnection(), status = useConnectionStatus();
  const { access } = useSession();
  const [section, setSection] = useState("installation");
  const [proposedRelease, setProposedRelease] = useState<ProposedRelease | null>(null);
  useEffect(() => {
    if (releaseSelection) { setProposedRelease(releaseSelection); setSection("installation"); clearReleaseSelection?.(); }
  }, [releaseSelection, clearReleaseSelection]);
  const navigation = <LocalTabs label="Updates" value={section} onChange={setSection} options={[["installation", "This installation"], ["publishers", "Published releases"]]} />;
  if (!access || !["owner", "admin", "developer"].includes(access.role)) return <div className="os-cluster"><Head title="Updates" /><Notice sentence="A developer, admin or owner can check for updates." /></div>;
  return section === "installation" ? <div className="os-cluster"><Head title="Updates" />{navigation}<InstallationUpdates query={connection?.query ?? null} connected={status === "connected"} visible={visible} proposedRelease={proposedRelease} onClearProposedRelease={() => setProposedRelease(null)} /></div> : <UpdateBrowser key={access.userId} onPrepare={release => { setProposedRelease({ candidateId: release.candidateId, catalogDigest: release.catalogDigest, label: title(release) }); setSection("installation"); }} navigation={navigation} query={connection?.query ?? null} connected={status === "connected"} visible={visible} />;
}

type UpdateBrowserProps = { query: UpdateQueries | null; connected: boolean; visible?: boolean; navigation?: ReactNode; onPrepare?: (release: Release) => void };
export function UpdateBrowser(props: UpdateBrowserProps) {
  const parentVisible = useContext(LiveReadVisibility);
  return <LiveReadVisibility.Provider value={parentVisible && props.visible !== false && props.connected}><UpdateBrowserContent {...props} /></LiveReadVisibility.Provider>;
}
function UpdateBrowserContent({ query, connected, visible = true, navigation, onPrepare }: UpdateBrowserProps) {
  const [source, selectSource] = useState<Source | null>(null);
  const [selected, selectRelease] = useState<Release | null>(null);
  const [search, setSearch] = useState("");
  const needle = useDebouncedValue(search.trim(), TEXT_DEBOUNCE_MS);
  const available = connected && query !== null;
  const publishers = useReleaseRead("sources", available ? async signal => sources(await query.releaseSources({}, { signal })) : null, visible && !source, query, 0);
  const releases = useReleasePages(`published-releases:${source?.id}:${needle}`, query, query && source && !selected ? async (cursor, signal) => {
    const result = page(await query.releaseDiscoverPublishedCandidates({ sourceId: source.id, cursor, limit: 10, search: needle }, { signal }), source);
    return { rows: result.releases.map(r => ({ ...r, id: r.candidateId })), nextCursor: result.nextCursor ?? "" };
  } : null, visible && !!source && !selected, connected);
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
  const back = selected ? { label: source!.publisher, onSelect: () => { restore.current = selected.candidateId; selectRelease(null); } } : source ? { label: "Updates", onSelect: () => { restore.current = source.id; selectSource(null); setSearch(""); } } : undefined;
  return <div className="os-cluster" ref={container}>
    <Head navigationKey={`update:${source?.id ?? "sources"}:${selected?.candidateId ?? "releases"}`} title={selected ? "Release" : source ? source.publisher : "Updates"} back={back}>{available && !current.error ? <Button onClick={current.retry} busy={visible && !current.fresh}>Check again</Button> : null}</Head>
    {!source ? navigation : null}
    {source && !selected ? <Refine label="Search published releases" search={search} onSearch={setSearch} placeholder="Search component, version or repository" /> : null}
    {!available ? <Notice sentence="Disconnected. Update information will refresh when the cluster reconnects." /> : null}
    {current.error ? <Notice tone="error" sentence={current.error}><Button onClick={current.retry}>Try again</Button></Notice> : null}
    {current.value === null && available && !current.error ? selected ? <ContentSkeleton label="Verifying release" /> : <RecordListSkeleton label="Checking updates" /> : null}
    {!source ? <>
      {publishers.fresh && publishers.value?.length === 0 ? <Caption>No release publishers are configured.</Caption> : null}
      <RecordList as="ul" label="Release publishers">{publishers.value?.map(s => <li key={s.id} data-update-key={s.id}><RecordRow name={s.publisher} secondary={s.id === s.publisher ? undefined : s.id} onOpen={() => { selectSource(s); setSearch(""); }} label={`Check updates from ${s.publisher}`} /></li>)}</RecordList>
    </> : !selected ? <>
      {releases.fresh && releases.value?.length === 0 ? <Caption>No published releases are available on this page.</Caption> : null}
      <RecordList as="ul" label="Published releases">{releases.value?.map(r => <li key={r.candidateId} data-update-key={r.candidateId}><RecordRow name={title(r)} state={releases.fresh ? "Verified" : undefined} onOpen={() => selectRelease(r)} label={`Inspect ${title(r)}`} /></li>)}</RecordList>
      <LoadMore feed={releases} />
    </> : release.value ? <>
      {release.value.components.map(c => <Panel key={c.name} label={c.name}><Subhead>{c.name}</Subhead><Facts><Fact label="Version" value={c.version} /><Fact label="Source" value={c.repository} /><Fact label="Commit" value={c.commit} /></Facts></Panel>)}
      <Caption>Prepare the exact update proposal for this installation. After its changes are reviewed and landed, review the installation plan and choose when to apply it.</Caption>
      {onPrepare ? <Button tone="primary" disabled={!release.fresh} onClick={() => onPrepare(release.value!)}>Select for this installation</Button> : null}
    </> : null}
  </div>;
}
