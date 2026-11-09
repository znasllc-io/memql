import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useEffect, useState } from "react";
import { FileArchive } from "lucide-react";
import { Button, Caption, Notice } from "../../../kit";
import { RecordList, RecordRow } from "../../../kit/RecordRow";
import { formatMoment } from "../../../kit/format";
import { useOsConnection } from "../../../live/connection";
import { fetchSiteVersions, type SiteVersion } from "../packages/calls";
import type { DeploymentRow } from "../packages/rows";
import type { SiteRow } from "../rows";

/** What a version is to its deployable now. */
export type VersionState = "Current" | "Candidate" | "Available";

/** Whether the deployable serves a build baked into the installed MemQL, which has no version history of its own. */
export function bundledWithMemQL(site: SiteRow): boolean {
  return site.bundleRef.startsWith("file://");
}

/** A version's name: the last segment of its bundle, or that MemQL ships it. */
export function versionLabel(ref: string): string {
  return ref.startsWith("file://") ? "Bundled with MemQL" : ref.replace(/\/$/, "").split("/").pop() || ref;
}

/** What `ref` is to `site`, read from the row as it stands. */
export function versionState(site: SiteRow, ref: string): VersionState {
  return ref === site.bundleRef ? "Current" : site.kind !== "shopify_storefront" && ref === site.candidateRef ? "Candidate" : "Available";
}

/**
 * Whether the deployable may be rolled back to `version`: an uploaded build
 * (never one MemQL ships) that it has SERVED and is not serving now, on a live
 * or offline deployable that is not system-owned, for a person who may
 * publish. The act is ABSENT otherwise (DESIGN.md rule 12), never offered and
 * then refused.
 */
export function rollBackLegal({ site, version, served, canPublish }: {
  site: SiteRow;
  version: SiteVersion;
  served: boolean;
  canPublish: boolean;
}): boolean {
  return (
    canPublish &&
    !site.systemOwned &&
    version.bundleRef.startsWith("blob://") &&
    version.bundleRef !== site.bundleRef &&
    served &&
    (site.status === "live" || site.status === "disabled")
  );
}

/**
 * A version is a bundle, not a health observation or a change to site settings.
 *
 * A ROW OPENS THE VERSION'S PAGE (design record 2026-11-08, section 10). It
 * opened a dialog holding the facts and a Roll back button; the page that owns
 * the trail and the bar draws the version now, so this list only says which
 * one was chosen, what it is, and whether it ever served.
 */
export function Versions({ site, runs, onOpenVersion }: {
  site: SiteRow;
  runs: readonly DeploymentRow[];
  onOpenVersion: (version: SiteVersion, state: VersionState, served: boolean) => void;
}) {
  const connection = useOsConnection();
  const bundled = bundledWithMemQL(site);
  const [history, setHistory] = useState<SiteVersion[]>([]);
  const [reading, setReading] = useState(false);
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    let active = true;
    setHistory([]);
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
  // Every version above is one this deployable has SERVED.
  const served = new Set(versions.keys());
  // A CANDIDATE THAT NEVER SERVED IS A VERSION TOO (memql#5601). A deploy as
  // the candidate records candidateRef and never bundleRef, so neither the row
  // history nor the runs' served versions name it -- and the list would leave
  // out the newest version the deployable has. It is not one to roll back to:
  // it never served. Promoting it is what serves it.
  const candidate = site.kind !== "shopify_storefront" && !bundled ? site.candidateRef.trim() : "";
  if (candidate !== "" && !served.has(candidate)) {
    const madeBy = runs.find((run) => run.deployables.some((o) => (o.candidateRef ?? "") === candidate));
    add({ bundleRef: candidate, createdAt: madeBy ? madeBy.finishedAt || madeBy.createdAt : "", status: "", artifactId: "" });
  }

  return <>
    <RecordList as="ul" label="Versions">
      {[...versions.values()].map(version => {
        const state = versionState(site, version.bundleRef);
        return <RecordRow key={version.bundleRef}
          icon={<FileArchive size={18} aria-hidden />}
          name={versionLabel(version.bundleRef)}
          secondary={bundled ? "No separate version number is recorded" : version.createdAt ? formatMoment(version.createdAt) : undefined}
          state={state} tone={version.bundleRef === site.bundleRef ? "accent" : "muted"}
          current={version.bundleRef === site.bundleRef}
          onOpen={() => onOpenVersion(version, state, served.has(version.bundleRef))} label={`Version ${versionLabel(version.bundleRef)}, ${state.toLowerCase()}`}
        />;
      })}
    </RecordList>
    {versions.size === 0 && !reading && !error ? <Caption>No versions have been published yet.</Caption> : null}
    {reading ? <RecordListSkeleton label="Loading versions" /> : null}
    {error ? <Notice tone="error" sentence="Earlier versions could not be read." detail={error}><Button onClick={() => setRetry(value => value + 1)}>Try again</Button></Notice> : null}
  </>;
}
