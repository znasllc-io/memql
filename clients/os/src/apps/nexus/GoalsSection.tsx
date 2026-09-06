import { useMemo, useState } from "react";
import { Plus } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Button,
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
  goals: LiveListSource<Row> | null;
  runs: readonly RunRow[];
  create: CreateGoalState;
  cancel: CancelGoalState;
  /** Opens the goal view over this goal, which replaces this list. */
  onOpenGoal: (goalId: string) => void;
  /** Set by another surface (a run page's "For ...") so the row is marked. */
  selectedGoalId: string;
}

export function GoalsSection({
  goals,
  runs,
  create,
  cancel,
  onOpenGoal,
  selectedGoalId,
}: GoalsSectionProps) {
  const [search, setSearch] = useState("");
  const [ascending, setAscending] = useState(false);
  const [composing, setComposing] = useState(false);
  const now = useNow(30_000);

  // ==========================================================================
  // LIVE WORK ON TOP, THEN THE SORT
  // ==========================================================================
  // A plain newest-first sort put the goal that was PARKED ON A PERSON at the
  // bottom of the list, under two that wanted nothing -- which was visible the
  // first time this was rendered with three goals in it. The question a person
  // opens this list with is "what is happening", and a goal whose run is in
  // flight or waiting on them is the answer; how recently it was set is the
  // tie-break, not the ordering.
  //
  // TWO BANDS, NOT A SCORE. Live work, then everything else, each band ordered
  // by the sort control -- so the control still does what it says inside each
  // band and a goal never jumps the queue for being new.
  // The view caches its projection on the GOALS snapshot, so a sort that also
  // reads the RUNS feed would go stale the moment a run parked -- the list
  // would keep its old order until a goal row happened to change. The key
  // carries the live SET, which moves only when a goal gains or loses work in
  // flight; `running` -> `waiting` is inside the same set and re-baselines
  // nothing, which is what keeps the arrival cue off a heartbeat.
  const liveToken = useMemo(
    () =>
      runs
        .filter(
          (run) =>
            run.status === "running" || run.status === "waiting" || run.status === "compiling",
        )
        .map((run) => idTail(run.goalId))
        .sort()
        .join(","),
    [runs],
  );
  const viewKey = `goals:${ascending ? "asc" : "desc"}:${search.trim().toLowerCase()}:${liveToken}`;
  const view = useLiveView<Row, GoalRow>(goals, viewKey, (rows) => {
    const projected = rows
      .map(goalFromRow)
      .filter((goal) => goal.id !== "")
      .filter((goal) => goalMatches(goal, search));
    projected.sort((a, b) => {
      const band = Number(isLive(b, runs)) - Number(isLive(a, runs));
      if (band !== 0) return band;
      return ascending
        ? a.createdAt.localeCompare(b.createdAt)
        : b.createdAt.localeCompare(a.createdAt);
    });
    return projected;
  });

  const rows = view?.snapshot.rows ?? [];

  return (
    <div className="os-nexus-goals">
      <Head title="Goals" meta={`${rows.length} ${rows.length === 1 ? "goal" : "goals"}`}>
        <Refine
          search={search}
          onSearch={setSearch}
          placeholder="What you asked for"
          label="Search your goals"
        />
        {/* THE HEAD'S ONE ACTION (rule 1). Everything else on this surface is
            a way of reading; this is the way of asking. */}
        <Button
          tone="primary"
          onClick={() => {
            setComposing(true);
            create.reset();
          }}
        >
          <Plus size={13} aria-hidden />
          New goal
        </Button>
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

/**
 * Whether this goal is work in flight.
 *
 * Read off the RUNS rather than off the goal's own status, deliberately.
 * `goal.status` is coarse -- `active` means "a run is in flight" and is not
 * re-written when that run parks -- so a goal waiting on a person can be
 * `active`, `open` or neither. The runs are what actually say.
 */
function isLive(goal: GoalRow, runs: readonly RunRow[]): boolean {
  if (goal.status === "closed") return false;
  const mine = runsOfGoal(runs, goal.id);
  return mine.some(
    (run) => run.status === "running" || run.status === "waiting" || run.status === "compiling",
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
      state={
        <>
          {/* A STANDING MARK, NOT A CUE. A goal whose run is parked stays
              marked until somebody answers it; the arrival ring decays on the
              clock and would only be seen by whoever was looking. */}
          {parked ? <Chip tone="accent">waiting for you</Chip> : null}
          <RunMarks runs={runs} />
          <span className="os-nexus-goal-status" data-status={goal.status}>
            {goalStatusWord(goal.status)}
          </span>
          <span className="os-caption" title={formatMoment(goal.createdAt)}>
            {formatFreshness(goal.createdAt, now)}
          </span>
        </>
      }
    >
      {/* SAID ONLY WHEN IT IS NOT YOURS (rule 7, say it once). "You asked for
          this" on a list of your own goals is a label on every row carrying
          no information, and it was crowding out the state cluster beside it.
          A goal a responsibility or the platform raised is the case worth
          marking. */}
      {goal.origin === "user" ? null : (
        <span className="os-nexus-goal-origin">{originWord(goal.origin)}</span>
      )}
    </KitRow>
  );
}

