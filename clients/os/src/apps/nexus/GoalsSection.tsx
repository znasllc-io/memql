import { AddButton } from "../../kit/AddButton";
import { useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Chip,
  Head,
  LiveList,
  Refine,
  Select,
  RecordRow as KitRow,
  listCount,
  SortControl,
  formatFreshness,
  formatMoment,
  useLiveView,
  useNow,
  type LiveListSource,
} from "../../kit";
import { LoadMore } from "../../kit/LoadMore";
import { useDebouncedValue } from "../../logs/useDebouncedValue";
import { useGoalDirectory, useGoalRunStatuses } from "./useNexusDirectory";
import { runFromRow } from "./rows";
import { NewGoal } from "./NewGoal";
import { RunMarks } from "./RunsSection";
import type { CancelGoalState, CreateGoalState } from "./actions";
import {
  goalFingerprint,
  goalFromRow,
  goalMatches,
  goalTitle,
  idTail,
  runWaitsOnYou,
  runsOfGoal,
  type GoalRow,
  type RunRow,
} from "./rows";
import { goalStatusWord, originWord } from "./words";

// GOALS: what this person asked for, and how it is going.
//
// ===========================================================================
// THE STATEMENT IS THE LOUDEST THING ON EVERY ROW
// ===========================================================================
// A goal's own name is a sentence somebody wrote, and everything else about it
// -- the origin, the status, the runs -- is metadata about that sentence. So
// the row leads with it at content size and the rest is quiet. A list that led
// with an id or a status word would make somebody read past the machine's
// vocabulary to find their own.
//
// ===========================================================================
// OPENING A GOAL REPLACES THIS LIST (rule 11, the `<- Goals` form)
// ===========================================================================
// This section used to hold the goal's detail beside the list, which was right
// when the detail was a statement, a handful of facts and a run list. It is
// not any more: a goal now opens as a MAP, a rail and a receipt, which is
// taller than the run page -- so the list goes and the goal view comes back
// with a way back to it. Two Heads in one scroller is the tell that neither
// happened.
//
// What is left here is one job done well: find the goal. Search, sort, and a
// row that says how the work is going without being opened -- whether it is
// parked on you, how its runs are doing, and when it was set.
//
// THE ASIDE IS FOR COMPOSING ONLY. Closing a goal lives on the goal view, with
// the thing it would close; it asks every run of the goal to stop, so it
// belongs on the page about the goal rather than on the page about finding
// one.

export interface GoalsSectionProps {
  goals?: LiveListSource<Row> | null;
  runs?: readonly RunRow[];
  create: CreateGoalState;
  cancel: CancelGoalState;
  /** Opens the goal view over this goal, which replaces this list. */
  onOpenGoal: (goalId: string) => void;
  /** Set by another surface (a run page's "For ...") so the row is marked. */
  selectedGoalId: string;
}

export function GoalsSection({
  goals: suppliedGoals,
  runs: suppliedRuns,
  create,
  cancel,
  onOpenGoal,
  selectedGoalId,
}: GoalsSectionProps) {
  const [includePipelines, setIncludePipelines] = useState(false);
  const [search, setSearch] = useState("");
  const [ascending, setAscending] = useState(false);
  const [composing, setComposing] = useState(false);
  const now = useNow(30_000);

  const query = useDebouncedValue(search, 200);
  const feed = useGoalDirectory(query, includePipelines, ascending, suppliedGoals === undefined);
  const goals = suppliedGoals === undefined ? feed.source : suppliedGoals;
  const statuses = useGoalRunStatuses(suppliedRuns === undefined ? (goals?.snapshot.rows ?? []).map(row => goalFromRow(row).id) : []);
  const runs = suppliedRuns ?? statuses.snapshot.rows.map(runFromRow);
  const viewKey = `goals:${includePipelines}:${ascending}:${query}`;
  const view = useLiveView<Row, GoalRow>(goals, viewKey, rows => rows.map(goalFromRow)
    .filter(goal => goal.id && (includePipelines || !["pipeline", "pipelines"].includes(goal.requestedVia)) && goalMatches(goal, query))
    .sort((a,b) => (ascending ? 1 : -1) * (a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id))));


  return (
    <div className="os-nexus-goals">
      <Head title="Goals" meta={listCount(view?.snapshot) === undefined ? undefined : `${listCount(view?.snapshot)} shown`}>
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="What you asked for"
          label="Search your goals"
        ><Select id="nexus-goal-scope" label="Include routine pipeline work" value={includePipelines ? "all" : "goals"} onChange={value => setIncludePipelines(value === "all")}><option value="goals">Goals</option><option value="all">Goals and pipeline work</option></Select></Refine>
        {/* THE HEAD'S ONE ACTION (rule 1). Everything else on this surface is
            a way of reading; this is the way of asking. */}
        <AddButton
          onClick={() => {
            setComposing(true);
            create.reset();
          }}
         label="New goal" />
      </Head>

      <div className="os-nexus-scope">
        <SortControl ascending={ascending} onToggle={() => setAscending((v) => !v)} />
      </div>

      <div className="os-nexus-split">
        <div className="os-nexus-column">
          <LiveList<GoalRow>
            source={view}
            label="Your goals"
            rowId={(goal) => goal.id}
            fingerprint={goalFingerprint}
            emptyText={
              search.trim() === ""
                ? "No goals yet. Say what you want done and the system works out how -- once."
                : "No goal matches that."
            }
            renderRow={(goal) => (
              <GoalLine
                goal={goal}
                runs={runsOfGoal(runs, goal.id)}
                now={now}
                selected={idTail(goal.id) === idTail(selectedGoalId)}
                onOpen={() => {
                  setComposing(false);
                  cancel.reset();
                  onOpenGoal(goal.id);
                }}
              />
            )}
          />
          {suppliedGoals === undefined ? <LoadMore feed={feed} /> : null}
        </div>

        {/* THE ASIDE IS ABSENT WHEN THERE IS NOTHING IN IT (rule 9: nothing
            paints half a window of dead space beside a half-width column).
            An empty panel saying "pick a goal" reserved 500px to say what a
            clickable row already says. Bin does the same. */}
        {composing ? (
          <div className="os-nexus-column os-nexus-aside">
            <NewGoal
              create={create}
              onCreated={(goalId) => {
                setComposing(false);
                // The row arrives on the feed with its own arrival cue. This
                // takes the person straight to the goal they just described,
                // which is where the work they asked for is about to appear.
                onOpenGoal(goalId);
              }}
              onCancel={() => setComposing(false)}
            />
          </div>
        ) : null}
      </div>

    </div>
  );
}

function GoalLine({
  goal,
  runs,
  now,
  selected,
  onOpen,
}: {
  goal: GoalRow;
  runs: readonly RunRow[];
  now: Date;
  selected: boolean;
  onOpen: () => void;
}) {
  const parked = runs.some(runWaitsOnYou);
  return (
    <KitRow
      name={<span className="os-nexus-goal-statement">{goalTitle(goal)}</span>}
      onOpen={onOpen}
      open={selected}
      current={goal.status === "active" || parked}
      dim={goal.status === "closed" && !parked}
      state={goalStatusWord(goal.status)}
      tone={goal.status === "active" || parked ? "accent" : "muted"}
      secondary={goal.origin === "user" ? undefined : originWord(goal.origin)}
      stateExtra={
        <>
          {/* A STANDING MARK, NOT A CUE. A goal whose run is parked stays
              marked until somebody answers it; the arrival ring decays on the
              clock and would only be seen by whoever was looking. */}
          {parked ? <Chip tone="accent">waiting for you</Chip> : null}
          <RunMarks runs={runs} />

        </>
      }
    >
      <span title={formatMoment(goal.createdAt)}>{formatFreshness(goal.createdAt, now)}</span>
    </KitRow>
  );
}

