import { Navigate } from "react-router-dom";
import type { ReactNode } from "react";

import { Callout, Container, EmptyState, PageHeader, Skeleton } from "../ui";
import { NewGoalAction } from "./NewGoalDialog";
import { useGoals } from "./useGoals";
import { nexusPath } from "./urls";

// /nexus -- no goal named, so open the most recent one.
//
// A REDIRECT rather than a landing page listing goals, because the picker in
// the header is already the list and a second one would be a second door to
// the same place. `replace` so the back button leaves Nexus rather than
// bouncing off this route into the goal it just redirected to.
//
// The empty state is an INVITATION now (memql#4528). This header used to say
// "NOTHING IN THIS CONSOLE CREATES ONE" and the empty state told the reader
// that goals come from asking an agent -- true at the time, and the owner has
// reversed it. Both are rewritten rather than outlived: a superseded argument
// left standing beside its replacement reads as the live one.
//
// What the new copy owes the reader is what happens NEXT, honestly. The button
// creates the plan and the planner starts SIZING it; whether anything is spent
// is the estimate / approval / budget gates' decision, and a big goal comes
// back to ask. See NewGoalDialog for why there is no startPlan call behind it.

export function NexusIndexPage(): ReactNode {
  const { goals, loading, error, mostRecentId } = useGoals();

  if (error !== "") {
    return (
      <Container>
        <section className="flex flex-col gap-6 pb-8">
          <PageHeader title="Nexus" blurb="A goal's world, as the system works on it." />
          <Callout tone="danger" title="Your goals could not be read">
            {error}
          </Callout>
        </section>
      </Container>
    );
  }

  if (loading) {
    return (
      <Container>
        <section className="flex flex-col gap-6 pb-8">
          <PageHeader title="Nexus" blurb="A goal's world, as the system works on it." />
          <Skeleton variant="rows" rows={4} />
        </section>
      </Container>
    );
  }

  if (mostRecentId !== "") return <Navigate to={nexusPath(mostRecentId)} replace />;

  return (
    <Container>
      <section className="flex flex-col gap-6 pb-8">
        <PageHeader title="Nexus" blurb="A goal's world, as the system works on it." />
        <EmptyState
          statement={
            goals.length === 0
              ? "You have no goals yet. Start one here, or ask an agent to do something " +
                "-- either way it lands on this map. The planner sizes a goal before it " +
                "spends anything, and asks you first if it is big."
              : "None of your goals can be opened."
          }
          action={<NewGoalAction tone="primary" />}
        />
      </section>
    </Container>
  );
}
