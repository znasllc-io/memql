import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ListChecks } from "lucide-react";

import { Button, EmptyState, Head, Notice, Refine, Select, useNow, type RefineChip } from "../../../kit";
import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { RecordRow } from "../../../kit/RecordRow";
import { useOsConnection } from "../../../live/connection";
import type { LiveView } from "../../../live/liveView";
import { useArrivals } from "../../../live/useArrivals";
import type { OsAppProps } from "../../../system/registry";
import { sourceName } from "../list";
import type { PackageRow } from "../packages/rows";
import type { PartsHeld } from "../parts";
import { Mark } from "./Mark";
import { Phrases } from "./Phrases";
import { isFinished, runFromRow, runRingFingerprint, sameId, type PipelineRow, type RunRow } from "./rows";
import {
  branchesOf, filterIsNarrowing, filterRuns, groupByDay, NO_RUN_FILTER, OUTCOME_FACETS, type OutcomeKey, type RunFilter,
} from "./runs";
import { RunPage } from "./RunPage";
import { branchWords, runOutcome, runTitle, shortSha, timeOfDay, triggerWords } from "./words";
import "./pipelines.css";

// RUNS (epic memql#5479, D12/D14; issue memql#5499): every run of every
// source this person connected, newest first, grouped by day, one line each.
//
// A THIRD NOUN, NOT A FILTER: a run is a thing with a page of its own, which a
// source's Checks row and a GitHub check's details link both open. Narrowing
// to a source, a branch or an outcome is a question asked of this list, so it
// lives behind Refine (DESIGN.md rule 2), never on a tab.
//
// THE ROW: the commit's message; then source, branch, commit and what opened
// it; then the outcome with its stage and how long; then the time. A refused
// fork and a push that could not notify read as outcomes in words.
//
// THE ARRIVAL CUE ON A TERMINAL STATE ONLY. A run is opened queued and moves
// for minutes: ringing on its arrival and on every stage would be a strobe the
// cue exists not to be. So the cue is fed the FINISHED runs alone -- a run
// enters that set the moment it has an answer, and that is the moment it rings.
// The cue's machinery is LiveList's own (`useArrivals`), with the rows drawn
// here because they are grouped under day headings, which LiveList does not
// draw.
//
// THE LIVE WINDOW AND THE PAST. The root retains the newest runs (the first
// pages of `pipelineRunsForOwner`) live; "Show older runs" reads the next
// page once, on request, and appends it. Older runs are finished runs and do
// not move, and a re-run of one arrives at the top of the live window.

type RunsView = { kind: "list" } | { kind: "run"; runId: string };

export interface RunsSectionProps {
  active: boolean;
  navigation?: OsAppProps["navigation"];
  runs: LiveView<RunRow> | null;
  /** The live window's cursor, and its re-read. */
  feed: { olderCursor: () => string; reseed: () => void };
  pipelines: readonly PipelineRow[];
  packages: readonly PackageRow[];
  openRequest?: { runId: string; revision: number };
  /** Refine to one source: a source page's All runs. */
  filterRequest?: { pipelineId: string; revision: number };
  can: PartsHeld;
  onOpenSource: (packageId: string) => void;
  onOpenSources: () => void;
}

export function RunsSection(props: RunsSectionProps) {
  const { active, navigation, runs, pipelines, packages, openRequest, filterRequest } = props;
  const [view, setView] = useState<RunsView>({ kind: "list" });
  const [filter, setFilter] = useState<RunFilter>(NO_RUN_FILTER);
  const lastNavigation = useRef<number | undefined>(undefined);
  const lastOpen = useRef<number | undefined>(undefined);
  const lastFilter = useRef<number | undefined>(undefined);

  // A TAB IS A CLEAN PEER DESTINATION; a link carries where it was going.
  useLayoutEffect(() => {
    if (!active) return;
    if (openRequest && openRequest.revision !== lastOpen.current) {
      lastOpen.current = openRequest.revision;
      lastNavigation.current = navigation?.revision;
      setView({ kind: "run", runId: openRequest.runId });
      return;
    }
    if (filterRequest && filterRequest.revision !== lastFilter.current) {
      lastFilter.current = filterRequest.revision;
      lastNavigation.current = navigation?.revision;
      setFilter({ ...NO_RUN_FILTER, pipelineId: filterRequest.pipelineId });
      setView({ kind: "list" });
      return;
    }
    if (navigation && navigation.revision !== lastNavigation.current) {
      lastNavigation.current = navigation.revision;
      if (navigation.origin === "peer") setView({ kind: "list" });
    }
  }, [active, navigation?.revision, navigation?.origin, openRequest?.revision, filterRequest?.revision]);

  const snapshot = runs?.snapshot ?? null;
  const live = snapshot?.rows ?? [];
  const [older, setOlder] = useState<RunRow[]>([]);
  const all = useMemo(() => {
    if (older.length === 0) return live;
    const held = new Set(live.map((r) => r.id));
    return [...live, ...older.filter((r) => !held.has(r.id))];
  }, [live, older]);

  // A run's source is its pipeline's package: one pipeline per source (D12).
  const pipelineName = (pipeline: PipelineRow): string => {
    const pkg = packages.find((p) => sameId(p.id, pipeline.packageId));
    return pkg ? sourceName(pkg) : pipeline.repository;
  };
  const sourceOf = (run: RunRow): { name: string; packageId: string } => {
    const pipeline = pipelines.find((p) => sameId(p.id, run.pipelineId));
    return { name: pipeline ? pipelineName(pipeline) : run.repository, packageId: pipeline?.packageId ?? "" };
  };

  if (view.kind === "run") {
    const run = all.find((r) => sameId(r.id, view.runId)) ?? null;
    const source = run ? sourceOf(run) : { name: "", packageId: "" };
    const pipeline = run ? pipelines.find((p) => sameId(p.id, run.pipelineId)) ?? null : null;
    const toList = () => setView({ kind: "list" });
    return (
      <RunPage
        key={view.runId}
        runId={view.runId}
        runs={all}
        runsState={snapshot?.state ?? "disconnected"}
        pipeline={pipeline}
        sourceName={source.name}
        breadcrumbs={[
          { label: "Runs", onSelect: toList },
          ...(source.name !== "" ? [{ label: source.name, ...(source.packageId !== "" ? { onSelect: () => props.onOpenSource(source.packageId) } : {}) }] : []),
          { label: run ? runTitle(run) : "Run" },
        ]}
        back={{ label: "Runs", onSelect: toList }}
        can={props.can}
        onOpenRun={(runId) => setView({ kind: "run", runId })}
      />
    );
  }

  return <RunsList {...props} all={all} older={older} setOlder={setOlder} filter={filter} setFilter={setFilter} sourceOf={sourceOf}
    pipelineName={pipelineName} onOpen={(runId) => setView({ kind: "run", runId })} />;
}

function RunsList({ runs, feed, pipelines, all, older, setOlder, filter, setFilter, sourceOf, pipelineName, onOpen, onOpenSources }: RunsSectionProps & {
  all: readonly RunRow[];
  older: readonly RunRow[];
  setOlder: (rows: RunRow[]) => void;
  filter: RunFilter;
  setFilter: (f: RunFilter) => void;
  sourceOf: (run: RunRow) => { name: string; packageId: string };
  pipelineName: (pipeline: PipelineRow) => string;
  onOpen: (runId: string) => void;
}) {
  const connection = useOsConnection();
  const now = useNow(60_000);
  const snapshot = runs?.snapshot ?? null;
  const state = snapshot?.state ?? "disconnected";
  const settled = state === "live" && !snapshot?.error;

  // The cue, fed the finished runs alone (see the header).
  const finished = useMemo(() => snapshot ? { ...snapshot, rows: snapshot.rows.filter(isFinished) } : { rows: [] as RunRow[], state: "disconnected" as const, error: "", version: 0 },
    [snapshot]);
  const ticks = useArrivals(finished, (r) => r.id, runRingFingerprint);
  const seen = useRef<Set<string>>(new Set());
  useEffect(() => { for (const r of all) seen.current.add(r.id); });

  const filtered = filterRuns(all, filter);
  const days = groupByDay(filtered, now);
  const narrowing = filterIsNarrowing(filter);

  // OLDER RUNS, one page per request, after the live window or the last page read.
  const [olderCursor, setOlderCursor] = useState<string | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState("");
  const cursor = olderCursor ?? feed.olderCursor();
  async function loadOlder() {
    if (!connection || cursor === "") return;
    setLoadingOlder(true);
    setOlderError("");
    try {
      const result = await connection.query.pipelineRunsForOwner({}, { cursor });
      const page = result.rows().map(runFromRow).filter((r) => r.id !== "");
      setOlder([...older, ...page]);
      setOlderCursor(result.meta()?.cursor ?? "");
    } catch (err) {
      setOlderError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingOlder(false);
    }
  }

  const chips: RefineChip[] = [];
  if (filter.pipelineId !== "") {
    const pipeline = pipelines.find((p) => sameId(p.id, filter.pipelineId));
    chips.push({ id: "source", label: pipeline ? pipelineName(pipeline) : "Source", onRemove: () => setFilter({ ...filter, pipelineId: "" }) });
  }
  if (filter.branch !== "") chips.push({ id: "branch", label: filter.branch, onRemove: () => setFilter({ ...filter, branch: "" }) });
  if (filter.outcome !== "") {
    chips.push({ id: "outcome", label: OUTCOME_FACETS.find((o) => o.value === filter.outcome)?.label ?? filter.outcome, onRemove: () => setFilter({ ...filter, outcome: "" }) });
  }
  const branches = branchesOf(all);

  return (
    <div className="os-app-stack pipeline-runs" data-os-page-context={JSON.stringify({ page: "Runs" })}>
      <Head title="Runs" meta={settled ? filtered.length : undefined}>
        <Refine iconOnly search={filter.search} onSearch={(search) => setFilter({ ...filter, search })} chips={chips} label="Refine runs">
          <Select id="runs-source" label="Source" value={filter.pipelineId} onChange={(pipelineId) => setFilter({ ...filter, pipelineId })}>
            <option value="">Any source</option>
            {pipelines.map((p) => (
              <option key={p.id} value={p.id}>{pipelineName(p)}</option>
            ))}
          </Select>
          <Select id="runs-branch" label="Branch" value={filter.branch} onChange={(branch) => setFilter({ ...filter, branch })}>
            <option value="">Any branch</option>
            {branches.map((b) => <option key={b} value={b}>{b}</option>)}
          </Select>
          <Select id="runs-outcome" label="Outcome" value={filter.outcome} onChange={(outcome) => setFilter({ ...filter, outcome: outcome as OutcomeKey | "" })}>
            <option value="">Any outcome</option>
            {OUTCOME_FACETS.map((o) => <option key={o.value} value={o.value}>{o.label}</option>)}
          </Select>
        </Refine>
      </Head>

      {snapshot?.error ? (
        <Notice tone="error" sentence="Runs could not be loaded." detail={snapshot.error} next="Check your connection; the list retries on its own.">
          <Button tone="quiet" onClick={feed.reseed}>Try again</Button>
        </Notice>
      ) : null}

      {runs === null || (state !== "live" && all.length === 0) ? (
        state === "seeding" ? <RecordListSkeleton label="Loading runs" rows={6} /> : (
          <EmptyState icon={ListChecks} title="Not connected to the cluster">Runs appear when the connection returns.</EmptyState>
        )
      ) : filtered.length === 0 ? (
        narrowing ? (
          <EmptyState icon={ListChecks} title="No matching runs" action={<Button onClick={() => setFilter(NO_RUN_FILTER)}>Clear filters</Button>}>
            Try another source, branch or outcome.
          </EmptyState>
        ) : (
          <EmptyState icon={ListChecks} title="No runs yet" action={<Button onClick={onOpenSources}>Open Sources</Button>}>
            A run starts with the next push or pull request to a source whose pipeline is connected. Connect one from a source's page.
          </EmptyState>
        )
      ) : (
        <div className="pipeline-runs-days">
          {days.map((day) => (
            <section key={day.key} className="pipeline-runs-day" aria-label={day.label}>
              <h4 className="pipeline-runs-day-label">{day.label}</h4>
              <ul className="os-livelist-rows os-record-list" data-density="comfortable" aria-label={`Runs, ${day.label}`}>
                {day.runs.map((run) => {
                  const tick = ticks.get(run.id)?.kind ?? null;
                  const arrival = tick === null ? undefined : seen.current.has(run.id) ? "updated" : tick;
                  return (
                    <li key={run.id} className="os-livelist-row" data-arrival={arrival}>
                      <RunLine run={run} source={sourceOf(run).name} onOpen={() => onOpen(run.id)} />
                    </li>
                  );
                })}
              </ul>
            </section>
          ))}
        </div>
      )}

      {settled && cursor !== "" && filtered.length > 0 ? (
        <div className="pipeline-runs-more">
          <button type="button" className="os-sort" onClick={() => void loadOlder()} disabled={loadingOlder} aria-busy={loadingOlder || undefined}>
            Show older runs
          </button>
          {olderError !== "" ? <span className="os-caption" role="status">{olderError}</span> : null}
        </div>
      ) : null}
    </div>
  );
}

function RunLine({ run, source, onOpen }: { run: RunRow; source: string; onOpen: () => void }) {
  const outcome = runOutcome(run);
  const title = runTitle(run);
  const branch = branchWords(run);
  return (
    <RecordRow
      icon={<Mark state={outcome.mark} />}
      name={title}
      secondary={
        <span className="pipeline-run-facts">
          <span>{source}</span>
          {branch !== "" ? <span>{branch}</span> : null}
          <span className="pipeline-mono">{shortSha(run.sha)}</span>
          <span>{triggerWords(run)}</span>
        </span>
      }
      state={outcome.word}
      tone={outcome.tone}
      trailing={<time className="pipeline-run-time" dateTime={run.queuedAt}>{timeOfDay(run.queuedAt)}</time>}
      current={outcome.mark === "current"}
      label={`Open ${title}: ${source}${branch ? `, ${branch}` : ""}, ${outcome.word.toLowerCase()}${outcome.detail ? ` ${outcome.detail}` : ""}`}
      onOpen={onOpen}
    >
      {outcome.detail !== "" ? <span className="pipeline-run-detail"><Phrases text={outcome.detail} /></span> : null}
    </RecordRow>
  );
}
