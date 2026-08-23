import { Link, useNavigate } from "react-router-dom";
import type { ReactNode } from "react";

import { Badge, Field, Select } from "../ui";
import { nexusPath } from "./urls";
import type { Goal } from "./useGoals";

// Which goal the map is showing, and how to get to another one.
//
// A <select> rather than a list of links, because the picker is CHROME: it
// sits in the page header beside the goal's own title, and a person changing
// goals is switching subject rather than browsing a population. The
// recent-goals strip below the map is the browse affordance, and it is links.
//
// Navigation, not state. Choosing a goal changes the URL (design D5), so the
// back button walks the goals you looked at and a chosen goal can be sent to
// someone.

export function GoalPicker({
  goals,
  currentId,
}: {
  goals: readonly Goal[];
  currentId: string;
}): ReactNode {
  const navigate = useNavigate();
  if (goals.length === 0) return null;

  return (
    <Field label="Goal">
      <Select value={currentId} onChange={(next) => navigate(nexusPath(next))}>
        {/* A goal reached by a deep link may not be in the caller's own list
            -- the list is a bounded page and the link may name an older one.
            Rendering an option for it keeps the control from silently
            snapping to a different goal than the one on screen. */}
        {goals.some((goal) => goal.id === currentId) || currentId === "" ? null : (
          <option value={currentId}>this goal</option>
        )}
        {goals.map((goal) => (
          <option key={goal.id} value={goal.id}>
            {goal.running ? "* " : ""}
            {goal.goal === "" ? goal.id : goal.goal}
          </option>
        ))}
      </Select>
    </Field>
  );
}

// The recent-goals strip: the last handful, as links, under the map.
export function RecentGoals({
  goals,
  currentId,
}: {
  goals: readonly Goal[];
  currentId: string;
}): ReactNode {
  const recent = goals.filter((goal) => goal.id !== currentId).slice(0, 6);
  if (recent.length === 0) return null;

  return (
    <nav aria-label="Recent goals" className="flex flex-wrap items-center gap-2">
      <span className="text-xs font-semibold tracking-wide text-muted uppercase">Recent goals</span>
      {recent.map((goal) => (
        <Link
          key={goal.id}
          to={nexusPath(goal.id)}
          className="rounded border border-line bg-surface px-2 py-1 text-xs hover:bg-raised"
        >
          <span className="max-w-64 truncate align-middle">{goal.goal === "" ? goal.id : goal.goal}</span>
          {goal.running ? (
            <span className="ml-1.5 align-middle">
              <Badge tone="warn">running</Badge>
            </span>
          ) : null}
        </Link>
      ))}
    </nav>
  );
}
