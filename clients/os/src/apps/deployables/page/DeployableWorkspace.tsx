import { Versions } from "./Versions";
import type { SiteLifecycleActions } from "../packages/actions";
import { AvailableVersion } from "./AvailableVersion";
import { AutoDeploySwitch } from "./stops/Source";
import { useState } from "react";
import { IconButton } from "../../../kit/IconButton";
import type { ReactNode } from "react";
import { AppWindow, Building2, ChevronRight, FileArchive, GitBranch, Globe, Hammer, Info, Link, Shapes, ShoppingBag, SlidersHorizontal } from "lucide-react";
import { Button, Caption, Notice, useLiveView } from "../../../kit";
import { ActivityTarget } from "../../../kit/SemanticActivity";
import { accountNameFrom, type AccountRow } from "../../accounts/rows";
import { domainFromRow, isListedDomain } from "../domains";
import { shortVersion, sourceLabel, type DeploymentRow, type PackageRow } from "../packages/rows";
import { boundStoreId, bundleForm, type SiteRow } from "../rows";
import { kindLabel } from "../targets";
import { useCustomDomains } from "../useCustomDomains";
import { railFor, type RailInput } from "./rail";
import { storeLabel } from "../store/rows";
import { PreviewSection } from "../preview/PreviewSection";
import { NO_PARTS, type PartsHeld } from "../parts";
import { useStore } from "../store/useStore";

export type WorkspaceDetail = "source" | "whatItIs" | "whereItLives" | "build" | "runtime" | "traffic" | "store";

export function DeployableWorkspace({ site, pkg, run, runs, can, accounts, canDomains, canStore, timelineState, timelineError, onRetryRead, onInspect, onOpenSource, lifecycle, canSources = false, onUpdate }: {
  canSources?: boolean; onUpdate?: () => void;
  site: SiteRow; pkg: PackageRow | null; run: DeploymentRow | null; accounts: AccountRow[];
  /** This source's whole timeline, for the versions this deployable has published. */
  runs?: readonly DeploymentRow[];
  /** The parts this session holds, for the Preview section's own acts. */
  can?: PartsHeld;
  canDomains: boolean; canStore: boolean; timelineState: string; timelineError: string; onRetryRead: () => void;
  onInspect: (detail: WorkspaceDetail) => void; onOpenSource: () => void; lifecycle: SiteLifecycleActions;
}) {
  const [modeOpen, setModeOpen] = useState(false);
  const storefront = site.kind === "shopify_storefront";
  const storeId = storefront ? boundStoreId(site) : "";
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
        {pkg?.sourceKind === "repo" && !site.systemOwned ? <button type="button" className="deployable-piece-chip" onClick={() => setModeOpen(open => !open)} aria-expanded={modeOpen}><GitBranch size={14} aria-hidden />{pkg.autoDeploy ? "Automatic" : "Manual"} deployment</button> : null}
        {!site.systemOwned ? <button type="button" className="deployable-piece-chip" onClick={() => onInspect("runtime")}><SlidersHorizontal size={14} aria-hidden />App values <span>{Object.keys(site.settings).length}</span></button> : null}
      </ActivityTarget>
      <div className="deployable-connections">
        {/* THE STORE IS A CONNECTION, NOT A BUILD SETTING (epic memql#5530).
            It used to be a chip inside App configuration, pointing at the
            What-it-is stop -- which is the BUILD REPORT, and neither what a
            store is nor where it is configured. Every slot in this column
            answers one question, "what is this thing connected to", and a
            Shopify store is the most consequential answer a storefront has.

            ABSENT, NOT DISABLED, when the grants do not reach it
            (DESIGN.md rule 12). The store row is cluster-owner tier, so
            somebody without `execute app:deployables/store` would be shown a
            slot the engine then serves nothing into -- a refusal rendered as
            an empty panel. */}
        {storefront && canStore ? <StorePiece storeId={storeId} onClick={() => onInspect("store")} siteId={site.id} /> : null}
        <ActivityTarget target={`deployables:${site.id}:address`}><Piece icon={<Globe size={18} aria-hidden />} label="Cluster address" detail={site.hostname || "No address recorded"} onClick={() => onInspect("whereItLives")} /></ActivityTarget>
        {canDomains ? <DomainPiece site={site} onClick={() => onInspect("whereItLives")} /> : null}
        <ActivityTarget target={`deployables:${site.id}:client`}><Piece icon={<Building2 size={18} aria-hidden />} label="Client" detail={accountNameFrom(accounts, site.accountId) || (site.accountId ? "Client name unavailable" : "The cluster")} onClick={() => onInspect("whereItLives")} /></ActivityTarget>
      </div>
    </section>
    {modeOpen && pkg?.sourceKind === "repo" ? canSources ? <AutoDeploySwitch pkg={pkg} /> : <Caption>{pkg.autoDeploy ? "Automatic" : "Manual"} deployment. Only someone with source access can change this mode.</Caption> : null}
    <section className="deployable-versions" aria-label="Serving version and latest attempt">
      <header><h3>Versions</h3></header>
      {pkg ? <AvailableVersion key={pkg.id} pkg={pkg} onUpdate={onUpdate} /> : null}
      <Versions site={site} runs={runs ?? []} canPublish={can?.publish ?? false} lifecycle={lifecycle} />
      {pkg ? <div className="deployable-version-row"><span><Hammer size={14} aria-hidden />Latest attempt</span><div><strong>{run ? attemptWord(run.status) : knownTimeline ? "No attempt yet" : "History unavailable"}</strong><small>{run ? [shortVersion(run.sourceVersion), build?.reason].filter(Boolean).join(" · ") : timelineState === "loading" || timelineState === "seeding" ? "Reading deployment history…" : "Deploy an update to start a new attempt"}</small></div>{run ? <IconButton label="Latest attempt details" onClick={() => onInspect(run.status === "awaiting_confirm" ? "whatItIs" : "build")}><Info size={16} aria-hidden /></IconButton> : null}</div> : null}
      {failed && site.status === "live" ? <Caption>The latest attempt did not replace the published version.</Caption> : null}
      {pkg && timelineError ? <Notice tone="error" sentence="Deployment history could not be read." detail={timelineError}><Button onClick={onRetryRead}>Try again</Button></Notice> : null}
    </section>
    {/* PREVIEW SITS UNDER VERSIONS, because it is a reading of the same thing:
        Versions says what is serving, Preview says what is being exercised
        beside it. Two sections rather than one, because the second answers a
        question the first cannot -- which store each version talks to -- and
        folding them together would bury it.

        ABSENT FOR THE PLATFORM'S OWN SITE. MemQL OS is systemOwned and exempt
        from the preview axis as it is from the status and settings axes; the
        engine refuses those writes, and drawing the section on the console
        somebody is reading this in would be a panel of controls that only fail. */}
    {!site.systemOwned ? <PreviewSection site={site} runs={runs ?? []} can={can ?? NO_PARTS} onOpenStore={() => onInspect("store")} /> : null}
  </>;
}

function Piece({ icon, label, detail, onClick }: { icon: ReactNode; label: string; detail: string; onClick: () => void }) {
  return <button type="button" className="deployable-slot" onClick={onClick}>{icon}<span><strong>{label}</strong><small>{detail}</small></span><ChevronRight size={13} aria-hidden /></button>;
}

function DomainPiece({ site, onClick }: { site: SiteRow; onClick: () => void }) {
  const { source } = useCustomDomains();
  const view = useLiveView(source, `workspace-domains:${site.id}`, rows => rows.map(domainFromRow).filter(d => d.siteId === site.id && isListedDomain(d)));
  const rows = view?.snapshot.rows ?? [];
  const ready = view?.snapshot.state === "live";
  const detail = view?.snapshot.error ? "Could not read domains" : rows.length ? rows.map(d => `${d.hostname}${d.status === "live" ? "" : ` · ${d.status.replace(/_/g, " ")}`}`).join(", ") : ready ? "Add a domain" : "Reading domains…";
  return <ActivityTarget target={`deployables:${site.id}:domains`}><Piece icon={<Link size={18} aria-hidden />} label="Custom domains" detail={detail} onClick={onClick} /></ActivityTarget>;
}

/**
 * The Store slot, showing the store's own domain.
 *
 * IT READS THE STORE, and it can afford to: this workspace is drawn for ONE
 * deployable, not once per row in the list, so the read is one per page. The
 * domain is what somebody came to check -- an opaque row id under a slot
 * labelled "Store" tells them only that a binding exists, which they can see
 * from the slot being drawn at all.
 *
 * FOUR STATES AND THEY ARE DIFFERENT ANSWERS. Nothing bound is an invitation.
 * A read in flight says so. A store that reads back gets its domain and its
 * state. A store that does not read back is NOT drawn as unbound -- that
 * would hide a real misconfiguration behind a state that looks deliberate.
 */
function StorePiece({ storeId, siteId, onClick }: { storeId: string; siteId: string; onClick: () => void }) {
  const bound = useStore(storeId);
  const detail =
    storeId === ""
      ? "Attach a store"
      : bound.state === "failed"
        ? "The store could not be read"
        : bound.store !== null
          ? [storeLabel(bound.store), bound.store.status].filter((part) => part !== "").join(" \u00b7 ")
          : bound.state === "read"
            ? "Names a store that is not on this cluster"
            : "Reading the store";
  return <ActivityTarget target={`deployables:${siteId}:store`}><Piece icon={<ShoppingBag size={18} aria-hidden />} label="Store" detail={detail} onClick={onClick} /></ActivityTarget>;
}

export function attemptWord(status: string): string {
  const words: Record<string, string> = { succeeded: "Finished", abandoned: "Lost", refused: "Refused", failed: "Failed", cancelled: "Cancelled", analyzing: "Analyzing", awaiting_confirm: "Waiting for review", building: "Building", staging_dsl: "Staging definitions", rolling: "Restarting cluster", publishing: "Putting files in place" };
  return words[status] ?? (status ? `Unknown state: ${status}` : "State unavailable");
}
