import { useMemo, useRef } from "react";
import { refusalSentence, rowString } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, Fact, Facts, InlineSkeleton, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { countFigure, type Figure } from "../../../kit/measure";
import { flatten } from "../../../kit/rows";
import { useOsConnection } from "../../../live/connection";
import { useRetryEnrollments } from "../actions";
import type { JourneyRow, TemplateRow } from "../rows";
import { EMAIL_REFRESH_MS, useReading } from "../useEmail";
import { stepSentence } from "./flowModel";

/** The statuses a run can be in, in the order people read them, with the words for each. */
const STATUSES = [["active", "In a run"], ["waiting", "Waiting"], ["completed", "Finished"], ["exited", "Left early"], ["failed", "Failed"]] as const;

interface FailedRun { id: string; stepId: string; lastError: string; address: string }
interface PeopleReading { counts: ReadonlyMap<string, Figure>; failed: FailedRun[] }
const NOBODY: PeopleReading = { counts: new Map(), failed: [] };

/**
 * How many people are in each status of one automation, and who stopped.
 *
 * Runs are not broadcast, so this reads them: the count for each status, then,
 * only when something failed, the failed runs and each person's address. The
 * addresses are read one person at a time because a run names its person by id.
 */
function useJourneyPeople(journeyId: string) {
  const connection = useOsConnection();
  const read = useMemo(() => !connection || !journeyId ? null : async (signal: AbortSignal): Promise<PeopleReading> => {
    const counts = new Map(await Promise.all(STATUSES.map(async ([status]) => [status, countFigure((await connection.query.journeyEnrollmentCounts({ journeyId, status }, { signal })).count())] as const)));
    const some = counts.get("failed");
    if (some?.kind !== "measured" || some.value === 0) return { counts, failed: [] };
    const runs = (await connection.query.journeyEnrollmentsByStatus({ journeyId, status: "failed" }, { signal })).rows().map(flatten);
    const people = [...new Set(runs.map(run => rowString(run, "recipientId")).filter(Boolean))];
    const addresses = new Map(await Promise.all(people.map(async recipientId => [recipientId, rowString(flatten((await connection.query.recipientById({ recipientId }, { signal })).rows()[0] ?? {}), "email")] as const)));
    return { counts, failed: runs.map(run => ({ id: rowString(run, "id"), stepId: rowString(run, "stepId"), lastError: rowString(run, "lastError"), address: addresses.get(rowString(run, "recipientId")) ?? "" })) };
  }, [connection, journeyId]);
  return useReading<PeopleReading>(NOBODY, read, [read]);
}

function figureWords(figure: Figure | undefined): string {
  return figure?.kind === "measured" ? `${figure.value.toLocaleString()}${figure.atLeast ? " or more" : ""}` : "—";
}

/** Where an automation's people are, and a way to start the failed ones again. */
export function People({ journey, templates, onRetried }: { journey: JourneyRow; templates: readonly TemplateRow[]; onRetried: () => void }) {
  const people = useJourneyPeople(journey.id);
  const retry = useRetryEnrollments();
  const working = useRef(false);
  const loaded = people.readAt !== "";
  async function start(enrollmentIds: string[]) {
    if (working.current) return;
    working.current = true;
    try {
      if (await retry.call(journey.id, enrollmentIds) >= 0) { await people.reload(); onRetried(); }
    } finally { working.current = false; }
  }
  const stepWords = (stepId: string) => {
    const step = journey.steps.find(candidate => candidate.id === stepId);
    return step ? stepSentence(step, { template: templates.find(template => template.id === step.templateId)?.name ?? "" }) : "";
  };
  return <Panel label="People">
    <AutoRefresh intervalMs={EMAIL_REFRESH_MS} onRefresh={people.reload} busy={people.state === "loading" || retry.busy} />
    <Subhead actions={people.value.failed.length > 0 ? <Button busy={retry.busy} onClick={() => { void start([]); }}>Retry all</Button> : undefined}>People</Subhead>
    {people.error ? <Notice tone="warn" sentence={refusalSentence(people.error)} /> : null}
    {retry.error ? <Notice tone="warn" sentence={refusalSentence(retry.error)} /> : null}
    {loaded ? <Facts>{STATUSES.map(([status, label]) => <Fact key={status} label={label} value={figureWords(people.value.counts.get(status))} />)}</Facts> : people.error ? null : <InlineSkeleton label="Loading people" />}
    {people.value.failed.length ? <RecordList label="People whose run failed">{people.value.failed.map(run => <RecordRow key={run.id} name={run.address || "An address no longer on the list"} secondary={stepWords(run.stepId)} actions={<Button busy={retry.busy} ariaLabel={`Retry ${run.address || "this person"}`} onClick={() => { void start([run.id]); }}>Retry</Button>}>{run.lastError}</RecordRow>)}</RecordList> : null}
  </Panel>;
}
