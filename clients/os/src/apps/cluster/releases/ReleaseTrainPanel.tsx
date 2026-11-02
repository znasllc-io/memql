import { useState } from "react";
import type { QueryClient, Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Notice, RecordList, RecordRow, SettingRow, Subhead, Switch } from "../../../kit";
import type { RecordTone } from "../../../kit/RecordRow";
import { formatMoment } from "../../../kit/format";
import { InPlaceAsk } from "../../../kit/pressHold";
import { flatten } from "../../../kit/rows";
import { useReleaseRead } from "./useReleaseRead";

// What each repository's releases are doing, and the controls a person needs: start
// one now, deploy one now, let them go by themselves, and resume after one went back
// (design record 2026-11-02-nightly-release-from-main-design.md). The engine decides
// and refuses; these only ask. Anything that replaces work or moves production asks
// first, where the button stood.

export type TrainQueries = Pick<QueryClient,
  "pipelineTrainsForOwner" | "pipelineDeploymentsForOwner" | "pipelinesTrainStatus" | "pipelinesTrainExpress" |
  "pipelinesTrainStanding" | "pipelinesDeployRelease" | "pipelinesDeploymentResume">;

interface TrainRow {
  id: string; repository: string; branch: string; state: string; version: string; heldReason: string; pullRequestUrl: string; departedAt: string;
}

interface DeploymentRow {
  id: string; repository: string; version: string; state: string; reason: string; pullRequestUrl: string; guards: string[];
}

interface RepositoryStatus {
  repository: string; fromMain: boolean; canRelease: boolean; canDeploy: boolean; release: boolean; deploy: boolean; paused: boolean;
  installRepository: string; windowStart: string; windowEnd: string; timeZone: string;
}

const words: Record<string, { word: string; tone: RecordTone }> = {
  building: { word: "Building", tone: "accent" },
  running: { word: "Testing", tone: "accent" },
  landing: { word: "Landing", tone: "accent" },
  releasing: { word: "Releasing", tone: "accent" },
  awaiting_approval: { word: "Waiting", tone: "warn" },
  held: { word: "Held", tone: "warn" },
  landed: { word: "Landed", tone: "muted" },
  published: { word: "Released", tone: "muted" },
  cancelled: { word: "Cancelled", tone: "muted" },
};
const deployWords: Record<string, { word: string; tone: RecordTone }> = {
  waiting: { word: "Waiting", tone: "warn" },
  pinning: { word: "Preparing", tone: "accent" },
  syncing: { word: "Deploying", tone: "accent" },
  verifying: { word: "Verifying", tone: "accent" },
  landing: { word: "Finishing", tone: "accent" },
  deployed: { word: "Deployed", tone: "muted" },
  rolling_back: { word: "Going back", tone: "warn" },
  verifying_rollback: { word: "Going back", tone: "warn" },
  rolled_back: { word: "Rolled back", tone: "warn" },
  stopped: { word: "Stopped", tone: "warn" },
  cancelled: { word: "Cancelled", tone: "muted" },
};
const moving = new Set(["building", "running", "landing", "releasing"]);
const deployMoving = new Set(["pinning", "syncing", "verifying", "landing", "rolling_back", "verifying_rollback"]);
const withReason = new Set(["held", "cancelled"]);

function text(row: Record<string, unknown>, name: string): string {
  const value = row[name];
  return typeof value === "string" ? value : "";
}

/** The newest row of each repository. The queries answer newest first. */
function newest<T extends { repository: string }>(rows: readonly T[]): Map<string, T> {
  const out = new Map<string, T>();
  for (const row of rows) {
    if (row.repository && !out.has(row.repository)) out.set(row.repository, row);
  }
  return out;
}

export function newestTrains(rows: readonly Row[]): TrainRow[] {
  return [...newest(rows.map((raw) => {
    const row = flatten(raw) as Record<string, unknown>;
    return { id: text(row, "id"), repository: text(row, "repository"), branch: text(row, "branch"), state: text(row, "state"), version: text(row, "version"), heldReason: text(row, "heldReason"), pullRequestUrl: text(row, "pullRequestUrl"), departedAt: text(row, "departedAt") };
  })).values()];
}

function deploymentOf(raw: Row): DeploymentRow {
  const row = flatten(raw) as Record<string, unknown>;
  const guards = Array.isArray(row.guards) ? row.guards.map(guard => text(guard as Record<string, unknown>, "detail")).filter(Boolean) : [];
  return { id: text(row, "id"), repository: text(row, "repository"), version: text(row, "version"), state: text(row, "state"), reason: text(row, "reason"), pullRequestUrl: text(row, "pullRequestUrl"), guards };
}

function statusOf(row: Record<string, unknown>): RepositoryStatus {
  const flag = (name: string) => row[name] === true;
  return { repository: text(row, "repository"), fromMain: flag("fromMain"), canRelease: flag("canRelease"), canDeploy: flag("canDeploy"), release: flag("release"), deploy: flag("deploy"), paused: flag("paused"), installRepository: text(row, "installRepository"), windowStart: text(row, "windowStart"), windowEnd: text(row, "windowEnd"), timeZone: text(row, "timeZone") };
}

type Ask =
  | { kind: "replace"; repository: string; branch: string }
  | { kind: "deploy"; repository: string; installRepository: string }
  | { kind: "standing"; repository: string; release: boolean; deploy: boolean };

export function ReleaseTrainPanel({ query, connected, visible = true }: { query: TrainQueries | null; connected: boolean; visible?: boolean }) {
  const available = connected && query !== null;
  const read = useReleaseRead("trains", available ? async signal => {
    const [status, trains, deployments] = await Promise.all([
      query.pipelinesTrainStatus({ signal }),
      query.pipelineTrainsForOwner({}, { signal }),
      query.pipelineDeploymentsForOwner({}, { signal }),
    ]);
    const first = status.rows()[0];
    const repositories = first ? (flatten(first) as Record<string, unknown>).repositories : undefined;
    return {
      repositories: Array.isArray(repositories) ? repositories.map(row => statusOf(row as Record<string, unknown>)) : [],
      trains: new Map(newestTrains(trains.rows()).map(train => [train.repository, train])),
      deployments: newest(deployments.rows().map(deploymentOf)),
    };
  } : null, visible, query);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");
  const [ask, setAsk] = useState<Ask | null>(null);
  async function act(work: () => Promise<unknown>) {
    if (busy) return;
    setBusy(true);
    setRefusal("");
    setAsk(null);
    try {
      await work();
      read.retry();
    } catch (error) {
      setRefusal(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  }
  const start = (repository: string) => act(() => query!.pipelinesTrainExpress({ repository, pullRequests: [] }));
  const deployNow = (repository: string) => act(() => query!.pipelinesDeployRelease({ repository }));
  const resume = (repository: string) => act(() => query!.pipelinesDeploymentResume({ repository }));
  const setStanding = (repository: string, release: boolean, deploy: boolean) => act(() => query!.pipelinesTrainStanding({ repository, release, deploy }));
  // Taking the word back asks nothing. Giving it lets code reach production unread.
  function changeStanding(status: RepositoryStatus, release: boolean, deploy: boolean) {
    if ((release && !status.release) || (deploy && !status.deploy)) {
      setAsk({ kind: "standing", repository: status.repository, release, deploy });
    } else {
      void setStanding(status.repository, release, deploy);
    }
  }
  const value = read.value;
  if (!value?.repositories.length) return null;
  return <>
    <Subhead>Latest release</Subhead>
    {refusal ? <Notice tone="error" sentence={refusal} /> : null}
    <RecordList as="ul" label="What each repository's releases are doing">
      {value.repositories.map(status => {
        const train = value.trains.get(status.repository);
        const deployment = value.deployments.get(status.repository);
        const state = train ? words[train.state] ?? { word: train.state, tone: "muted" as const } : undefined;
        const url = train?.pullRequestUrl;
        const name = url ? <a href={url} target="_blank" rel="noreferrer">{status.repository}</a> : status.repository;
        const secondary = train ? [train.version, train.branch, train.departedAt ? formatMoment(train.departedAt) : ""].filter(Boolean).join(" · ") : "No release yet";
        const waitingOn = train?.state === "held" || train?.state === "awaiting_approval";
        const deployState = deployment ? deployWords[deployment.state] ?? { word: deployment.state, tone: "muted" as const } : undefined;
        const asking = ask?.repository === status.repository ? ask : null;
        return <li key={status.repository}>
          <RecordRow name={name} secondary={secondary} state={state?.word ?? ""} tone={state?.tone ?? "muted"} />
          {train?.heldReason && withReason.has(train.state) ? <Caption>{train.heldReason}</Caption> : null}
          {status.fromMain && !(train && moving.has(train.state)) ? <div className="os-panel-actions">
            {waitingOn && train
              ? <Button disabled={busy} onClick={() => setAsk({ kind: "replace", repository: status.repository, branch: train.branch })}>Replace with a new release</Button>
              : <Button disabled={busy} onClick={() => void start(status.repository)}>Start release now</Button>}
            {status.canDeploy && !(deployment && deployMoving.has(deployment.state))
              ? <Button disabled={busy} onClick={() => setAsk({ kind: "deploy", repository: status.repository, installRepository: status.installRepository })}>Deploy newest release</Button>
              : null}
          </div> : null}
          {asking?.kind === "replace" ? <Notice tone="warn" sentence={`This cancels ${asking.branch}, closes its pull request and deletes its branch, then starts a new release from main.`}>
            <InPlaceAsk className="os-refresh-row">
              <Button tone="danger" onClick={() => void start(asking.repository)}>Replace it</Button>
              <Button onClick={() => setAsk(null)}>Keep it</Button>
            </InPlaceAsk>
          </Notice> : null}
          {asking?.kind === "deploy" ? <Notice tone="warn" sentence={`This puts the newest published release on ${asking.installRepository} now, whatever the hour and whatever makes it wait. A failure goes back to what ran before, unless the release changes tables or data, when it stops and asks you.`}>
            <InPlaceAsk className="os-refresh-row">
              <Button tone="danger" onClick={() => void deployNow(asking.repository)}>Deploy it</Button>
              <Button onClick={() => setAsk(null)}>Not now</Button>
            </InPlaceAsk>
          </Notice> : null}
          {deployment && deployState ? <>
            <RecordRow name="Production" secondary={[deployment.version ? `v${deployment.version}` : "", status.installRepository].filter(Boolean).join(" · ")} state={deployState.word} tone={deployState.tone} />
            {deployment.reason ? <Caption>{deployment.reason}</Caption> : null}
            {deployment.state === "waiting" ? deployment.guards.map(guard => <Caption key={guard}>{guard}</Caption>) : null}
          </> : null}
          {status.paused ? <Notice tone="warn" sentence="Automatic deploys are paused, because what caused this is still on main. Fix it, then resume.">
            <InPlaceAsk className="os-refresh-row">
              <Button disabled={busy} onClick={() => void resume(status.repository)}>Resume automatic deploys</Button>
            </InPlaceAsk>
          </Notice> : null}
          {status.canRelease ? <>
            <SettingRow label="Publish by itself"><Switch checked={status.release} disabled={busy} onChange={next => changeStanding(status, next, next && status.deploy)}>{status.release ? "On" : "Off"}</Switch></SettingRow>
            {status.canDeploy ? <SettingRow label={status.windowStart ? `Deploy by itself, ${status.windowStart} to ${status.windowEnd} ${status.timeZone}` : "Deploy by itself"}>
              <Switch checked={status.deploy} disabled={busy || !status.release || !status.windowStart} onChange={next => changeStanding(status, status.release, next)}>{status.deploy ? "On" : "Off"}</Switch>
            </SettingRow> : null}
          </> : null}
          {asking?.kind === "standing" ? <Notice tone="warn" sentence={asking.deploy
            ? "With this on, code pushed to main reaches production overnight with nobody reading it, if its tests pass. A release that changes tables, a machine's contract or a pipeline step still waits for you."
            : "With this on, code pushed to main is tagged and published overnight with nobody reading it, if its tests pass."}>
            <InPlaceAsk className="os-refresh-row">
              <Button tone="danger" onClick={() => void setStanding(asking.repository, asking.release, asking.deploy)}>Turn it on</Button>
              <Button onClick={() => setAsk(null)}>Not now</Button>
            </InPlaceAsk>
          </Notice> : null}
        </li>;
      })}
    </RecordList>
  </>;
}
