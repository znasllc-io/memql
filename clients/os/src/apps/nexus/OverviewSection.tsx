import { useMemo } from "react";
import type { LiveSnapshot, Row } from "@znasllc-io/memql-sdk-core/client";
import { Waypoints } from "lucide-react";

import { Button, Caption, ContentSkeleton, EmptyState, Notice } from "../../kit";
import { InfoDetail } from "../../kit/InfoDetail";
import { absent, figureOf, type Figure } from "../../kit/measure";
import { Overview, OverviewBreakdown, type OverviewMetric } from "../../kit/Overview";
import { anyLabelled, reuseTally } from "./reuse";
import { approvalFromRow, goalFromRow, runFromRow, runIsTerminal } from "./rows";
import { useReusableAfter, useReuseLibrary } from "./useInterventions";
import { reuseWord } from "./words";

// Nexus's Overview (epic memql#5414): what is open, what is moving, what is
// waiting on you -- and how much of what the system has learned is reusable.
//
// ===========================================================================
// EVERY FIGURE COMES FROM A READ NEXUS ALREADY MAKES
// ===========================================================================
// Goals, runs and approvals are the app root's three live feeds, passed in;
// the automation library is the authored catalog and the learned procedures,
// the same two reads the Automations section makes, held here only while the
// Overview is on screen. Nothing is fetched to be summarised that is not also
// what the lists behind it show (DESIGN.md, "App overviews").
//
// ===========================================================================
// ABSENT IS NOT ZERO -- AND "NOT YET LABELLED" IS NOT "FOR ONE GOAL"
// ===========================================================================
// A feed still answering draws the SHAPE of its figure, never a caption and
// never a 0. The reuse ratio stays absent until the sweep has labelled
// something: "0 to 0" would say the library had been looked at and found
// wanting, when nobody has looked. Unlabelled automations are their own
// segment for the same reason -- folding them into "for one goal" would make a
// claim the evidence never made.

export function OverviewSection({
  goals,
  runs,
  approvals,
  navigate,
}: {
  goals: LiveSnapshot<Row>;
  runs: LiveSnapshot<Row>;
  approvals: LiveSnapshot<Row>;
  navigate: (sectionId: string) => void;
}) {
  const library = useReuseLibrary();
  const reusableAfter = useReusableAfter();

  const openGoals = useMemo(
    () => goals.rows.map(goalFromRow).filter((goal) => goal.id !== "" && goal.status !== "closed").length,
    [goals.rows],
  );
  const inFlight = useMemo(
    () => runs.rows.map(runFromRow).filter((run) => run.id !== "" && !runIsTerminal(run)).length,
    [runs.rows],
  );
  const waiting = useMemo(
    () => approvals.rows.map(approvalFromRow).filter((approval) => approval.id !== "").length,
    [approvals.rows],
  );
  const tally = useMemo(
    () => reuseTally([...library.catalog.snapshot.rows, ...library.procedures.snapshot.rows]),
    [library.catalog.snapshot.rows, library.procedures.snapshot.rows],
  );

  const catalog = library.catalog.snapshot;
  const procedures = library.procedures.snapshot;
  const libraryLoading = catalog.state === "seeding" || procedures.state === "seeding";
  // The library is known when BOTH reads have answered. One refused read is
  // that read's own sentence; the figures it feeds are absent, not smaller.
  const libraryError = catalog.error || procedures.error;
  const libraryKnown = settled(catalog) && settled(procedures) && libraryError === "";

  const labelled = anyLabelled(tally);
  const reuseFigure: Figure = libraryError !== ""
    ? absent("failed", libraryError)
    : !libraryKnown
      ? absent("unread")
      : labelled
        ? figureOf(tally.reusable)
        : absent("unmeasured", tally.total === 0 ? "No automations have been kept yet." : "Nothing is labelled yet.");

  const metrics: OverviewMetric[] = [
    feedMetric("Goals open", goals, openGoals),
    feedMetric("Runs in flight", runs, inFlight),
    feedMetric("Approvals waiting", approvals, waiting),
    {
      label: "Reusable to goal-specific",
      figure: reuseFigure,
      loading: libraryLoading,
      format: () => `${tally.reusable} to ${tally.goalSpecific}`,
      detail: libraryKnown ? reuseDetail(tally) : undefined,
    },
  ];

  const everythingSettled = settled(goals) && settled(runs) && settled(approvals) && libraryKnown;
  const nothingAtAll = everythingSettled && goals.rows.length === 0 && runs.rows.length === 0 && tally.total === 0;

  return (
    <Overview
      metrics={metrics}
      actions={
        /* THE RULES BEHIND THE FIGURES, ONE CLICK AWAY rather than standing
           under them (DESIGN.md rule 7): what each counts, and the reuse
           threshold as the cluster's own policy states it. */
        <InfoDetail title="About this overview">
          <p>
            <strong>Goals open</strong> are the ones not closed yet. <strong>Runs in flight</strong> are working
            something out, running, or waiting. <strong>Approvals waiting</strong> are runs stopped on you.
          </p>
          <p>
            <strong>Reusable to goal-specific</strong> counts your automations, the ones you authored and the ones
            the system learned.{" "}
            {reusableAfter === null
              ? "An automation is reusable once enough different goals have used it."
              : `An automation is reusable once ${reusableAfter} different goals have used it.`}{" "}
            One used by a single goal is for that goal; one tied to a single account is for that account. A label you
            gave an automation yourself counts instead of the evidence.
          </p>
          <p>Not yet labelled means nothing has looked at its evidence yet. It is not counted as either.</p>
        </InfoDetail>
      }
    >
      {goals.error || runs.error || approvals.error ? (
        <Notice
          tone="warn"
          sentence="Some of this could not be read."
          next="The figures that could are shown; the others are marked as not read."
          detail={goals.error || runs.error || approvals.error}
        />
      ) : null}
      {nothingAtAll ? (
        <EmptyState
          icon={Waypoints}
          title="Nothing to summarise yet"
          action={<Button onClick={() => navigate("goals")}>Open goals</Button>}
        >
          Ask for something in Goals. The work it takes, and what the system learns doing it, is
          counted here.
        </EmptyState>
      ) : null}
      {libraryError !== "" ? (
        <Notice tone="warn" sentence="Your automations could not be read." detail={libraryError} />
      ) : libraryLoading ? (
        <ContentSkeleton kind="metrics" label="Loading your automations" />
      ) : libraryKnown && tally.total > 0 ? (
        <>
          <OverviewBreakdown
            title="Your automations, by reuse"
            segments={[
              { label: reuseWord("reusable"), count: tally.reusable, tone: "good" },
              { label: reuseWord("goalSpecific"), count: tally.goalSpecific },
              { label: reuseWord("accountSpecific"), count: tally.accountSpecific },
              { label: reuseWord(""), count: tally.unlabelled, tone: "unknown" },
            ]}
          />
        </>
      ) : libraryKnown && !nothingAtAll ? (
        <Caption>No automations have been kept yet. A goal that finishes is kept as one.</Caption>
      ) : null}
    </Overview>
  );
}

/** Whether a feed has answered -- live, or degraded with the last answer still on screen. */
function settled(snapshot: LiveSnapshot<Row>): boolean {
  return snapshot.state === "live" || snapshot.state === "degraded";
}

/** A count off one of the root's live feeds: its shape while it seeds, absent when it failed or is not connected. */
function feedMetric(label: string, snapshot: LiveSnapshot<Row>, value: number): OverviewMetric {
  if (snapshot.error) return { label, figure: absent("failed", snapshot.error) };
  if (snapshot.state === "seeding") return { label, figure: absent("unread"), loading: true };
  if (settled(snapshot)) return { label, figure: figureOf(value) };
  return { label, figure: absent("unread") };
}

/** The quiet line under the ratio: what the two numbers leave out. */
function reuseDetail(tally: ReturnType<typeof reuseTally>): string | undefined {
  const parts: string[] = [];
  if (tally.accountSpecific > 0) parts.push(`${tally.accountSpecific} for one account`);
  if (tally.unlabelled > 0) parts.push(`${tally.unlabelled} not yet labelled`);
  return parts.length === 0 ? undefined : parts.join(" · ");
}
