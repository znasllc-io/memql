import { useEffect, useState } from "react";
import { Concepts } from "@znasllc-io/memql-sdk-core/client";
import { ArrowLeft, ExternalLink, History, Sparkles } from "lucide-react";

import { Button, Chip, Chips, Head, Input, Panel, useLiveView } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { OpenLogsButton } from "../../../logs/OpenLogs";
import { useAccountOptions } from "../../accounts/tie";
import { usePackageActions, useSiteLifecycle } from "../packages/actions";
import { ProblemNotice } from "../packages/ReportView";
import type { Placement } from "../packages/calls";
import { deploymentFromRow, type DeploymentRow, type PackageRow } from "../packages/rows";
import { usePackageDeployments } from "../packages/usePackages";
import { liveUrlFor, ownerLabel, siteName, type SiteRow } from "../rows";
import type { CredentialRow } from "../sources/rows";
import { actsFor, type ActName } from "./acts";
import { Rail } from "./Rail";
import { openStopFor, refusalStopFor, type RailStage, type StandingInput } from "./rail";
import { BuildStop } from "./stops/Build";
import { LiveStop } from "./stops/Live";
import { SourceStop } from "./stops/Source";
import { WhatItIsStop } from "./stops/WhatItIs";
import { WhereItLivesStop } from "./stops/WhereItLives";
import { useBundleFlip } from "./useBundleFlip";

// The deployable page (epic memql#4937, design sections C and D): ONE head,
// ONE rail, ONE bar.
//
// ===========================================================================
// WHAT THIS PAGE USED TO BE
// ===========================================================================
// Measured on a live cluster: 5,069px over 5.9 viewports, TWO stacked heads
// (the list's and this one's), THIRTEEN rails at three different meanings, 36
// controls -- with Pause at y=2412, Archive at 2499, and "archive this source
// AND EVERY APP IT PRODUCED" at 885, three sections higher than either. Six
// controls read "Retry", carrying two different promises.
//
// Three moves fix all of it, and each is a rule rather than a tidy-up:
//
//   1. THE PAGE REPLACES THE LIST (DESIGN.md rule 11). One head.
//   2. A SETTLED STOP IS ONE LINE, and exactly one stop is open -- chosen by
//      what is actually a question (`openStopFor`). One rail.
//   3. ACTS FOLLOW THE STATE, IN ONE PLACE (rule 12). The bar is pinned to
//      the bottom edge, so nothing is ever scrolled to.
//
// The history is one line at the foot of the rail and belongs to the SOURCE,
// which is also what stops it being drawn twice for a two-app package.
//
// ===========================================================================
// THE TIMELINE IS RETAINED HERE, NEVER AT THE ROOT
// ===========================================================================
// The site, package and credential feeds are the app root's -- one per
// concept, for the life of the window. A package's deployment TIMELINE is
// retained by this page (clients/os/README.md): keeping every package's
// timeline live would subscribe the window to every deploy in the cluster to
// render one.

export interface DeployablePageProps {
  site: SiteRow;
  /** Its source when package-produced, else null: hand-made. */
  pkg: PackageRow | null;
  /** The caller's credential cards, from the root feed. */
  credentials: readonly CredentialRow[];
  viewerUserId: string;
  /** Rank >= 200; the app computes it once. */
  canWrite: boolean;
  isClusterOwner: boolean;
  clusterDomain: string;
  onAsk?: (tag: string) => void;
  /** The quiet Back to the list. */
  onBack: () => void;
  /** Opens the source's own view. */
  onOpenSource: (packageId: string) => void;
  /** Opens the source's history view. */
  onOpenHistory: (packageId: string) => void;
  /** True while this deployable's delete is still tearing its domains down. */
  deleting?: boolean;
  /** Told when a delete was accepted, so the section can start the teardown reading. */
  onDeleted?: (siteId: string) => void;
}

export function DeployablePage({
  site,
  pkg,
  credentials,
  viewerUserId,
  canWrite,
  isClusterOwner,
  clusterDomain,
  onAsk,
  onBack,
  onOpenSource,
  onOpenHistory,
  deleting = false,
  onDeleted,
}: DeployablePageProps) {
  const { source: timeline, reseed } = usePackageDeployments(pkg?.id ?? "");
  const deployments = useLiveView(timeline, `deployments:${pkg?.id ?? ""}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  const run = deployments?.snapshot.rows[0] ?? null;

  const accounts = useAccountOptions();
  const flipped = useBundleFlip(site);
  const headActions = usePackageActions();
  const lifecycle = useSiteLifecycle();
  const [zipOpen, setZipOpen] = useState(false);

  // THE OPEN STOP IS A READING, WITH AN OVERRIDE. `openStopFor` decides which
  // stop is the question; clicking another opens it instead, and clicking the
  // open one closes it. Cleared when the deployable changes, so a stop opened
  // on one is never carried onto another.
  const [openOverride, setOpenOverride] = useState<string | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  useEffect(() => {
    setOpenOverride(null);
    setConfirming(false);
    setTyped("");
  }, [site.id]);

  const rail: StandingInput = { mode: "standing", pkg, app: site.packageDeployableName, run, site };
  const openStop = openOverride ?? openStopFor(rail);
  const refusalStop = refusalStopFor(run);
  const name = siteName(site);
  const url = liveUrlFor(site.hostname);

  const reading = actsFor({ site, pkg, run, canWrite, deleting, releasing: site.hostname });

  function act(named: ActName) {
    switch (named) {
      case "Discard":
      case "Delete":
      case "Archive":
        setConfirming(true);
        return;
      case "Publish":
        void lifecycle.setStatus(site.id, "live");
        return;
      case "Unpublish":
        void lifecycle.setStatus(site.id, "disabled");
        return;
      case "Restore":
        void lifecycle.restore(site.id);
        return;
      case "Cancel":
        if (pkg !== null && run !== null) void headActions.cancel(pkg.id, run.id).then(reseed);
        return;
      case "Deploy":
      case "Deploy the update":
      case "Redeploy":
      case "Retry the deploy":
        // A PARKED RUN'S DEPLOY IS THE CONFIRMATION, not a new run -- but it
        // is still scoped to this page's app, or confirming from here would
        // deploy every sibling the source declares.
        if (pkg !== null && run !== null && run.status === "awaiting_confirm") {
          void headActions.deploy(pkg.id, true, onlyThisApp(pkg, site)).then(reseed);
          return;
        }
        if (pkg === null) {
          // A hand-made deployable's next version IS a zip, so the act opens
          // the picker rather than starting a run there is no source for.
          setOpenOverride("source");
          setZipOpen(true);
          return;
        }
        void headActions.deploy(pkg.id, false, onlyThisApp(pkg, site)).then(reseed);
        return;
      default:
        return;
    }
  }

  // Which destructive act the confirmation is for. Delete and Discard are one
  // capability under two names; Archive is its own.
  const confirmAct: ActName | null = confirming
    ? (reading.acts.find((a) => a.name === "Delete" || a.name === "Discard" || a.name === "Archive")?.name ?? null)
    : null;

  async function runConfirm() {
    if (confirmAct === "Archive") {
      await lifecycle.archive(site.id, typed);
      setConfirming(false);
      return;
    }
    const done = await lifecycle.remove(site.id, typed);
    if (done) {
      // DISCARDING A SOURCE-BACKED APP TURNS IT OFF, or it comes straight
      // back. The site row is gone, but the source still DECLARES the app --
      // so the list would show it again as "not deployed" and offer to deploy
      // the very thing somebody just discarded, which is what they saw.
      //
      // The site is deleted first and this second: the deletion is the act,
      // and a failure to record the preference must not leave a deployable
      // nobody asked to keep.
      if (pkg !== null && site.packageDeployableName !== "") {
        await headActions.setDeployableList(
          pkg.id,
          [...new Set([...pkg.disabledDeployables, site.packageDeployableName])],
        );
      }
      setConfirming(false);
      onDeleted?.(site.id);
    }
  }

  const busy = lifecycle.busy || headActions.busy;
  const acts: Act[] = reading.acts.map((spec) => ({
    label: spec.name,
    tone: spec.tone,
    busy,
    onAct: () => act(spec.name),
    ariaLabel: `${spec.name} ${name}`,
  }));

  const stopBody = (stage: RailStage) => {
    const refusal = refusalStop === stage.id && run?.error ? run.error : null;
    switch (stage.id) {
      case "source":
        return (
          <SourceStop
            site={site}
            pkg={pkg}
            credentials={credentials}
            canWrite={canWrite}
            flipped={flipped}
            zipOpen={zipOpen}
            onZipOpenChange={setZipOpen}
            refusal={refusal}
            onOpenSource={pkg === null ? undefined : () => onOpenSource(pkg.id)}
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
    <div className="os-deploy-pane">
      <div className="os-deploy-scroll">
        <Panel label={`Deployable ${name}`}>
          {/* ONE HEAD, and no primary action in it: every act is on the bar
              (rule 12). What stays is the quiet three -- where the thing is,
              its lines, and Ask -- none of which changes its state. */}
          <Head title={name}>
            <Button tone="quiet" onClick={onBack}>
              <ArrowLeft size={13} aria-hidden /> Deployables
            </Button>
            {url === "" ? null : (
              <a className="os-button" data-tone="quiet" href={url} target="_blank" rel="noreferrer noopener">
                <ExternalLink size={13} aria-hidden /> Open
              </a>
            )}
            <OpenLogsButton subject={site.id} subjectConcept={Concepts.PLATFORM_SITE} ariaLabel={`Logs for ${name}`} />
            {onAsk ? (
              <Button
                tone="quiet"
                onClick={() => onAsk(`app:deployables site:${site.hostname || site.id}`)}
                ariaLabel={`Ask about ${name}`}
              >
                <Sparkles size={13} aria-hidden /> Ask
              </Button>
            ) : null}
          </Head>

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

          {headActions.refusal ? (
            <ProblemNotice problem={{ ...headActions.refusal, fatal: true }} tone="error" />
          ) : null}

          <Rail
            input={rail}
            stopBody={stopBody}
            openStop={openStop}
            onOpenStop={(id) => setOpenOverride(id)}
            answerFor={(stage) => (stage.reason === "" ? "" : stage.reason)}
          />

          {/* THE HISTORY IS ONE LINE, AND IT IS THE SOURCE'S. Six attempts,
              each a full six-stop rail with its own refusal block, is 2,600px
              -- and `usePackageDeployments` reads the PACKAGE's timeline, so
              two apps of one source rendered the identical wall twice. */}
          {pkg === null ? null : (
            <button type="button" className="os-deploy-history-line" onClick={() => onOpenHistory(pkg.id)}>
              <History size={12} aria-hidden />
              <span>History &middot; {historySummary(deployments?.snapshot.rows ?? [])}</span>
              <span aria-hidden>&#9656;</span>
            </button>
          )}

          {/* A refusal renders IN SURFACE, beside the rail -- never a toast,
              and never inside a dialog that then closes, because a refusal
              nobody can re-read is a refusal nobody can act on. */}
          {lifecycle.refusal ? (
            <ProblemNotice problem={{ ...lifecycle.refusal, fatal: true }} tone="error" />
          ) : null}
        </Panel>
      </div>

      <ActionBar state={reading.state} detail={reading.detail} tone={reading.tone} acts={confirming ? [] : acts}>
        {confirming && confirmAct ? (
          <ConfirmRow
            site={site}
            act={confirmAct}
            typed={typed}
            onTyped={setTyped}
            busy={lifecycle.busy}
            onCancel={() => {
              setConfirming(false);
              setTyped("");
            }}
            onConfirm={() => void runConfirm()}
          />
        ) : null}
      </ActionBar>
    </div>
  );
}

/**
 * The typed confirmation, in the bar.
 *
 * IT SAYS WHAT IT COSTS BEFORE IT ASKS. A delete releases the hostname and
 * takes the deployable's domains down, and it is terminal -- archive is the
 * reversible step, one rung up. Saying that where the person is about to type
 * is the difference between a confirmation and a speed bump.
 */
function ConfirmRow({
  site,
  act,
  typed,
  onTyped,
  busy,
  onCancel,
  onConfirm,
}: {
  site: SiteRow;
  act: ActName;
  typed: string;
  onTyped: (v: string) => void;
  busy: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const removing = act === "Delete" || act === "Discard";
  return (
    <>
      <span className="os-actbar-confirm">
        {removing ? (
          <>
            Deleting releases <strong className="os-mono">{site.hostname}</strong> and takes its domains down. The
            record stays in this cluster&rsquo;s history, but the deployable cannot be brought back &mdash; restore it
            from Archived instead if you are not sure.
          </>
        ) : (
          <>Archiving keeps this deployable and its whole history, and it can be restored. Nothing is deleted.</>
        )}
      </span>
      <Input
        id={`os-confirm-${site.id}`}
        label={`Type ${site.hostname} to confirm`}
        value={typed}
        onChange={onTyped}
        placeholder={site.hostname}
      />
      <Button tone="quiet" onClick={onCancel}>
        Keep it
      </Button>
      <Button tone="danger" disabled={typed !== site.hostname} busy={busy} onClick={onConfirm}>
        {act}
      </Button>
    </>
  );
}

/**
 * Placements that redeploy THIS deployable and leave its siblings alone.
 *
 * THE SUBJECT OF THIS PAGE IS ONE APP. A deploy call with no placements is a
 * whole-PACKAGE run: the engine defaults every deployable to not-skipped, so
 * Redeploy on one app rebuilt and republished all of them. Worse, it FAILED
 * outright once a source declared an app that had never been deployed -- the
 * run refused with `deployable "web" has never been deployed and no hostname
 * was chosen for it`, on a page about a different app entirely, while the
 * site this page is about was serving perfectly.
 *
 * This used to be justified with "it needs no placements -- every app of an
 * existing deployable already has its address", which stopped being true the
 * moment a source recorded what it DECLARES as well as what it deployed.
 *
 * THIS app gets no entry: absent means not-skipped, and the engine republishes
 * through the (packageId, deployable name) key it recorded on the first
 * deploy, so its address is already remembered. Every OTHER declared app is
 * skipped BY NAME -- including ones the owner turned off, which the engine has
 * no other way to know about.
 */
function onlyThisApp(pkg: PackageRow, site: SiteRow): Record<string, Placement> {
  const mine = site.packageDeployableName;
  const out: Record<string, Placement> = {};
  for (const declared of pkg.declares) {
    if (declared.name === "" || declared.name === mine) continue;
    out[declared.name] = { hostname: "", accountId: "", ownDomain: "", skip: true };
  }
  return out;
}

/** The history line's own summary: how many, and how the last one went. */
function historySummary(rows: readonly DeploymentRow[]): string {
  if (rows.length === 0) return "no attempts yet";
  const attempts = `${rows.length} attempt${rows.length === 1 ? "" : "s"}`;
  const last = rows[0];
  if (last === undefined) return attempts;
  return `${attempts}, last ${statusWord(last.status)}`;
}

function statusWord(status: string): string {
  switch (status) {
    case "succeeded":
      return "live";
    case "abandoned":
      return "lost";
    case "awaiting_confirm":
      return "waiting for you";
    default:
      return status.replace(/_/g, " ");
  }
}

/**
 * The timeline, newest first.
 *
 * The collection folds events in the order the cluster sent them, so a page
 * that read `rows[0]` as the newest run would read a STALE one the moment a
 * new run arrived by event -- exactly when somebody is watching.
 */
function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}
