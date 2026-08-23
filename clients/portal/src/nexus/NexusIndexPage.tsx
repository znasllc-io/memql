import { Navigate } from "react-router-dom";
import type { ReactNode } from "react";

import { Callout, Container, EmptyState, PageHeader, Skeleton } from "../ui";
import { useGoals } from "./useGoals";
import { nexusPath } from "./urls";

// /nexus -- no goal named, so open the most recent one.
//
// A REDIRECT rather than a landing page listing goals, because the picker in
// the header is already the list and a second one would be a second door to
// the same place. `replace` so the back button leaves Nexus rather than
// bouncing off this route into the goal it just redirected to.
//
// The empty state is the honest part. A goal is a v1:planner:plan, and
// NOTHING IN THIS CONSOLE CREATES ONE -- plans come from asking an agent to
// do something, in whatever surface this cluster's product puts in front of
// people. Saying "create your first goal" with no button under it would be
// worse than saying where goals come from.

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
              ? "You have no goals yet. A goal is created when you ask an agent to do " +
                "something -- this console reads them, it does not start them."
              : "None of your goals can be opened."
          }
        />
      </section>
    </Container>
  );
}
