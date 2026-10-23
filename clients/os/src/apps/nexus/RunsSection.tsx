import { flatten } from "../../kit/rows";
import { useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Chip,
  Head,
  LiveList,
  Refine,
  RecordRow as KitRow,
  listCount,
  SortControl,
  formatFreshness,
  formatMoment,
  useLiveView,
  useNow,
  type LiveListSource,
} from "../../kit";
import {
  runFingerprint,
  runFromRow,
  runIsTerminal,
  runMatches,
  runTitle,
  runWaitsOnYou,
  type RunRow,
} from "./rows";
import { runModeDetail, runModeWord, runStatusWord } from "./words";

// THE RUNS LIST: every execution this person owns, newest first.
//
// Waiting is stated once in each row; urgency still informs its emphasis.
//
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
  showFinished: boolean;
  onOpenRun: (runId: string) => void;
  conversationId?: string;
  onClearConversation?: () => void;
}

export function RunsSection({ runs, showFinished, onOpenRun, conversationId, onClearConversation }: RunsSectionProps) {
  const [search, setSearch] = useState("");
  const [ascending, setAscending] = useState(false);
  const now = useNow(15_000);

  // THE VIEW KEY RE-BASELINES THE CUE WHEN THE QUESTION CHANGES. Revealing
  // rows the browser already had is not the cluster sending them, so flipping
  // "show finished" must not announce every finished run as new.
  const viewKey = `runs:${conversationId ?? ""}:${showFinished ? "all" : "live"}:${ascending ? "asc" : "desc"}:${search.trim().toLowerCase()}`;
  const view = useLiveView<Row, RunRow>(runs, viewKey, (rows) => {
    const projected = rows
      .filter(row => !conversationId || conversationOf(row) === conversationId)
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


  const parked = (view?.snapshot.rows ?? []).filter(runWaitsOnYou).length;

  return (
    <div className="os-nexus-list">
      <Head
        title="Runs"
        meta={listCount(view?.snapshot) === undefined ? undefined : `${listCount(view?.snapshot)}${parked > 0 ? ` · ${parked} waiting for you` : ""}`}
      >
        <SortControl ascending={ascending} onToggle={() => setAscending((v) => !v)} />
        <Refine
          chips={conversationId ? [{ id: "conversation", label: "This conversation", onRemove: () => onClearConversation?.() }] : []}
          search={search}
          onSearch={setSearch}
          placeholder="Automation, status or mode"
          label="Search your runs"
        />
      </Head>

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
            now={now}
            onOpen={() => onOpenRun(run.id)}
          />
        )}
      />
    </div>
  );
}

/** A run's generated task summary and one lifecycle status. */
export function RunLine({
  run,
  now,
  onOpen,
}: {
  run: RunRow;
  now: Date;
  onOpen: () => void;
}) {
  const parked = runWaitsOnYou(run);
  const moving = run.status === "running" || run.status === "compiling";
  return (
    <KitRow
      name={<span className="os-nexus-run-name">{runTitle(run)}</span>}
      onOpen={onOpen}
      // `current` takes a row from muted to full ink: a run that is moving or
      // one that is stuck on you is what this list is about, and everything
      // else is history.
      current={moving || parked}
      dim={runIsTerminal(run) && !parked}
      state={runStatusWord(run.status)}
      tone={moving || parked ? "accent" : "muted"}
      secondary={undefined}
      stateExtra={
        <>
          {run.mode === "live" ? null : (
            <Chip tone="muted" title={runModeDetail(run.mode)}>
              {runModeWord(run.mode)}
            </Chip>
          )}

        </>
      }
    >
      <span title={formatMoment(run.startedAt || run.createdAt)}>{formatFreshness(run.startedAt || run.createdAt, now)}</span>
    </KitRow>
  );
}

/**
 * A goal's runs as marks -- the compact form, for a list row.
 *
 * Each mark carries its own accessible name, so the strip reads as "run 1,
 * done; run 2, waiting" rather than as decoration. It is the same
 * shape the Deployables list row's compact rail takes, and for the same
 * reason: a row should be readable as the thing it opens into.
 */
export function RunMarks({ runs }: { runs: readonly RunRow[] }) {
  if (runs.length === 0) return null;
  return (
    <span className="os-nexus-marks" role="list" aria-label="Runs of this goal">
      {runs.map((run) => (
        <span
          key={run.id}
          role="listitem"
          className="os-nexus-mark"
          data-status={run.status}
          data-parked={runWaitsOnYou(run) || undefined}
          aria-label={`${runTitle(run)}: ${runStatusWord(run.status)}`}
          title={`${runTitle(run)} -- ${runStatusWord(run.status)}`}
        />
      ))}
    </span>
  );
}

function conversationOf(row: Row): string {
  const flat = flatten(row);
  const conversation = flat.conversation as { id?: string } | undefined;
  const input = flat.input as { conversation?: { id?: string } } | undefined;
  return conversation?.id ?? input?.conversation?.id ?? "";
}
