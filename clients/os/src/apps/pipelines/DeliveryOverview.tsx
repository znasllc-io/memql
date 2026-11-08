import { useState } from "react";
import { usePipelineRuns } from "../deployables/pipelines/feeds";
import { NO_RUN_FILTER } from "../deployables/pipelines/runs";
import { runFromRow } from "../deployables/pipelines/rows";
import { useDebouncedValue } from "../../logs/useDebouncedValue";
import { LoadMore } from "../../kit/LoadMore";
import { Refine } from "../../kit";
import { Overview } from "../../kit/Overview";
import { Caption, Head, Notice, RecordList, RecordRow, Subhead } from "../../kit";
import { absent, figureOf } from "../../kit/measure";
import type { PipelineRow, RunRow } from "../deployables/pipelines/rows";
import { runOutcome, runTitle, triggerWords } from "../deployables/pipelines/words";

export function latestChanges(runs: readonly RunRow[]): RunRow[] {
  const changes = new Map<string, RunRow>();
  for (const run of runs) {
    if (run.event !== "pull_request" && run.event !== "merge_group") continue;
    const key = `${run.repository}:${run.pullRequest || run.headBranch || run.sha}`;
    const previous = changes.get(key);
    if (!previous || run.queuedAt > previous.queuedAt) changes.set(key, run);
  }
  return [...changes.values()].sort((a, b) => b.queuedAt.localeCompare(a.queuedAt));
}

export function DeliveryOverview({ pipelines, runs, ready, error, onOpenRun, onRepositories, onRuns, onReleases }: {
  pipelines: readonly PipelineRow[]; runs: readonly RunRow[]; ready: boolean; error: string;
  onOpenRun: (id: string) => void; onRepositories?: () => void; onRuns?: () => void; onReleases?: () => void;
}) {
  const active = runs.filter(r => r.status !== "completed");
  const failed = runs.filter(r => r.status === "completed" && ["failure", "refused"].includes(r.conclusion));
  const figures = [["Connected workflows", pipelines.length], ["In progress", active.length], ["Changes", latestChanges(runs).length], ["Failed attempts", failed.length]] as const;
  return <Overview metrics={figures.map(([label, value]) => ({ label, figure: ready ? figureOf(value) : absent(error ? "failed" : "unread"), loading: !ready && !error }))}>
    <Caption>Run figures describe recent activity available to your account.</Caption>
    {error ? <Notice tone="warn" sentence="Delivery information could not be refreshed." detail={error} /> : null}
    <section><Subhead>In progress</Subhead><RecordList as="ul" label="Active pipeline runs">
      {active.slice(0, 8).map(run => <RecordRow key={run.id} name={runTitle(run)} secondary={run.repository} state={runOutcome(run).word} onOpen={() => onOpenRun(run.id)} />)}
    </RecordList>{ready && !active.length ? <Caption>No active runs in recent activity.</Caption> : null}</section>
    <section><Subhead>Delivery</Subhead><RecordList as="ul" label="Delivery destinations">
      {onRepositories ? <RecordRow name="Repositories" secondary="Sources and their declared workflows" onOpen={onRepositories} /> : null}
      {onRuns ? <RecordRow name="Run history" secondary="Verification, builds and recorded outcomes" onOpen={onRuns} /> : null}
      {onReleases ? <RecordRow name="Releases" secondary="Prepared candidates and publication evidence" onOpen={onReleases} /> : null}
    </RecordList></section>
  </Overview>;
}

export function ChangesSection({ runs, ready, error, onOpenRun }: { runs: readonly RunRow[]; ready: boolean; error: string; onOpenRun: (id: string) => void }) {
  const [search, setSearch] = useState("");
  const query = useDebouncedValue(search, 250);
  const feed = usePipelineRuns(true, { ...NO_RUN_FILTER, changesOnly: true, search: query });
  const scoped = feed.source !== null;
  const changes = latestChanges(scoped ? feed.snapshot.rows.map(runFromRow) : runs);
  ready = scoped ? feed.snapshot.state === "live" && !feed.snapshot.error : ready;
  error = scoped ? feed.snapshot.error : error;
  return <div className="os-app-stack"><Head title="Changes" meta={ready ? `${changes.length}${feed.hasMore ? "+" : ""} recent` : undefined}>
    <Refine search={search} onSearch={setSearch} label="Search changes" placeholder="Title, repository or branch" />
  </Head>
    {error ? <Notice tone="warn" sentence="Changes could not be refreshed." detail={error} /> : null}
    <RecordList as="ul" label="Repository changes">{changes.map(run => <RecordRow key={run.id} name={runTitle(run)} secondary={`${run.repository}${run.pullRequest ? ` · #${run.pullRequest}` : ""}`} state={runOutcome(run).word} onOpen={() => onOpenRun(run.id)}><span>{triggerWords(run)}</span></RecordRow>)}</RecordList>
    <LoadMore feed={feed} />
    {ready && !changes.length ? <Caption>No pull requests or merge-queue attempts in recent activity.</Caption> : null}
  </div>;
}
