import { useEffect, useMemo, useState } from "react";
import { ArrowLeft, Sparkles } from "lucide-react";

import { Button, Caption, Head, Notice, Panel, useLiveView } from "../../../kit";
import { ActionBar, type Act } from "../../../kit/ActionBar";
import { useAccountOptions } from "../../accounts/tie";
import { useCreateSite, usePublish, useSiteAccount } from "../actions";
import { useAddDomain } from "../domainActions";
import { hostnameFor } from "../hostname";
import { useNewPackage, usePackageActions } from "../packages/actions";
import { ProblemNotice, ReportView } from "../packages/ReportView";
import {
  deploymentFromRow,
  shortRepo,
  sourceLabel,
  type DeployableOutcome,
  type DeploymentRow,
  type PackageRow,
} from "../packages/rows";
import { usePackageDeployments } from "../packages/usePackages";
import { probeNote, probeParks, zipVerdict } from "../sources/probe";
import type { CredentialRow } from "../sources/rows";
import { useArtifactProbe, useSourceProbe } from "../sources/useProbes";
import { kindLabel, type StopId } from "../targets";
import {
  EMPTY_ADDRESS,
  EMPTY_DRAFT,
  appsToPlace,
  movingStopFor,
  pathOf,
  phaseOf,
  placementsComplete,
  placementsFrom,
  seedAddress,
  sourceReady,
  type AddressDraft,
  type ComposeDraft,
  type ComposePath,
  type ComposePhase,
} from "./compose";
import { runForApp } from "./acts";
import { Rail } from "./Rail";
import { headActionFor, type ComposeInput, type HeadAction, type RailProblem, type RailStage } from "./rail";
import { ManifestPreview } from "./stops/compose/ManifestPreview";
import { ComposeSourceStop } from "./stops/compose/Source";
import { ComposeWhereItLivesStop } from "./stops/compose/WhereItLives";

// The compose reading (epic memql#4885, design D4): THE RAIL IS THE FORM.
//
// ===========================================================================
// THE SAME FIVE STOPS, ANSWERED INSTEAD OF READ
// ===========================================================================
// New deployable opens this in place of the list: the Head's title becomes
// "New deployable", a quiet Back returns to the list, and beneath the Head
// the five stops the page reads as facts are rendered as INPUTS -- Source
// open first, the rest not reachable yet, the Head's ONE action following
// the state. There is no Next, no Back between stops and no step number: the
// order is a law the pipeline enforces, and the next unanswered stop is the
// open one.
//
// ===========================================================================
// TWO PATHS THROUGH ONE RAIL
// ===========================================================================
// A repository, and a zip whose root holds a manifest, are the PACKAGE path:
// Analyze creates the source and parks a run with its report, the report
// names the apps, and Deploy places them. A CI push, and a zip that is a
// built site, are the HAND-MADE path: there is nothing to analyze, so
// What-it-is is answered by the probe the moment the source is chosen,
// Analyze creates the draft site, and Deploy publishes the zip -- or, for a
// CI push, there is nothing left to do here and the Live stop waits.
//
// The address is asked for at Where it lives on BOTH paths (rule 7: a scope
// is named in one place). On the hand-made path that stop is open BEFORE
// Analyze, because `createSite` needs the hostname -- which is why the Head's
// `sourceComplete` there means "the flow has what the create needs" rather
// than "the source field is filled in". No server round trip separates the
// two on that path, so they are one answer.
//
// ===========================================================================
// WHERE THE FLOW IS COMES FROM THE ROWS, NEVER FROM A STEP COUNTER
// ===========================================================================
// A run advances by writing its own status on a node nobody in this browser
// is talking to, so the phase is a READING (`compose.ts`'s `phaseOf`) over
// the newest run and what this flow has created. That is also what makes
// closing the window and reopening from the list land in the same place: the
// parked run IS the state.

export interface ComposePageProps {
  clusterDomain: string;
  /** Rank >= 200; the app computes it once. */
  canWrite: boolean;
  /** A client's own domain and a CI-pushed source are cluster-owner acts. */
  isClusterOwner: boolean;
  viewerUserId: string;
  /** The caller's credential cards, from the root feed, for the Source stop's picker. */
  credentials: readonly CredentialRow[];
  /** The quiet Back: the list is what this replaced. */
  onBack: () => void;
  onAsk?: (tag: string) => void;
  /** A parked run and its source, when a "will serve" row reopened the reading. */
  parked?: { pkg: PackageRow; run: DeploymentRow };
  /**
   * Deploy ONLY this app, leaving every other one the source declares alone.
   *
   * Set when the flow was entered from an app the source merely DECLARES -- it
   * has no site and no run, so the analysis this opens is what produces its
   * confirm gate. Every other app is sent explicitly skipped; without that the
   * engine defaults them to not-skipped and one app's first deploy rebuilds
   * and republishes all of them.
   */
  only?: string;
  /**
   * The manifest apps of that source that ALREADY have a site, from the app
   * root's site feed. Their addresses were chosen on their first deploy and
   * the pipeline never re-reads them, so this stop does not re-ask.
   */
  placed?: readonly string[];
}

export function ComposePage(props: ComposePageProps) {
  const { clusterDomain, canWrite, isClusterOwner, credentials, onBack, onAsk, parked, placed, only } = props;

  const [draft, setDraft] = useState<ComposeDraft>(EMPTY_DRAFT);
  const [addresses, setAddresses] = useState<Record<string, AddressDraft>>({});
  // What this flow has CREATED. Held rather than derived, because it is the
  // one thing about the flow that is not a reading of a row this browser
  // already holds: the package and site feeds are the app root's, and the new
  // rows reach them on their own broadcasts rather than from here.
  const [created, setCreated] = useState({ packageId: "", siteId: "" });
  const [publishedZip, setPublishedZip] = useState(false);
  // "USE A TOKEN INSTEAD", HELD HERE, which is `ZipPicker`'s arrangement on
  // the standing Source stop and holds for the same reason: a fold whose
  // state lived in the stop would close under somebody every time a probe
  // answered or a credential arrived on its own feed.
  const [tokenFormOpen, setTokenFormOpen] = useState(false);

  const probe = useSourceProbe();
  const zipProbe = useArtifactProbe();
  const accounts = useAccountOptions();

  const newPackage = useNewPackage();
  const pkgActions = usePackageActions();
  const createSite = useCreateSite();
  const publish = usePublish();
  const tie = useSiteAccount();
  const addDomain = useAddDomain();

  const packageId = created.packageId || parked?.pkg.id || "";
  const { source: timeline, reseed } = usePackageDeployments(packageId);
  const deployments = useLiveView(timeline, `compose-deployments:${packageId}`, (rows) =>
    newestFirst(rows.map(deploymentFromRow).filter((d) => d.id !== "")),
  );
  // THE RUN THIS COMPOSE IS ABOUT (memql#4953). `rows[0]` is the source's
  // newest run whatever app it names, and this flow is entered FOR one when
  // `only` is set -- so composing `web` while `storefront` deploys used to
  // read the sibling's run as the gate it was waiting on. Unscoped compose (a
  // brand-new source) still takes the newest, which is right: there is one app
  // and one run.
  const timelineRows = deployments?.snapshot.rows ?? [];
  const run =
    (only !== undefined && only !== ""
      ? runForApp(timelineRows, only)
      : (timelineRows[0] ?? null)) ??
    parked?.run ??
    null;
  const report = run?.report ?? null;

  const zip = zipProbe.reply === null ? null : zipVerdict(zipProbe.reply);
  const path: ComposePath = parked === undefined ? pathOf(draft, zip) : "package";
  const probeParked = probeParks(probe.reply?.reason ?? "");
  const sourceDone = parked !== undefined || sourceReady(draft, zip, probeParked);

  const phase = phaseOf({
    path,
    runStatus: run?.status ?? "",
    siteId: created.siteId,
    // A CI-PUSHED SOURCE HAS NOTHING TO PUBLISH FROM HERE. Its bytes are on
    // somebody's CI, so the moment the draft site exists this flow is done
    // and the Live stop is what waits.
    published: draft.choice === "ci" ? created.siteId !== "" : publishedZip,
  });

  const placedKey = (placed ?? []).join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const apps = useMemo(() => appsToPlace(path, report, placed ?? []), [path, report, placedKey]);
  const appsKey = apps.join("|");
  const sourceName = parked ? parked.pkg.name : draft.name;

  // EVERY APP STARTS WITH A SUGGESTION, and only a MISSING one.
  // `suggestSlug` derives it from names already in front of the person;
  // re-seeding one they have edited would undo their typing every time the
  // report re-arrived on an event.
  useEffect(() => {
    if (apps.length === 0) return;
    setAddresses((held) => {
      let changed = false;
      const next = { ...held };
      for (const app of apps) {
        if (next[app] !== undefined) continue;
        next[app] = seedAddress(sourceName, app);
        changed = true;
      }
      return changed ? next : held;
    });
    // `appsKey` rather than `apps`: the array is rebuilt on every report
    // event, and keying on its identity would re-run this for a report
    // naming exactly the same apps.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [appsKey, sourceName]);

  // DEPLOYING ONE DECLARED APP, and nothing else.
  //
  // `apps` is already just the unplaced ones, so the gate ASKS about the right
  // app on its own. The problem is the others: an app with a site gets no
  // placement, the engine defaults it to not-skipped, and deploying one app
  // rebuilds and republishes every one of them -- the complaint that took the
  // Deploy button off the source page, in a new place.
  //
  // So when the flow is scoped to one app, every OTHER app the report names is
  // sent explicitly skipped. That is the same `skip` a person ticks by hand,
  // and it is why this could not be built until skip actually reached the
  // engine.
  const everyApp = useMemo(() => (report?.deployables ?? []).map((d) => d.name).filter((n) => n !== ""), [report]);
  const scoped = only !== undefined && only !== "";
  const placementApps = scoped ? everyApp : apps;
  const placementAddresses = useMemo(() => {
    if (!scoped) return addresses;
    const out: Record<string, AddressDraft> = { ...addresses };
    for (const app of everyApp) {
      if (app === only) continue;
      out[app] = { ...(out[app] ?? EMPTY_ADDRESS), skip: true };
    }
    return out;
  }, [scoped, addresses, everyApp, only]);

  const placementsDone = placementsComplete(apps, addresses, clusterDomain);
  const readyToAnalyze = canWrite && path !== "unknown" && sourceDone && (path !== "handmade" || placementsDone);
  const readyToDeploy = path === "handmade" ? draft.artifactId !== "" : placementsDone;

  const action = actionFor(phase, readyToAnalyze, readyToDeploy);
  const busy = newPackage.busy || pkgActions.busy || createSite.busy || publish.busy || tie.busy || addDomain.busy;

  const outcomes: readonly DeployableOutcome[] = run?.deployables ?? [];
  const placementsLocked =
    phase === "deploying" || phase === "published" || (path === "handmade" && created.siteId !== "");
  const siteHostname = created.siteId === "" ? "" : hostnameFor(addresses[""]?.slug ?? "", clusterDomain);

  // -------------------------------------------------------------------------
  // The two acts
  // -------------------------------------------------------------------------

  async function analyze(): Promise<void> {
    if (path === "package") {
      const id = await newPackage.create({
        name: draft.name.trim(),
        sourceKind: draft.choice === "zip" ? "artifact" : "repo",
        repoUrl: draft.choice === "repo" ? draft.repoUrl.trim() : "",
        repoRef: draft.choice === "repo" ? draft.repoRef.trim() : "",
        credentialId: draft.choice === "repo" ? draft.credentialId.trim() : "",
        artifactId: draft.choice === "zip" ? draft.artifactId : "",
      });
      if (id === "") return;
      setCreated((held) => ({ ...held, packageId: id }));
      // WITHOUT confirm: the run parks with its report and nothing is built.
      // The gate is always present (design D12), and this is it.
      await pkgActions.deploy(id, { confirm: false });
      reseed();
      return;
    }

    const address = addresses[""] ?? EMPTY_ADDRESS;
    const siteId = await createSite.create(
      { slug: address.slug, kind: draft.kind, title: draft.name.trim(), storeDomain: "", storefrontTokenRef: "" },
      clusterDomain,
    );
    if (siteId === "") return;
    setCreated((held) => ({ ...held, siteId }));
    // THE TWO OPTIONAL HALVES, applied exactly as the pipeline applies a
    // placement's: the same two calls, under the same actor, so the same
    // guards decide. Either being refused leaves the deployable created, and
    // says so rather than reading as a failed create.
    if (address.accountId.trim() !== "") await tie.setAccount(siteId, address.accountId.trim());
    if (address.ownDomain.trim() !== "") await addDomain.add(siteId, address.ownDomain.trim());
  }

  async function deploy(): Promise<void> {
    if (path === "handmade") {
      const ok = await publish.publish(created.siteId, draft.artifactId);
      if (ok) setPublishedZip(true);
      return;
    }
    // SKIPPING IS A STANDING CHOICE, not a fact about this run. Before this,
    // a skipped app came back on the next screen as an ordinary "not deployed"
    // row -- indistinguishable from one nobody had got to -- so the console
    // offered to deploy the very app somebody had just declined, and
    // discarding the result offered it again.
    //
    // ONLY WHAT THE PERSON TICKED. `placementAddresses` also carries the apps
    // that `only` skipped to scope a single-app deploy, and those are a
    // MECHANISM rather than a choice -- disabling them would turn "deploy just
    // this one" into "turn the others off".
    const declined = Object.entries(addresses)
      .filter(([, a]) => a.skip === true)
      .map(([app]) => app);
    // The names, not the resulting list. Composing the result needed the
    // package's current off-list, and `parked` is absent on the first-deploy
    // path -- which is exactly where somebody skips an app -- so that read had
    // to fall back to an empty list and hope nothing else was already off. A
    // membership change has nothing to read (memql#4951).
    await pkgActions.disableDeployables(packageId, declined);
    // THE PARKED RUN IS WHAT THIS CONFIRMS (memql#4954). Compose is where a
    // person reads the report and answers it, so the answer has to land on the
    // run that asked -- otherwise the gate they just passed stays open and the
    // bytes their report described are fetched again.
    await pkgActions.deploy(packageId, {
      confirm: true,
      placements: placementsFrom(placementApps, placementAddresses, clusterDomain),
      ...(run !== null && run.status === "awaiting_confirm" ? { deploymentId: run.id } : {}),
    });
    reseed();
  }

  async function retry(): Promise<void> {
    if (packageId === "") return;
    await pkgActions.deploy(packageId, { confirm: false });
    reseed();
  }

  function act() {
    switch (phase) {
      case "composing":
        void analyze();
        return;
      case "awaiting_confirm":
        void deploy();
        return;
      case "stopped":
        void retry();
        return;
      default:
        return;
    }
  }

  // -------------------------------------------------------------------------
  // The rail
  // -------------------------------------------------------------------------

  const input: ComposeInput = {
    mode: "compose",
    only,
    ...stopsFor(phase, path, sourceDone, report !== null),
    probeReason: probeParked && probe.reply !== null ? probeNote(probe.reply) : "",
    report,
    problem: phase === "stopped" ? problemOf(run) : null,
    moving: movingStopFor(phase, run?.status ?? ""),
    prebuilt: path === "handmade",
    answers: {
      source: sourceAnswer(parked?.pkg ?? null, draft),
      ...(path === "handmade" ? { whatItIs: handMadeVerdict(draft) } : {}),
      ...(siteHostname === "" ? {} : { whereItLives: siteHostname }),
      ...(phase === "published" && publishedHosts(outcomes).length > 0
        ? { whereItLives: publishedHosts(outcomes).join(", ") }
        : {}),
      ...liveAnswer(phase, path, draft, siteHostname, outcomes),
    },
  };

  const stopBody = (stage: RailStage) => {
    switch (stage.id) {
      case "source":
        return (
          <ComposeSourceStop
            draft={draft}
            onDraft={(patch) => setDraft((held) => ({ ...held, ...patch }))}
            credentials={credentials}
            isClusterOwner={isClusterOwner}
            probe={probe}
            zipProbe={zipProbe}
            zip={zip}
            siteId={created.siteId}
            clusterDomain={clusterDomain}
            locked={parked !== undefined || phase !== "composing"}
            tokenFormOpen={tokenFormOpen}
            onTokenFormOpenChange={setTokenFormOpen}
          />
        );
      case "whatItIs":
        // THE PREVIEW STANDS IN UNTIL THE REPORT EXISTS, and never beside it.
        // A probe under a grant answers what the manifest CLAIMS, which is
        // enough to recognise the package you just picked; the run answers
        // what this cluster FOUND, and the moment it has, the claim is the
        // weaker of two sentences about one thing.
        if (report === null) {
          // ...and a stop nobody can reach yet says nothing, which is the
          // rule the address stop below states: a preview inside a pending
          // stop would contradict the mark beside it.
          if (stage.state === "pending" || stage.state === "ahead") return null;
          if (probe.reply === null) return null;
          return (
            <div className="os-stop-body">
              <ManifestPreview manifest={probe.reply.manifest} />
            </div>
          );
        }
        return (
          <div className="os-stop-body">
            <ReportView report={report} only={only} />
          </div>
        );
      case "whereItLives":
        // Not reachable yet is not a form: a stop nobody can answer draws its
        // blurb and nothing else.
        if (stage.state === "pending" || stage.state === "ahead") return null;
        return (
          <ComposeWhereItLivesStop
            apps={apps}
            sourceName={sourceName}
            addresses={addresses}
            onAddress={(app, patch) =>
              setAddresses((held) => ({ ...held, [app]: { ...(held[app] ?? EMPTY_ADDRESS), ...patch } }))
            }
            accounts={accounts}
            isClusterOwner={isClusterOwner}
            clusterDomain={clusterDomain}
            outcomes={outcomes}
            /* CHOSEN ONCE. The moment the flow has written something at these
               addresses -- a package run past its gate, or the hand-made
               draft site -- the fields become facts: editing a slug that
               `createSite` already claimed changes nothing, and a field that
               accepts a value and does nothing is worse than no field. */
            locked={placementsLocked}
          />
        );
      default:
        return null;
    }
  };

  // THE FORWARD ACT LIVES ON THE BAR, NOT IN THE HEAD (rule 12, epic
  // memql#4937). It was at the top, so answering a long Source stop meant
  // scrolling back UP to continue -- the complaint that started this epic.
  // Cancel sits beside it, so leaving is as reachable as continuing.
  const barActs: Act[] =
    action === null || action.disabled
      ? []
      : [{ label: action.label, tone: action.tone, busy, onAct: act }];

  return (
    <div className="os-deploy-pane">
      <div className="os-deploy-scroll">
    {/* THE TITLE SAYS WHAT THIS IS. "New deployable" is true only when there
        is no source yet: opened for one app of a source added days ago, it
        read as though the whole source were being added again -- which is
        exactly what the report beside it appeared to confirm. */}
    <Panel label={composeTitle(parked, only)}>
      <Head title={composeTitle(parked, only)}>
        <Button tone="quiet" onClick={onBack}>
          <ArrowLeft size={13} aria-hidden /> Deployables
        </Button>
        {onAsk ? (
          <Button
            tone="quiet"
            onClick={() =>
              onAsk(parked ? `app:deployables package:${parked.pkg.name || parked.pkg.id}` : "app:deployables compose")
            }
            ariaLabel="Ask about this deployable"
          >
            <Sparkles size={13} aria-hidden /> Ask
          </Button>
        ) : null}
      </Head>

      {/* A refusal the engine gave BEFORE any row exists has no stop to belong
          to yet, so it renders beneath the action that asked for it. Every
          other refusal in this flow lands at its stop. */}
      {newPackage.refusal ? <ProblemNotice problem={{ ...newPackage.refusal, fatal: true }} tone="error" /> : null}
      {pkgActions.refusal ? <ProblemNotice problem={{ ...pkgActions.refusal, fatal: true }} tone="error" /> : null}
      {createSite.error === "" ? null : (
        <Notice
          tone="error"
          sentence="This deployable was not created."
          next="Nothing was written. The name may already be taken -- this cluster's own answer is below."
          detail={createSite.error}
        />
      )}
      {tie.error === "" ? null : (
        <Notice
          tone="warn"
          sentence="It was created, but not tied to that client."
          next="Set the client on the deployable's own page. Nothing else was affected."
          detail={tie.error}
        />
      )}
      {addDomain.error === "" ? null : (
        <Notice
          tone="warn"
          sentence="It was created, but the domain was not bound."
          next="Add the domain on the deployable's own page, where the two DNS records are."
          detail={addDomain.error}
        />
      )}
      {publish.error === "" ? null : (
        <Notice
          tone="error"
          sentence="That zip was not deployed."
          next="The deployable exists and is serving nothing yet. Deploy again, or pick another zip."
          detail={publish.error}
        />
      )}

      <Rail input={input} stopBody={stopBody} />

      {/* THE OUTCOME IS THE BAR'S, not a notice in the panel (DESIGN.md rule
          12). This said the same thing the bar said, two inches apart, and
          carried the act while the bar offered Cancel -- so the finished flow
          had two summaries and its only real control was in the half a person
          does not read for controls. */}
      {canWrite ? null : (
        <Caption>Composing a deployable is a deploy-tier act, and this cluster has not given you that rank.</Caption>
      )}
    </Panel>
      </div>

      <ActionBar
        state={composePhaseWord(phase)}
        detail={
          phase === "published"
            ? `${publishedSentence(path, draft, outcomes)} It is on the Deployables list now.`
            : composePhaseDetail(phase, action, apps, addresses)
        }
        tone={phase === "analyzing" || phase === "deploying" ? "busy" : "none"}
        acts={
          phase === "published"
            ? [{ label: "Done", tone: "primary" as const, onAct: onBack }]
            : barActs
        }
      >
        {/* CANCEL IS ALWAYS REACHABLE WHILE THERE IS SOMETHING TO CANCEL, on
            the left of the forward act: nothing is written until Analyze, so
            leaving costs nothing, and a flow somebody cannot leave without
            hunting for the way out is one they abandon by closing the window.
            ONCE IT HAS PUBLISHED there is nothing to cancel -- the apps are
            deployed -- so the act is Done and the word Cancel would be a lie
            about what leaving now would undo. */}
        {phase === "published" ? null : (
          <Button tone="quiet" onClick={onBack}>
            Cancel
          </Button>
        )}
      </ActionBar>
    </div>
  );
}

/**
 * What this flow is about, for the Head and the Panel's label.
 *
 * "New deployable" is the ONLY-when-nothing-is-known answer, and both other
 * facts arrive independently: `only` is known from the click, while `parked`
 * lands when the analysis parks its gate seconds later. Reading the title off
 * `parked` alone meant an app-scoped flow was called "New deployable" for as
 * long as the analysis ran -- which is the frame the report was read in.
 */
function composeTitle(parked: { pkg: PackageRow; run: DeploymentRow } | undefined, only: string | undefined): string {
  const app = only !== undefined && only !== "" ? only : "";
  const source = parked === undefined ? "" : parked.pkg.name || parked.pkg.id;
  if (app !== "" && source !== "") return `Deploy ${app} from ${source}`;
  if (app !== "") return `Deploy ${app}`;
  if (source !== "") return `Deploy ${source}`;
  return "New deployable";
}

/** Where the flow is, in words -- the bar's left half. */
function composePhaseWord(phase: ComposePhase): string {
  switch (phase) {
    case "analyzing":
      return "Reading the source";
    case "awaiting_confirm":
      return "Ready to deploy";
    case "deploying":
      return "Deploying";
    case "published":
      return "Deployed";
    case "stopped":
      return "Stopped";
    default:
      return "New deployable";
  }
}

/**
 * What the bar says beside the phase.
 *
 * On the placement step it counts what is actually going out, because that is
 * the number the forward act is about -- and because "0 of 2" is the honest
 * reading of a state where the act is absent (memql#4930).
 */
function composePhaseDetail(
  phase: ComposePhase,
  action: HeadAction | null,
  apps: readonly string[],
  addresses: Readonly<Record<string, AddressDraft>>,
): string {
  if (phase === "awaiting_confirm" && apps.length > 1) {
    const going = apps.filter((app) => addresses[app]?.skip !== true).length;
    if (going === 0) return "pick at least one app to deploy";
    return `${going} of ${apps.length} apps, each at its own address`;
  }
  if (action !== null && action.disabled) return "there is more to answer above";
  switch (phase) {
    case "analyzing":
      return "reading the tree and running the gates a node runs at boot";
    case "deploying":
      return "building, then publishing -- nothing is live until the last step";
    case "published":
      return "it is on the Deployables list now";
    default:
      return "nothing is written until you deploy";
  }
}

// ---------------------------------------------------------------------------
// The readings
// ---------------------------------------------------------------------------

/** Which stops are answered and which one is open, by phase. */
function stopsFor(
  phase: ComposePhase,
  path: ComposePath,
  sourceDone: boolean,
  hasReport: boolean,
): { answered: StopId[]; open: StopId | null } {
  switch (phase) {
    case "composing":
      if (!sourceDone) return { answered: [], open: "source" };
      return path === "handmade"
        ? { answered: ["source", "whatItIs"], open: "whereItLives" }
        : { answered: ["source"], open: "whatItIs" };
    case "analyzing":
      return { answered: ["source"], open: null };
    case "awaiting_confirm":
      return path === "handmade"
        ? { answered: ["source", "whatItIs", "whereItLives"], open: "live" }
        : { answered: ["source", "whatItIs"], open: "whereItLives" };
    case "deploying":
      return { answered: ["source", "whatItIs", "whereItLives"], open: null };
    case "published":
      return { answered: ["source", "whatItIs", "whereItLives", "build", "live"], open: null };
    case "stopped":
      return { answered: hasReport ? ["source", "whatItIs"] : ["source"], open: null };
  }
}

/**
 * The Head's action for a phase.
 *
 * It goes THROUGH `headActionFor` rather than around it: that function is the
 * design's table, and a second answer beside it would be a second table. All
 * this decides is which row of it a phase is on.
 */
function actionFor(phase: ComposePhase, readyToAnalyze: boolean, readyToDeploy: boolean): HeadAction | null {
  switch (phase) {
    case "composing":
      return headActionFor({ at: "composing", sourceComplete: readyToAnalyze });
    case "awaiting_confirm":
      return headActionFor({ at: "awaiting_confirm", placementsComplete: readyToDeploy });
    case "analyzing":
    case "deploying":
      return headActionFor({ at: "running" });
    case "stopped":
      return headActionFor({ at: "refused_or_failed" });
    case "published":
      // Nothing. What follows a first publish -- Make it live -- belongs to
      // the deployable's own page, where the row and its lifecycle are.
      return null;
  }
}

/** What the Source stop's one-line answer says, from the source itself. */
function sourceAnswer(pkg: PackageRow | null, draft: ComposeDraft): string {
  if (pkg !== null) return sourceLabel(pkg);
  switch (draft.choice) {
    case "repo":
      return draft.repoUrl.trim() === ""
        ? ""
        : `${shortRepo(draft.repoUrl)} at ${draft.repoRef.trim() === "" ? "default branch" : draft.repoRef.trim()}`;
    case "zip":
      return draft.artifactId === "" ? "" : "a zip in Files";
    case "ci":
      return draft.name.trim() === "" ? "" : "pushed by your CI";
    default:
      return "";
  }
}

/** A hand-made deployable's verdict: nobody analyzed it, so it is what was chosen. */
function handMadeVerdict(draft: ComposeDraft): string {
  const kind = draft.kind === "" ? "" : kindLabel(draft.kind);
  if (kind === "") return "";
  return draft.choice === "ci" ? `${kind}, pushed by your CI` : `${kind}, already built`;
}

function liveAnswer(
  phase: ComposePhase,
  path: ComposePath,
  draft: ComposeDraft,
  siteHostname: string,
  outcomes: readonly DeployableOutcome[],
): Partial<Record<StopId, string>> {
  if (phase !== "published") return {};
  if (path === "handmade" && draft.choice === "ci") {
    return {
      live:
        siteHostname === ""
          ? "Waiting for the first push from your CI."
          : `Waiting for the first push from your CI. ${siteHostname} starts serving when it lands.`,
    };
  }
  const published = publishedHosts(outcomes);
  const hosts = published.length > 0 ? published.join(", ") : siteHostname;
  return hosts === "" ? {} : { live: `Published to ${hosts}. Not serving yet.` };
}

/**
 * The hostnames a run actually published, from its outcomes.
 *
 * `?? ""` IS THE WHOLE FIX. The engine writes a skipped outcome as
 * `{name, refusal}` with NO hostname key, and `listOf<DeployableOutcome>`
 * casts the raw array without normalising -- so `hostname` is `undefined` at
 * runtime while the type says `string`, and the obvious `o.hostname !== ""`
 * is TRUE for it. Skipping one app of two and deploying reported "Published 2
 * apps", and the hostname list it joined carried an empty slot.
 *
 * A REFUSAL IS NOT THE TEST. `deployable_account_refused` and
 * `deployable_domain_refused` publish the app and then fail the tie, so they
 * carry both a hostname and a refusal -- and they did publish. The hostname is
 * the fact; the refusal is about something else.
 */
function publishedHosts(outcomes: readonly DeployableOutcome[]): string[] {
  return outcomes.map((o) => (o.hostname ?? "").trim()).filter((h) => h !== "");
}

function publishedSentence(path: ComposePath, draft: ComposeDraft, outcomes: readonly DeployableOutcome[]): string {
  if (path === "handmade" && draft.choice === "ci") {
    return "The deployable is ready and its door is open. Nothing serves until your CI pushes.";
  }
  // NO OUTCOMES AT ALL is the hand-made path: a zip is published through
  // `sitePublishFromArtifact` rather than through a run, so there is no
  // per-deployable list to count and one site was published. "Nothing was
  // published" is reserved for a run that REPORTED outcomes and published
  // none -- which is a real state, and a different one.
  if (outcomes.length === 0) return "Published. It is not serving yet.";
  const made = publishedHosts(outcomes).length;
  if (made > 1) return `Published ${made} apps. None of them is serving yet.`;
  return made === 1 ? "Published 1 app. It is not serving yet." : "Nothing was published.";
}

/** The refusal that stopped the flow: the run's error, or the report's first fatal problem. */
function problemOf(run: DeploymentRow | null): RailProblem | null {
  if (run === null) return null;
  if (run.error !== null && run.error.message.trim() !== "") return run.error;
  const fatal = (run.report?.problems ?? []).find((p) => p.fatal);
  return fatal ? { code: fatal.code, message: fatal.message, scope: fatal.scope } : null;
}

/**
 * The timeline, newest first -- the page's own rule for its own reason: the
 * collection folds events in the order the cluster sent them, so `rows[0]`
 * would read a stale run exactly when a new one arrived.
 */
function newestFirst(rows: DeploymentRow[]): DeploymentRow[] {
  const at = (d: DeploymentRow): string => d.startedAt || d.createdAt;
  return [...rows].sort((a, b) => {
    const byTime = at(b).localeCompare(at(a));
    return byTime !== 0 ? byTime : b.id.localeCompare(a.id);
  });
}
