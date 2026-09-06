import { useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Caption,
  Chip,
  Head,
  LiveList,
  Refine,
  Row as KitRow,
  SortControl,
  formatFreshness,
  formatMoment,
  useLiveView,
  useNow,
  type LiveListSource,
} from "../../kit";
import {
  goalTitle,
  idTail,
  runFingerprint,
  runFromRow,
  runIsTerminal,
  runMatches,
  runTitle,
  runWaitsOnYou,
  type GoalRow,
  type RunRow,
} from "./rows";
import { runModeDetail, runModeWord, runStatusWord, waitingWord } from "./words";

// THE RUNS LIST: every execution this person owns, newest first.
//
// ===========================================================================
// "WAITING FOR YOU" IS A STANDING MARK, NOT AN ARRIVAL CUE
// ===========================================================================
// The two are different statements and this list needs both. The arrival ring
// says "this just changed" and decays on the clock; the mark says "this run
// is stuck until you do something" and stays until somebody does it. A cue
// alone would make the news visible only to whoever happened to be looking at
// the moment it parked -- and a run can wait for days. That pairing is the
// Deployables update chip's, adopted for the same reason.
//
// ===========================================================================
// THE HEARTBEAT IS NOT IN THE FINGERPRINT, AND THAT IS THIS APP'S SHARPEST
// CASE OF THE RULE
// ===========================================================================
// A running run writes `heartbeatAt` at every step boundary and broadcasts the
// whole row each time. Fleet's heartbeat moves every fifteen seconds; this one
// moves as fast as the work does. Naming it in `runFingerprint` would ring the
// row somebody is already watching, hardest, for the whole duration of the
// run. `spent` is out for the second reason the campaigns app records: the
// counters must RE-RENDER live and must not RING, and the fingerprint is the
// only thing that separates those.

export interface RunsSectionProps {
  runs: LiveListSource<Row> | null;
  goalsById: Map<string, GoalRow>;
  showFinished: boolean;
  onOpenRun: (runId: string) => void;
}

export function RunsSection({ runs, goalsById, showFinished, onOpenRun }: RunsSectionProps) {
  const [search, setSearch] = useState("");
  const [ascending, setAscending] = useState(false);
  const now = useNow(15_000);

  // THE VIEW KEY RE-BASELINES THE CUE WHEN THE QUESTION CHANGES. Revealing
  // rows the browser already had is not the cluster sending them, so flipping
  // "show finished" must not announce every finished run as new.
  const viewKey = `runs:${showFinished ? "all" : "live"}:${ascending ? "asc" : "desc"}:${search.trim().toLowerCase()}`;
  const view = useLiveView<Row, RunRow>(runs, viewKey, (rows) => {
    const projected = rows
      .map(runFromRow)
      .filter((run) => run.id !== "")
      .filter((run) => showFinished || !runIsTerminal(run))
      .filter((run) => runMatches(run, search));
    projected.sort((a, b) => {
      const left = a.createdAt;
      const right = b.createdAt;
      return ascending ? left.localeCompare(right) : right.localeCompare(left);
    });
    return projected;
  });

  const count = view?.snapshot.rows.length ?? 0;
  const parked = useMemo(
    () => (view?.snapshot.rows ?? []).filter(runWaitsOnYou).length,
    [view?.snapshot],
  );

  return (
    <div className="os-work-list">
      <Head
        title="Runs"
        meta={
          // A COUNT OF WHAT IS STUCK, NOT AN UNREAD BADGE. It is derived from
          // the rows on screen and it goes away when the last one is answered
          // -- there is nothing to dismiss.
          parked > 0 ? `${count} runs -- ${parked} waiting for you` : `${count} runs`
        }
      >
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="Automation, status or mode"
          label="Search your runs"
        />
      </Head>

      <div className="os-work-scope">
        <SortControl ascending={ascending} onToggle={() => setAscending((v) => !v)} />
      </div>

      <LiveList<RunRow>
        source={view}
        label="Your runs"
        rowId={(run) => run.id}
        fingerprint={runFingerprint}
        emptyText={
          search.trim() !== ""
            ? "No run matches that."
            : showFinished
              ? "No runs yet. A goal opens one the moment it is accepted."
              : "Nothing is running. Finished runs are hidden -- the setting is in this app's Settings."
        }
        renderRow={(run) => (
          <RunLine
            run={run}
            goal={goalsById.get(idTail(run.goalId)) ?? null}
            now={now}
            onOpen={() => onOpenRun(run.id)}
          />
        )}
      />
    </div>
  );
}

/**
 * One run, as a line.
 *
 * Exported because the goal's detail lists the same thing and a second
 * spelling of a run row is a second answer to "what does a run look like".
 */
export function RunLine({
  run,
  goal,
  now,
  onOpen,
  showContext = true,
}: {
  run: RunRow;
  goal: GoalRow | null;
  now: Date;
  onOpen: () => void;
  /**
   * Whether the row says what the run is FOR.
   *
   * SAY IT ONCE (rule 7). On the Runs list it is the most useful thing on the
   * row -- a run's own name is an automation name and the goal's statement is
   * why it exists. Inside a goal's own detail it repeats the heading two
   * inches above it, and it was doing so at the cost of the run's name, which
   * truncated to "re...".
   */
  showContext?: boolean;
}) {
  const parked = runWaitsOnYou(run);
  const moving = run.status === "running" || run.status === "compiling";
  return (
    <KitRow
      name={<span className="os-work-run-name">{runTitle(run)}</span>}
      onOpen={onOpen}
      // `current` takes a row from muted to full ink: a run that is moving or
      // one that is stuck on you is what this list is about, and everything
      // else is history.
      current={moving || parked}
      dim={runIsTerminal(run) && !parked}
      state={
        <>
          {parked ? (
            <Chip tone="accent" title={waitingWord(run.waitingOnKind)}>
              waiting for you
            </Chip>
          ) : null}
          {run.mode === "live" ? null : (
            <Chip tone="muted" title={runModeDetail(run.mode)}>
              {runModeWord(run.mode)}
            </Chip>
          )}
          <span className="os-work-run-status" data-status={run.status}>
            {runStatusWord(run.status)}
          </span>
          <span className="os-caption" title={formatMoment(run.startedAt || run.createdAt)}>
            {formatFreshness(run.startedAt || run.createdAt, now)}
          </span>
        </>
      }
    >
      {showContext ? (
        <span className="os-work-run-for">
          {goal === null ? (run.goalId === "" ? "an automation" : "a goal") : goalTitle(goal)}
        </span>
      ) : null}
    </KitRow>
  );
}

/**
 * A goal's runs as marks -- the compact form, for a list row.
 *
 * Each mark carries its own accessible name, so the strip reads as "run 1,
 * done; run 2, waiting for you" rather than as decoration. It is the same
 * shape the Deployables list row's compact rail takes, and for the same
 * reason: a row should be readable as the thing it opens into.
 */
export function RunMarks({ runs }: { runs: readonly RunRow[] }) {
  if (runs.length === 0) return <Caption>no runs yet</Caption>;
  return (
    <span className="os-work-marks" role="list" aria-label="Runs of this goal">
      {runs.map((run) => (
        <span
          key={run.id}
          role="listitem"
          className="os-work-mark"
          data-status={run.status}
          data-parked={runWaitsOnYou(run) || undefined}
          aria-label={`${runTitle(run)}: ${runStatusWord(run.status)}`}
          title={`${runTitle(run)} -- ${runStatusWord(run.status)}`}
        />
      ))}
    </span>
  );
}
