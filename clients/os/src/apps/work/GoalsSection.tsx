import { useMemo, useState } from "react";
import { Plus } from "lucide-react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import {
  Button,
  Caption,
  Chip,
  Chips,
  Fact,
  Facts,
  Head,
  Input,
  LiveList,
  Notice,
  Panel,
  Refine,
  Row as KitRow,
  SortControl,
  Subhead,
  formatFreshness,
  formatMoment,
  useLiveView,
  useNow,
  type LiveListSource,
} from "../../kit";
import { ActionBar, type Act, type ActionBarTone } from "../../kit/ActionBar";
import { NewGoal } from "./NewGoal";
import { RunLine, RunMarks } from "./RunsSection";
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
import { goalStatusDetail, goalStatusWord, originWord } from "./words";

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
// LIST BESIDE DETAIL, WITH THE ACTS ON ONE BAR (rules 11 and 12)
// ===========================================================================
// A goal's detail is short -- a statement, a handful of facts and its runs --
// so this is the beside-the-list form of rule 11 rather than the replace-it
// form the run timeline takes. Each column scrolls on its own.
//
// The one act with consequences, closing the goal, is on the bar and NOWHERE
// ELSE, and it takes a typed confirmation because it asks every run of the
// goal to stop. `cancelGoal` is deliberately not offered from a RUN page: it
// would be a control that stops this run's siblings, sitting on a page about
// one of them, which is the shape the Deployables recomposition removed.

export interface GoalsSectionProps {
  goals: LiveListSource<Row> | null;
  runs: readonly RunRow[];
  create: CreateGoalState;
  cancel: CancelGoalState;
  onOpenRun: (runId: string) => void;
  /** Set by another surface (a run page's "For ...") so the goal opens here. */
  selectedGoalId: string;
  onSelectGoal: (goalId: string) => void;
}

export function GoalsSection({
  goals,
  runs,
  create,
  cancel,
  onOpenRun,
  selectedGoalId,
  onSelectGoal,
}: GoalsSectionProps) {
  const [search, setSearch] = useState("");
  const [ascending, setAscending] = useState(false);
  const [composing, setComposing] = useState(false);
  const [confirm, setConfirm] = useState("");
  const now = useNow(30_000);

  const viewKey = `goals:${ascending ? "asc" : "desc"}:${search.trim().toLowerCase()}`;
  const view = useLiveView<Row, GoalRow>(goals, viewKey, (rows) => {
    const projected = rows
      .map(goalFromRow)
      .filter((goal) => goal.id !== "")
      .filter((goal) => goalMatches(goal, search));
    projected.sort((a, b) =>
      ascending ? a.createdAt.localeCompare(b.createdAt) : b.createdAt.localeCompare(a.createdAt),
    );
    return projected;
  });

  const rows = view?.snapshot.rows ?? [];
  const selected = rows.find((goal) => idTail(goal.id) === idTail(selectedGoalId)) ?? null;
  const selectedRuns = useMemo(
    () => (selected === null ? [] : runsOfGoal(runs, selected.id)),
    [runs, selected],
  );

  // Closing is the only act with consequences, and it is absent on a goal that
  // is already closed -- an illegal act is ABSENT, never disabled (rule 12).
  const closable = selected !== null && selected.status !== "closed";
  const confirmed = selected !== null && confirm.trim() === goalTitle(selected).trim();
  const acts: Act[] = [];
  if (closable) {
    acts.push({
      label: "Close this goal",
      tone: "danger",
      busy: cancel.busy,
      onAct: () => {
        if (!confirmed || selected === null) return;
        void cancel.cancel(selected.id, "Closed from MemQL OS").then((ok) => {
          if (ok) setConfirm("");
        });
      },
    });
  }

  const tone: ActionBarTone =
    selected === null
      ? "none"
      : selected.status === "active"
        ? "busy"
        : selected.status === "open"
          ? "live"
          : "none";

  return (
    <div className="os-work-goals">
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

      <div className="os-work-scope">
        <SortControl ascending={ascending} onToggle={() => setAscending((v) => !v)} />
      </div>

      <div className="os-work-split">
        <div className="os-work-column">
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
                  setConfirm("");
                  cancel.reset();
                  onSelectGoal(goal.id);
                }}
              />
            )}
          />
        </div>

        {/* THE ASIDE IS ABSENT WHEN THERE IS NOTHING IN IT (rule 9: nothing
            paints half a window of dead space beside a half-width column).
            An empty panel saying "pick a goal" reserved 500px to say what a
            clickable row already says. Bin does the same. */}
        {composing || selected !== null ? (
          <div className="os-work-column os-work-aside">
            {composing ? (
              <NewGoal
                create={create}
                onCreated={(goalId) => {
                  setComposing(false);
                  // The row arrives on the feed with its own arrival cue. This
                  // only says WHICH one to open, so the person lands on the
                  // goal they just described rather than hunting for it.
                  onSelectGoal(goalId);
                }}
                onCancel={() => setComposing(false)}
              />
            ) : selected === null ? null : (
              <GoalDetail goal={selected} runs={selectedRuns} now={now} onOpenRun={onOpenRun} />
            )}
          </div>
        ) : null}
      </div>

      {composing || selected === null ? null : (
        <ActionBar
          state={goalStatusWord(selected.status)}
          detail={goalStatusDetail(selected.status)}
          tone={tone}
          acts={acts}
        >
          {/* THE CONFIRMATION IS THE THING'S OWN NAME, which for a goal is the
              sentence somebody wrote. Closing asks every run of it to stop, so
              this is the one place in the app that asks somebody to type
              something -- and what it asks for is the statement, because that
              is what they would call it. */}
          {closable ? (
            <span className="os-work-confirm">
              <Input
                id="work-close-confirm"
                label={`Type the goal's own words to close it: ${goalTitle(selected)}`}
                placeholder="Type the goal to close it"
                value={confirm}
                onChange={setConfirm}
              />
            </span>
          ) : null}
          {cancel.error === "" ? null : (
            <span className="os-work-act-error os-mono" role="alert">
              {cancel.error}
            </span>
          )}
        </ActionBar>
      )}
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
      name={<span className="os-work-goal-statement">{goalTitle(goal)}</span>}
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
          <span className="os-work-goal-status" data-status={goal.status}>
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
        <span className="os-work-goal-origin">{originWord(goal.origin)}</span>
      )}
    </KitRow>
  );
}

function GoalDetail({
  goal,
  runs,
  now,
  onOpenRun,
}: {
  goal: GoalRow;
  runs: readonly RunRow[];
  now: Date;
  onOpenRun: (runId: string) => void;
}) {
  const ceilings = goal.ceilings;
  return (
    <>
      <Panel label="This goal">
        {/* THE STATEMENT IN FULL, AT CONTENT SIZE. The list row truncates it
            to one line because it has to; the detail is where the whole
            sentence lives, and it is the reason the panel exists. */}
        <p className="os-work-goal-full">{goalTitle(goal)}</p>
        <Facts>
          <Fact label="Origin" value={originWord(goal.origin)} />
          <Fact label="Asked through" value={goal.requestedVia} mono />
          <Fact label="Opened" value={formatMoment(goal.createdAt)} title={goal.createdAt} />
          {/* THE CLOSING FACTS ONLY EXIST ONCE IT IS CLOSED. Two em dashes on
              every open goal is two lines a reader has to take in to learn
              nothing -- an absent value renders as an em dash, but an absent
              QUESTION should not be asked at all. */}
          {goal.status === "closed" ? (
            <>
              <Fact
                label="Closed"
                value={goal.closedAt === "" ? "" : formatMoment(goal.closedAt)}
                title={goal.closedAt}
              />
              <Fact label="Why it closed" value={goal.closeReason} />
            </>
          ) : null}
        </Facts>
        {goal.accountIds.length === 0 ? null : (
          <>
            {/* A LONE PILL READING "acme" IS A MYSTERY. `Chips` names the
                group for assistive tech; a sighted reader needs the same
                sentence. */}
            <Subhead>Who this work is for</Subhead>
            <Chips label="Who this work is for">
              {goal.accountIds.map((accountId) => (
                <Chip key={accountId} tone="muted">
                  {accountId}
                </Chip>
              ))}
            </Chips>
          </>
        )}
        {/* CEILINGS ARE A FACT ABOUT THE GOAL, NOT A CONTROL. They are set
            when the goal is accepted and inherited by every run; there is no
            editor here because there is no verb for one, and a form that
            looked editable and refused would be worse than an absent one. */}
        {ceilings === null ? null : (
          <>
            <Subhead>Ceilings every run of this inherits</Subhead>
            <Facts>
              {Object.entries(ceilings).map(([key, value]) => (
                <Fact
                  key={key}
                  label={key}
                  value={typeof value === "number" || typeof value === "string" ? String(value) : ""}
                  mono
                />
              ))}
            </Facts>
          </>
        )}
      </Panel>

      {/* The Panel is a NAMED REGION, so the list inside it carries no
          aria-label of its own: a list repeating its section's name makes a
          screen reader announce "runs of this goal" twice (rule 7, say it
          once) -- and made the name ambiguous to a query, which is how it was
          found. */}
      <Panel label="Runs of this goal">
        <Subhead>Runs</Subhead>
        {runs.length === 0 ? (
          <Caption>
            No run yet. A goal opens one the moment it is accepted, so this is either very new or
            the run has not reached this window.
          </Caption>
        ) : (
          <ul className="os-work-runlist">
            {runs.map((run) => (
              <li key={run.id}>
                <RunLine
                  run={run}
                  goal={goal}
                  now={now}
                  showContext={false}
                  onOpen={() => onOpenRun(run.id)}
                />
              </li>
            ))}
          </ul>
        )}
      </Panel>

      {goal.status === "closed" ? (
        <Notice
          tone="info"
          sentence="This goal is closed."
          next="Its runs were asked to stop at their next step boundary; anything already journaled stays readable."
        />
      ) : null}
    </>
  );
}
