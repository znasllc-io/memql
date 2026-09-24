import { useEffect, useState } from "react";
import { FileArchive } from "lucide-react";
import { Button, Caption, Fact, Facts, Notice } from "../../../kit";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { formatMoment } from "../../../kit/format";
import { useOsConnection } from "../../../live/connection";
import { fetchSiteVersions, type SiteVersion } from "../packages/calls";
import type { SiteLifecycleActions } from "../packages/actions";
import type { DeploymentRow } from "../packages/rows";
import type { SiteRow } from "../rows";
import { ProblemNotice } from "../packages/ReportView";
import { DetailDialog } from "./DetailDialog";

/** A version is a bundle, not a health observation or a change to site settings. */
export function Versions({ site, runs, canPublish, lifecycle }: {
  site: SiteRow;
  runs: readonly DeploymentRow[];
  canPublish: boolean;
  lifecycle: SiteLifecycleActions;
}) {
  const connection = useOsConnection();
  const bundled = site.bundleRef.startsWith("file://");
  const [history, setHistory] = useState<SiteVersion[]>([]);
  const [reading, setReading] = useState(false);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const [selected, setSelected] = useState<SiteVersion | null>(null);

  useEffect(() => {
    let active = true;
    setHistory([]);
    setSelected(null);
    setError("");
    setReading(false);
    // A baked path is mutable across engine releases. Row history cannot
    // identify or restore older contents at that same path.
    if (bundled) return;
    if (!connection) { setError("Not connected to the cluster."); return; }
    setReading(true);
    void fetchSiteVersions(connection.query, site.id).then(rows => {
      if (active) setHistory(rows);
    }).catch((cause: unknown) => {
      if (active) setError(cause instanceof Error ? cause.message : String(cause));
    }).finally(() => { if (active) setReading(false); });
    return () => { active = false; };
  }, [connection, site.id, site.bundleRef, bundled, retry]);

  const versions = new Map<string, SiteVersion>();
  const add = (version: SiteVersion) => {
    if (version.bundleRef.trim() && !versions.has(version.bundleRef)) versions.set(version.bundleRef, version);
  };
  add({ bundleRef: site.bundleRef, createdAt: "", status: site.status, artifactId: site.artifactId });
  if (!bundled) {
    for (const version of history) add(version);
    for (const run of runs) for (const outcome of run.deployables) {
      if (outcome.name !== site.packageDeployableName || outcome.refusal) continue;
      add({ bundleRef: outcome.bundleRef, createdAt: run.finishedAt || run.createdAt, status: "", artifactId: "" });
    }
  }
  const label = (ref: string) => ref.startsWith("file://") ? "Bundled with MemQL" : ref.replace(/\/$/, "").split("/").pop() || ref;
  const state = (ref: string) => ref === site.bundleRef ? "Current" : ref === site.candidateRef ? "Candidate" : "Available";

  return <>
    <RecordList as="ul" label="Versions">
      {[...versions.values()].map(version => <RecordRow key={version.bundleRef}
        icon={<FileArchive size={18} aria-hidden />}
        name={label(version.bundleRef)}
        secondary={bundled ? "No separate version number is recorded" : version.createdAt ? formatMoment(version.createdAt) : undefined}
        state={state(version.bundleRef)} tone={version.bundleRef === site.bundleRef ? "accent" : "muted"}
        current={version.bundleRef === site.bundleRef}
        onOpen={() => setSelected(version)} label={`Version ${label(version.bundleRef)}, ${state(version.bundleRef).toLowerCase()}`}
      />)}
    </RecordList>
    {versions.size === 0 && !reading && !error ? <Caption>No versions have been published yet.</Caption> : null}
    {reading ? <Caption>Reading versions…</Caption> : null}
    {error ? <Notice tone="error" sentence="Earlier versions could not be read." detail={error}><Button onClick={() => setRetry(value => value + 1)}>Try again</Button></Notice> : null}
    {selected ? <DetailDialog title={`Version ${label(selected.bundleRef)}`} onClose={() => setSelected(null)}>
      <Facts>
        <Fact label="Version" value={label(selected.bundleRef)} />
        <Fact label="State" value={state(selected.bundleRef)} />
        <Fact label="Bundle" value={selected.bundleRef} />
        {selected.createdAt ? <Fact label="Recorded" value={formatMoment(selected.createdAt)} /> : null}
      </Facts>
      {bundled ? <Caption>This app is included with the installed MemQL build. Earlier builds are managed with the cluster release.</Caption> : null}
      {canPublish && !site.systemOwned && selected.bundleRef.startsWith("blob://") && selected.bundleRef !== site.bundleRef && (site.status === "live" || site.status === "disabled") ?
        <Button busy={lifecycle.busy} onClick={() => void lifecycle.rollTo(site.id, selected.bundleRef)} ariaLabel={`Roll ${site.hostname} back to ${label(selected.bundleRef)}`}>Roll back to this version</Button> : null}
      {lifecycle.refusal ? <ProblemNotice problem={lifecycle.refusal} tone="error" /> : null}
    </DetailDialog> : null}
  </>;
}
