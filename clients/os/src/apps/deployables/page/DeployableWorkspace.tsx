import { AvailableVersion } from "./AvailableVersion";
import { AutoDeploySwitch } from "./stops/Source";
import { useState } from "react";
import { healthExplanation } from "../health";
import { IconButton } from "../../../kit/IconButton";
import type { ReactNode } from "react";
import { AppWindow, Building2, ChevronRight, FileArchive, GitBranch, Globe, Hammer, History, Info, Link, Radio, Shapes, ShoppingBag, SlidersHorizontal, Activity } from "lucide-react";
import { Button, Caption, Notice, useLiveView } from "../../../kit";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { accountNameFrom, type AccountRow } from "../../accounts/rows";
import { domainFromRow } from "../domains";
import { shortVersion, sourceLabel, type DeploymentRow, type PackageRow } from "../packages/rows";
import { bundleForm, storefrontBinding, type SiteRow } from "../rows";
import { kindLabel } from "../targets";
import { useCustomDomains } from "../useCustomDomains";
import { siteIsBuilt, siteStateWord } from "../words";
import { railFor, type RailInput } from "./rail";

export type WorkspaceDetail = "source" | "whatItIs" | "whereItLives" | "build" | "live" | "runtime" | "traffic";

export function DeployableWorkspace({ site, pkg, run, accounts, canDomains, timelineState, timelineError, onRetryRead, onInspect, onOpenSource, onHistory, canSources = false, onUpdate }: {
  canSources?: boolean; onUpdate?: () => void;
  site: SiteRow; pkg: PackageRow | null; run: DeploymentRow | null; accounts: AccountRow[];
  canDomains: boolean; timelineState: string; timelineError: string; onRetryRead: () => void;
  onInspect: (detail: WorkspaceDetail) => void; onOpenSource: () => void; onHistory: () => void;
}) {
  const [modeOpen, setModeOpen] = useState(false);
  const built = siteIsBuilt(site);
  const state = site.status === "" ? "Unknown" : siteStateWord(site);
  const binding = site.kind === "shopify_storefront" ? storefrontBinding(site) : null;
  const input: RailInput = { mode: "standing", site, pkg, run, app: site.packageDeployableName };
  const build = railFor(input).stages.find(s => s.id === "build");
  const failed = run !== null && ["failed", "refused", "abandoned", "cancelled"].includes(run.status);
  const knownTimeline = timelineState === "live" || timelineState === "ready";
  return <>
    <div className="deployable-workspace-heading"><Caption>{kindLabel(site.kind)}</Caption></div>
    <section className="deployable-composition" aria-label="Source, deployable and connections">
      <ActivityTarget target={`deployables:${site.id}:source`} className="deployable-source-bank">
        <span className="deployable-piece-label">Source</span>
        <button className="deployable-source-piece" type="button" aria-label={pkg ? `Open ${sourceLabel(pkg)}` : "Source details"} onClick={pkg ? onOpenSource : () => onInspect("source")}>
          {pkg?.sourceKind === "repo" ? <GitBranch size={22} aria-hidden /> : <FileArchive size={22} aria-hidden />}
          <strong>{pkg ? sourceLabel(pkg) : site.packageId ? "Source unavailable" : "Built files"}</strong>
          {pkg?.sourceKind === "repo" ? null : <small>{pkg ? "Source ZIP from Files" : bundleForm(site.bundleRef).replace(/-/g, " ")}</small>}
          {pkg ? <small>{pkg.declares.length} declared app{pkg.declares.length === 1 ? "" : "s"}</small> : null}
          <ChevronRight className="deployable-piece-chevron" size={14} aria-hidden />
        </button>
      </ActivityTarget>
      <ActivityTarget target={`deployables:${site.id}:app`} className="deployable-object">

        <div className="deployable-object-title">{site.kind === "shopify_storefront" ? <ShoppingBag size={17} aria-hidden /> : <AppWindow size={17} aria-hidden />}<span>App configuration</span></div>
        <button type="button" className="deployable-piece-chip" onClick={() => onInspect("whatItIs")}><Shapes size={14} aria-hidden />Build settings</button>
        {binding ? <button type="button" className="deployable-piece-chip" onClick={() => onInspect("whatItIs")}><Link size={14} aria-hidden />{binding.storeDomain || "Shopify binding"}</button> : null}
        {pkg?.sourceKind === "repo" && !site.systemOwned ? <button type="button" className="deployable-piece-chip" onClick={() => setModeOpen(open => !open)} aria-expanded={modeOpen}><GitBranch size={14} aria-hidden />{pkg.autoDeploy ? "Automatic" : "Manual"} deployment</button> : null}
        {!site.systemOwned ? <button type="button" className="deployable-piece-chip" onClick={() => onInspect("runtime")}><SlidersHorizontal size={14} aria-hidden />App values <span>{Object.keys(site.settings).length}</span></button> : null}
      </ActivityTarget>
      <div className="deployable-connections">
        <ActivityTarget target={`deployables:${site.id}:address`}><Piece icon={<Globe size={18} aria-hidden />} label="Cluster address" detail={site.hostname || "No address recorded"} onClick={() => onInspect("whereItLives")} /></ActivityTarget>
        {canDomains ? <DomainPiece site={site} onClick={() => onInspect("whereItLives")} /> : null}
        <ActivityTarget target={`deployables:${site.id}:client`}><Piece icon={<Building2 size={18} aria-hidden />} label="Client" detail={accountNameFrom(accounts, site.accountId) || (site.accountId ? "Client name unavailable" : "The cluster")} onClick={() => onInspect("whereItLives")} /></ActivityTarget>
      </div>
    </section>
    {modeOpen && pkg?.sourceKind === "repo" ? canSources ? <AutoDeploySwitch pkg={pkg} /> : <Caption>{pkg.autoDeploy ? "Automatic" : "Manual"} deployment. Only someone with source access can change this mode.</Caption> : null}
    <section className="deployable-versions" aria-label="Serving version and latest attempt">
      <header><h3>Versions</h3><IconButton label={pkg ? "Deployment history" : "Version history"} onClick={onHistory}><History size={16} aria-hidden /></IconButton></header>
      {pkg ? <AvailableVersion key={pkg.id} pkg={pkg} onUpdate={onUpdate} /> : null}
      <div className="deployable-version-row"><span><Radio size={14} aria-hidden />{site.status === "live" ? state : "Stored version"}</span><div><strong className="os-mono">{built ? versionOf(site.bundleRef) : "No bundle yet"}</strong><small>{site.status === "live" ? healthExplanation(site) : built ? "Not published" : "Waiting for built files"}</small></div><IconButton label="Stored version details" onClick={() => onInspect("live")}><Info size={16} aria-hidden /></IconButton></div>
      {pkg ? <div className="deployable-version-row"><span><Hammer size={14} aria-hidden />Latest attempt</span><div><strong>{run ? attemptWord(run.status) : knownTimeline ? "No attempt yet" : "History unavailable"}</strong><small>{run ? [shortVersion(run.sourceVersion), build?.reason].filter(Boolean).join(" · ") : timelineState === "loading" || timelineState === "seeding" ? "Reading deployment history…" : "Deploy an update to start a new attempt"}</small></div>{run ? <IconButton label="Latest attempt details" onClick={() => onInspect(run.status === "awaiting_confirm" ? "whatItIs" : "build")}><Info size={16} aria-hidden /></IconButton> : null}</div> : null}
      {failed && site.status === "live" ? <Caption>The latest attempt did not replace the published version.</Caption> : null}
      {pkg && timelineError ? <Notice tone="error" sentence="Deployment history could not be read." detail={timelineError}><Button onClick={onRetryRead}>Try again</Button></Notice> : null}
    </section>
    <div className="deployable-reading-tools"><IconButton label="Traffic" onClick={() => onInspect("traffic")}><Activity size={16} aria-hidden /></IconButton></div>
  </>;
}

function Piece({ icon, label, detail, onClick }: { icon: ReactNode; label: string; detail: string; onClick: () => void }) {
  return <button type="button" className="deployable-slot" onClick={onClick}>{icon}<span><strong>{label}</strong><small>{detail}</small></span><ChevronRight size={13} aria-hidden /></button>;
}

function DomainPiece({ site, onClick }: { site: SiteRow; onClick: () => void }) {
  const { source } = useCustomDomains();
  const view = useLiveView(source, `workspace-domains:${site.id}`, rows => rows.map(domainFromRow).filter(d => d.siteId === site.id && d.status !== "removed"));
  const rows = view?.snapshot.rows ?? [];
  const ready = view?.snapshot.state === "live";
  const detail = view?.snapshot.error ? "Could not read domains" : rows.length ? rows.map(d => `${d.hostname}${d.status === "live" ? "" : ` · ${d.status.replace(/_/g, " ")}`}`).join(", ") : ready ? "Add a domain" : "Reading domains…";
  return <ActivityTarget target={`deployables:${site.id}:domains`}><Piece icon={<Link size={18} aria-hidden />} label="Custom domains" detail={detail} onClick={onClick} /></ActivityTarget>;
}

export function versionOf(ref: string): string { return ref.replace(/\/$/, "").split("/").pop() || ref; }
export function attemptWord(status: string): string {
  const words: Record<string, string> = { succeeded: "Finished", abandoned: "Lost", refused: "Refused", failed: "Failed", cancelled: "Cancelled", analyzing: "Analyzing", awaiting_confirm: "Waiting for review", building: "Building", staging_dsl: "Staging definitions", rolling: "Restarting cluster", publishing: "Putting files in place" };
  return words[status] ?? (status ? `Unknown state: ${status}` : "State unavailable");
}
