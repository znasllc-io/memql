import { useState } from "react";
import { ArrowLeft, ExternalLink, Sparkles } from "lucide-react";

import { Button, Chip, Chips, Head, Panel, useLiveView } from "../../../kit";
import { useAccountOptions } from "../../accounts/tie";
import { usePackageActions, useSiteLifecycle } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import { deploymentFromRow, type DeploymentRow, type PackageRow } from "../packages/rows";
import { usePackageDeployments } from "../packages/usePackages";
import { liveUrlFor, ownerLabel, siteName, type SiteRow } from "../rows";
import type { CredentialRow } from "../sources/rows";
import { EveryAttempt } from "./EveryAttempt";
import { headStateFor } from "./head";
import { Rail } from "./Rail";
import { headActionFor, refusalStopFor, type RailStage, type StandingInput } from "./rail";
import { BuildStop } from "./stops/Build";
import { LiveStop } from "./stops/Live";
import { SourceStop } from "./stops/Source";
import { WhatItIsStop } from "./stops/WhatItIs";
import { WhereItLivesStop } from "./stops/WhereItLives";
import { useBundleFlip } from "./useBundleFlip";

// The deployable page (epic memql#4885, design section A): one page for
// every deployable that has a site row, in its standing and deploy readings.
//
// ===========================================================================
// THE RAIL IS THE FORM, AND THE HEAD HAS ONE ACTION
// ===========================================================================
// Below the Head the rail reads the deployable's five stops off the rows --
// the package, the newest run, the site -- and each stop's BODY is mounted
// beneath its note through the rail's `stopBody` slot rather than drawn as a
// second panel beside it. The Head carries the deployable's name and ONE
// action that follows the state (`headStateFor` -> `headActionFor`), with
// the quiet Ask and Open beside it; nothing else stands. A refusal renders at
// the stop it belongs to, the OS headline above and the server's sentence
// beneath, never as a toast.
//
// ===========================================================================
// THE TIMELINE IS RETAINED HERE, NEVER AT THE ROOT
// ===========================================================================
// The site, package and credential feeds are the app root's -- one per
// concept, for the life of the window. A package's deployment TIMELINE is
// retained by this page (clients/os/README.md): keeping every package's
// timeline live would subscribe the window to every deploy in the cluster to
// render one. A hand-made deployable has no source and reads no timeline.
//
// ===========================================================================
// THE MODE IS A SEAM
// ===========================================================================
// `mode` names which reading the page is in. Today there is exactly one --
// `standing`, which also covers a deploy in flight, because during a run the
// same stops report progress (rail.ts) -- and the compose reading arrives
// with memql#4891 as a second arm of the same union. The page passes the
// mode's kind straight to the rail, so adding the arm is adding a reading,
// not a second page.

export type DeployablePageMode = { kind: "standing" };

const STANDING: DeployablePageMode = { kind: "standing" };

export interface DeployablePageProps {
  /** The deployable. */
  site: SiteRow;
  /** Its source when package-produced (joined by `site.packageId`), else null: hand-made. */
  pkg: PackageRow | null;
  /** The other sites the same package produced, for "archive this source and every app it produced". */
  siblings: readonly SiteRow[];
  /** The caller's credential cards, from the root feed. */
  credentials: readonly CredentialRow[];
  viewerUserId: string;
  /** Rank >= 200; the app computes it once. */
  canWrite: boolean;
  /** The client's own domain and the Domains content render only for one. */
  isClusterOwner: boolean;
  clusterDomain: string;
  onAsk?: (tag: string) => void;
  /** The quiet Back the section supplies when the page replaces the list; nothing renders for it when absent. */
  onBack?: () => void;
  /** Which reading the page is in. Standing when absent. */
  mode?: DeployablePageMode;
}

export function DeployablePage({
  site,
  pkg,
  siblings,
  credentials,
  viewerUserId,
  canWrite,
  isClusterOwner,
  clusterDomain,
  onAsk,
  onBack,
  mode = STANDING,
}: DeployablePageProps) {
  const { source: timeline, reseed } = usePackageDeployments(pkg?.id ?? "");
  const deployments = useLiveView(timeline, `deployments:${pkg?.id ?? ""}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  const run = deployments?.snapshot.rows[0] ?? null;

  const accounts = useAccountOptions();
  const flipped = useBundleFlip(site);
  // TWO WRITE HOOKS, because their refusals render in two places: a deploy
  // refused from the Head renders beneath it, and a status write refused
  // from the Head's Make it live renders on the Live stop, beside the other
  // status controls, which is where somebody looks for what happened to it.
  const headActions = usePackageActions();
  const lifecycle = useSiteLifecycle();
  // The hand-made Redeploy opens the Source stop's zip picker rather than
  // starting a run: a hand-made deployable's next version IS a zip.
  const [zipOpen, setZipOpen] = useState(false);

  const state = headStateFor({ site, pkg, run, canWrite });
  const action = state === null ? null : headActionFor(state);
  const refusalStop = refusalStopFor(run);
  const name = siteName(site);
  const url = liveUrlFor(site.hostname);

  const rail: StandingInput = { mode: mode.kind, pkg, app: site.packageDeployableName, run, site };

  function act() {
    if (state === null) return;
    switch (state.at) {
      case "awaiting_confirm":
        // Every app of an existing deployable already has an address, so the
        // confirm sends no placements: the pipeline republishes through the
        // (packageId, deployable name) key it recorded on the first deploy.
        if (pkg !== null) void headActions.deploy(pkg.id, true).then(reseed);
        return;
      case "draft_with_bundle":
        void lifecycle.setStatus(site.id, "live");
        return;
      case "live":
      case "refused_or_failed":
        if (pkg === null) {
          setZipOpen(true);
          return;
        }
        // Deploy the update, Redeploy and Retry are one call: a fresh run
        // that parks with its report, so the state flips to awaiting_confirm
        // and the Head reads Deploy.
        void headActions.deploy(pkg.id, false).then(reseed);
        return;
      default:
        return;
    }
  }

  const busy = state?.at === "draft_with_bundle" ? lifecycle.busy : headActions.busy;

  const stopBody = (stage: RailStage) => {
    // The newest run's refusal, at the stop it belongs to and nowhere else.
    const refusal = refusalStop === stage.id && run?.error ? run.error : null;
    switch (stage.id) {
      case "source":
        return (
          <SourceStop
            site={site}
            pkg={pkg}
            siblings={siblings}
            credentials={credentials}
            canWrite={canWrite}
            flipped={flipped}
            zipOpen={zipOpen}
            onZipOpenChange={setZipOpen}
            refusal={refusal}
          />
        );
      case "whatItIs":
        return <WhatItIsStop site={site} pkg={pkg} run={run} refusal={refusal} />;
      case "whereItLives":
        return (
          <WhereItLivesStop
            site={site}
            accounts={accounts}
            isClusterOwner={isClusterOwner}
            clusterDomain={clusterDomain}
          />
        );
      case "build":
        return <BuildStop run={run} app={site.packageDeployableName} refusal={refusal} />;
      case "live":
        return <LiveStop site={site} canWrite={canWrite} lifecycle={lifecycle} refusal={refusal} />;
      default:
        return null;
    }
  };

  return (
    <Panel label={`Deployable ${name}`}>
      <Head title={name}>
        {onBack ? (
          <Button tone="quiet" onClick={onBack}>
            <ArrowLeft size={13} aria-hidden /> Back
          </Button>
        ) : null}
        {url === "" ? null : (
          /* The single most useful link on this surface: where the thing
             actually is. `rel` is not decoration -- a new tab handed a live
             `window.opener` can navigate the shell it came from. */
          <a className="os-button" data-tone="quiet" href={url} target="_blank" rel="noreferrer noopener">
            <ExternalLink size={13} aria-hidden /> Open
          </a>
        )}
        {onAsk ? (
          <Button onClick={() => onAsk(`app:deployables site:${site.hostname || site.id}`)} ariaLabel={`Ask about ${name}`}>
            <Sparkles size={13} aria-hidden /> Ask
          </Button>
        ) : null}
        {action === null ? null : (
          <Button tone={action.tone} disabled={action.disabled} busy={busy} onClick={act}>
            {action.label}
          </Button>
        )}
      </Head>

      {/* WHOSE IT IS, and the two facts about a row that no stop carries. An
          empty ownerUserId is the meaningful CLUSTER-OWNED state -- the
          seeded portal row is the one the platform ships that way -- rather
          than a row that failed to record an owner. */}
      <Chips label="Deployable facts">
        <Chip tone={ownerLabel(site, viewerUserId) === "yours" ? "accent" : "muted"}>
          {ownerLabel(site, viewerUserId)}
        </Chip>
        {site.apiProxy ? (
          <Chip title="/_memql/* is mounted on this origin and forwarded to the bff, so the site is same-origin with its own API.">
            api proxy
          </Chip>
        ) : null}
        {site.systemOwned ? (
          <Chip title="Re-seeded at boot and refused at the delete path, so cluster management cannot be bricked by deleting it.">
            system-owned
          </Chip>
        ) : null}
      </Chips>

      {/* A deploy the engine refused before it opened a run has no row to
          carry it, so it renders here, beneath the action that asked. */}
      {headActions.refusal ? <ProblemNotice problem={{ ...headActions.refusal, fatal: true }} tone="error" /> : null}

      <Rail input={rail} stopBody={stopBody} />

      <EveryAttempt pkg={pkg} deployments={deployments} canWrite={canWrite} reseed={reseed} />
    </Panel>
  );
}

/**
 * The timeline, newest first.
 *
 * The collection folds events in the order the cluster sent them, so a page
 * that read `rows[0]` as the newest run would read a STALE one the moment a
 * new run arrived by event -- exactly when somebody is watching. Sorted here,
 * once, the way the map's layout sorts its own input.
 */
function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}
