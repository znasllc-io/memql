import { useCallback, useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from "react";
import { useSession } from "../../chrome/access";
import { Caption, ContentSkeleton, Head, InlineSkeleton, Notice, Rail, useAppReach, type Stop } from "../../kit";
import { ActionBar } from "../../kit/ActionBar";
import { useOsConnection } from "../../live/connection";
import { useMachines } from "../../live/machines";
import { MODULE_DESCRIPTIONS } from "../../system/modules";
import type { OsAppProps } from "../../system/registry";
import { readLaggingInstallations, type LaggingInstallation } from "../deployables/pipelines/calls";
import { rememberConnectAttempt, returnPathFor, type ConnectReturn } from "../deployables/sources/connectReturn";
import { ConnectReturnNotice } from "../deployables/sources/ConnectReturnNotice";
import { GithubAppMissing } from "../deployables/sources/GithubAppSetup";
import { useGithubApp, type GithubAppActions } from "../deployables/sources/useGithubApp";
import { bare } from "../deployables/people";
import { machineFromRow } from "../fleet/rows";
import { machinesAllowingPipelines } from "../deployables/pipelines/fleet";
import {
  fleetSentence,
  missingPermissionsWords,
  readPipelines,
  type SubStep,
} from "./pipelinesReadiness";
import "./pipelines.css";
import { PipelineRunners } from "./PipelineRunners";

// Settings -> Pipelines (epic memql#5479, design record D15).
//
// ===========================================================================
// A STANDING READINESS ITEM, NOT A WIZARD
// ===========================================================================
// Pipelines is the cluster's first OPTIONAL readiness item: nothing needs it,
// and it can be set up later. Its three sub-steps -- the GitHub App, a
// repository whose pipeline is connected, compute -- are the readiness row's
// own facts (pipelinesReadiness.ts), drawn as the setup rail draws a set of
// doors: a check for what holds, a held ring for what is the owner's to do
// here, a plain ring for what is unset and not this page's to do, a dimmed
// ring for what cannot be done before something else, and the "not known"
// state for a fact the cluster has not said. There is no Next and nothing to
// finish here; the page is what is true now and the acts that change it.
//
// ===========================================================================
// THE GITHUB APP IS SET UP THE WAY IT IS EVERYWHERE ELSE
// ===========================================================================
// From the cluster's app manifest, whose permissions -- checks write included,
// since the pipelines record widened it -- are composed by the identity node
// and arrive at GitHub pre-filled. The same component Settings > Connections
// renders (`GithubAppMissing`), so there is one way to register the app and
// one place its question lives. It is offered only when the cluster has said
// this person may: "not known" offers nothing, and a refusal lands in place.
//
// ===========================================================================
// GITHUB'S OWN QUESTION IS SURFACED, NOT LEFT TO BLOCK RUNS
// ===========================================================================
// When the app asks for more than an installation has accepted, GitHub asks
// that account to approve the change and, until it does, refuses the check
// runs a pipeline writes. That waits on GitHub's installation settings page,
// where nobody looks unprompted -- so every lagging installation is named here
// with a link to exactly that page. A read that failed is said as a failure,
// never drawn as "none lag".
//
/** Where GitHub sends an owner back to after registering the app from here. */
export const PIPELINES_RETURN_PATH = returnPathFor("pipelines", "settings");

export function PipelinesSection({ intent, consumeIntent }: Pick<OsAppProps, "intent" | "consumeIntent">) {
  const { readiness, access } = useSession();
  const loaded = readiness?.loaded === true;
  const verdict = readiness?.loaded ? readiness.of("pipelines") : null;
  const reading = useMemo(() => readPipelines(verdict), [verdict]);
  const github = useGithubApp();
  const returned = useConnectReturn(intent, consumeIntent, github.refresh);
  const installations = useLaggingInstallations();
  const fleet = useMachinesAllowingPipelines(access?.userId ?? "");
  const sources = useAppReach("deployables");
  const openSources =
    sources.canOpenWindows && sources.sections.includes("sources") ? () => sources.open("sources") : null;
  // GITHUB ENDS A REGISTER-AND-INSTALL ON A NEUTRAL RETURN (`?github=installed`)
  // that names no section, which would land the owner on Deployables > Sources.
  // Remembering where the trip began brings them back here; the record steers
  // navigation only, and identity still validates everything the trip carries.
  // BARE, as the dispatcher compares it (ConnectReturnDispatcher).
  const viewer = bare(access?.userId ?? "");
  const githubFromHere: GithubAppActions = {
    ...github,
    setup: (returnPath, organization) => {
      rememberConnectAttempt("", viewer, "", "pipelines", "settings");
      return github.setup(returnPath, organization);
    },
  };
  const stops: Stop[] = [
    githubAppStop(reading.githubApp, githubFromHere, returned),
    repositoryStop(reading.repository, reading.githubApp.reading, openSources),
    computeStop(reading.compute, fleet),
  ];

  return (
    <section className="os-action-pane os-pipelines-setup" aria-label="Pipelines">
      <div className="os-action-body os-app-stack">
        <Head title="Pipelines" meta={verdict?.optional ? "Optional" : undefined} />
        <Caption>{MODULE_DESCRIPTIONS.pipelines}</Caption>
        {loaded ? (
          <Rail stops={stops} label="What pipelines need" scale="page" />
        ) : readiness?.state === "seeding" ? (
          // LOADING IS THE SHAPE OF THE CONTENT (DESIGN.md): three quiet
          // shapes where the three sub-steps will stand.
          <ContentSkeleton kind="detail" label="Loading what pipelines need" />
        ) : (
          // A FEED THAT IS NOT SEEDING AND HAS NOT LOADED IS NOT IN PROGRESS --
          // a skeleton here would promise an answer that is not on its way.
          <Caption>The cluster's setup could not be read.</Caption>
        )}
        <LaggingInstallations read={installations} />
      </div>
      {loaded ? (
        <ActionBar
          state={reading.words}
          detail={reading.detail}
          tone={reading.state === "setUp" ? "live" : "none"}
          live
          acts={[]}
        />
      ) : null}
    </section>
  );
}

// ---------------------------------------------------------------------------
// The three stops
// ---------------------------------------------------------------------------

function githubAppStop(step: SubStep, app: GithubAppActions, returned: ConnectReturn | null): Stop {
  const base = { id: step.id, name: step.name };
  // What came back from GitHub belongs beside the control that went there.
  const notice = returned === null ? null : <ConnectReturnNotice result={returned} />;
  const body = (inner: ReactNode) =>
    notice === null && inner === null ? undefined : <div className="os-pipelines-stop">{notice}{inner}</div>;
  const slug = app.status?.slug ?? "";
  const registered = slug === "" ? "Registered" : `Registered as ${slug}`;
  if (step.reading === "done") {
    return {
      ...base,
      state: "done",
      sentence: slug === "" ? "This cluster has a GitHub App." : `${registered}.`,
      body: body(null),
    };
  }
  if (step.reading === "notKnown") {
    return {
      ...base,
      state: "unknown",
      sentence: "This cluster has not said whether it has a GitHub App.",
      body: body(null),
    };
  }
  // REGISTERED, AND NOT YET REPORTED. The agents evaluate the item on their
  // own schedule, so an app registered a moment ago reads "not done" here
  // until they look again -- and offering to register a second one meanwhile
  // would be the worst answer on the page.
  if (app.status?.configured === true) {
    return { ...base, state: "open", sentence: `${registered}. The cluster has not reported it yet.`, body: body(null) };
  }
  if (app.status?.configured === false && app.status.canSetup) {
    // SAY IT ONCE (rule 7): the setup's own caption says what the app is and
    // what it may do, so the stop carries no sentence of its own.
    return { ...base, state: "open", body: body(<GithubAppMissing app={app} returnPath={PIPELINES_RETURN_PATH} />) };
  }
  return {
    ...base,
    state: "open",
    // NOT KNOWN IS NOT "YOU MAY NOT": with no answer about this person, the
    // stop says what is missing and offers nothing that might be refused.
    sentence:
      app.status === null
        ? "This cluster has no GitHub App yet."
        : "This cluster has no GitHub App yet. A cluster owner can set it up.",
    body: body(null),
  };
}

function repositoryStop(step: SubStep, app: SubStep["reading"], openSources: (() => void) | null): Stop {
  const base = { id: step.id, name: step.name };
  if (step.reading === "done") return { ...base, state: "done", sentence: "A repository's pipeline is connected." };
  if (step.reading === "notKnown") {
    return {
      ...base,
      state: "unknown",
      sentence: "This cluster has not said whether a repository's pipeline is connected.",
    };
  }
  // A PIPELINE IS CONNECTED THROUGH THE APP, so without one this stop cannot
  // be done yet: dimmed, and with no act that would lead somewhere it fails.
  if (app === "notDone") {
    return {
      ...base,
      state: "ahead",
      sentence:
        "No repository's pipeline is connected yet. Connect one from its source's page once the GitHub App is set up.",
    };
  }
  return {
    ...base,
    state: "open",
    sentence: "No repository's pipeline is connected yet. Connect one from its source's page.",
    // Words alone where there is no window to open, or no Sources this person
    // may reach: the sentence already says where.
    body:
      openSources === null ? undefined : (
        <div className="os-pipelines-stop">
          <button type="button" className="os-link os-pipelines-act" onClick={openSources}>
            Open Sources
          </button>
        </div>
      ),
  };
}

function computeStop(step: SubStep, fleet: FleetReading): Stop {
  const base = { id: step.id, name: step.name };
  if (step.reading === "done") {
    return {
      ...base,
      state: "done",
      sentence: "The step dispatcher is installed.",
      body: <div className="os-pipelines-stop">
        <PipelineRunners />
        {fleet.state === "absent" ? null : <FleetLine fleet={fleet} />}
      </div>,
    };
  }
  if (step.reading === "notKnown") {
    return { ...base, state: "unknown", sentence: "This cluster has not said whether it can run steps." };
  }
  // NOT THE OWNER'S TO DO FROM HERE: the runner is installed with the
  // cluster, so this stop is unset rather than waiting on the person -- no
  // held ring, which would promise an act this page does not have.
  return {
    ...base,
    state: "waiting",
    sentence: "This cluster cannot run steps yet. A command step fails until the pipelines runner is installed.",
  };
}

function FleetLine({ fleet }: { fleet: FleetReading }) {
  if (fleet.state === "absent") return null;
  if (fleet.state === "reading") return <InlineSkeleton label="Loading your machines" />;
  if (fleet.state === "unread") return <p className="os-pipelines-fleet">Your machines could not be read.</p>;
  return <p className="os-pipelines-fleet">{fleetSentence(fleet.count)}</p>;
}

// ---------------------------------------------------------------------------
// Installations whose accepted permissions lag the app's
// ---------------------------------------------------------------------------

type LaggingRead = { state: "reading" } | { state: "read"; installations: LaggingInstallation[] } | { state: "failed" };

/** Asked once, when the section opens; an abandoned read is aborted. */
function useLaggingInstallations(): LaggingRead {
  const connection = useOsConnection();
  const query = connection?.query ?? null;
  const [read, setRead] = useState<LaggingRead>({ state: "reading" });
  useEffect(() => {
    if (query === null) return;
    const controller = new AbortController();
    setRead({ state: "reading" });
    readLaggingInstallations(query, controller.signal).then(
      (installations) => {
        if (!controller.signal.aborted) setRead({ state: "read", installations });
      },
      () => {
        if (!controller.signal.aborted) setRead({ state: "failed" });
      },
    );
    return () => controller.abort();
  }, [query]);
  return read;
}

function LaggingInstallations({ read }: { read: LaggingRead }) {
  // NOTHING WHILE ASKING. Nearly every cluster has nothing to say here, and a
  // shape reserved for notices that almost never come would be a gap that
  // closes on every visit.
  if (read.state === "reading") return null;
  if (read.state === "failed") return <Caption>GitHub could not be asked about the app's installations.</Caption>;
  return (
    <>
      {read.installations.map((installation, index) => (
        <LaggingNotice key={`${installation.installationId}:${installation.account}:${index}`} installation={installation} />
      ))}
    </>
  );
}

function LaggingNotice({ installation }: { installation: LaggingInstallation }) {
  const account = installation.account || "an account";
  const missing = missingPermissionsWords(installation.missingPermissions);
  return (
    <Notice tone="warn" sentence={`GitHub is asking ${account} to accept this app's new permissions.`}>
      <p className="os-caption">
        Until it does, check runs on its repositories are refused.
        {installation.suspended ? " Its installation is also suspended, so nothing reaches those repositories." : ""}
        {missing === "" ? "" : ` Missing: ${missing}.`}
      </p>
      {/* GitHub's own page for the installation, where the change waits to be
          accepted: a new tab, because this page is where they come back to. */}
      {installation.htmlUrl === "" ? null : (
        <a className="os-link os-pipelines-review" href={installation.htmlUrl} target="_blank" rel="noopener noreferrer">
          Review on GitHub
        </a>
      )}
    </Notice>
  );
}

// ---------------------------------------------------------------------------
// The fleet, the return from GitHub, and "Not now"
// ---------------------------------------------------------------------------

/** No machines feed at all (no connection), still asking, a read that did
 *  not answer, or the count. */
type FleetReading = { state: "absent" } | { state: "reading" } | { state: "unread" } | { state: "read"; count: number };

/** How many of the viewer's machines allow pipelines, from the one machines
 *  feed the shell retains -- never a second read of the same concept. */
function useMachinesAllowingPipelines(viewerId: string): FleetReading {
  const { collection, feedState } = useMachines();
  const subscribe = useCallback((listener: () => void) => collection?.subscribe(listener) ?? (() => {}), [collection]);
  const snapshot = useSyncExternalStore(subscribe, () => collection?.snapshot ?? null, () => null);
  const count = useMemo(
    () => machinesAllowingPipelines((snapshot?.rows ?? []).map(machineFromRow), viewerId),
    [snapshot, viewerId],
  );
  if (feedState === "live" || feedState === "degraded") return { state: "read", count };
  if (feedState === "seeding") return { state: "reading" };
  if (feedState === "absent") return { state: "absent" };
  return { state: "unread" };
}

/**
 * A GitHub round trip that came back to this section (`returnPathFor`):
 * held for the GitHub App stop, the status asked again, and the intent
 * consumed by its id so a stale render can never eat a newer one.
 */
function useConnectReturn(
  intent: OsAppProps["intent"],
  consumeIntent: OsAppProps["consumeIntent"],
  refresh: () => void,
): ConnectReturn | null {
  const [returned, setReturned] = useState<ConnectReturn | null>(null);
  useEffect(() => {
    if (!intent) return;
    const answer = intent.payload.connect as ConnectReturn | undefined;
    if (answer && typeof answer.reason === "string") {
      setReturned(answer);
      refresh();
    }
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent, refresh]);
  return returned;
}
