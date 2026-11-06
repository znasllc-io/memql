import { useRef } from "react";
import { refusalSentence } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Button, Fact, Facts, InlineSkeleton, Notice, Panel, RecordList, RecordRow, Subhead } from "../../../kit";
import { figureWords } from "../../../kit/measure";
import { useRetryEnrollments } from "../actions";
import type { JourneyRow, TemplateRow } from "../rows";
import { EMAIL_REFRESH_MS, RUN_STATUSES, useJourneyPeople } from "../useEmail";
import { stepSentence } from "./flowModel";

/** How many failed runs the engine lists at once; Retry all still covers every one. */
const LISTED = 50;

/** Where an automation's people are, and a way to start the failed ones again. */
export function People({ journey, templates, onRetried }: { journey: JourneyRow; templates: readonly TemplateRow[]; onRetried: () => void }) {
  const people = useJourneyPeople(journey.id);
  const retry = useRetryEnrollments();
  const working = useRef(false);
  const loaded = people.readAt !== "";
  // A retired automation's runs stay as they are: the engine refuses to change it.
  const retryable = journey.status !== "archived";
  const failedCount = people.value.counts.get("failed");
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
    <Subhead actions={retryable && people.value.failed.length > 0 ? <Button busy={retry.busy} onClick={() => { void start([]); }}>Retry all</Button> : undefined}>People</Subhead>
    {people.error ? <Notice tone="warn" sentence={refusalSentence(people.error)} /> : null}
    {retry.error ? <Notice tone="warn" sentence={refusalSentence(retry.error)} /> : null}
    {loaded ? <Facts>{RUN_STATUSES.map(([status, label]) => <Fact key={status} label={label} value={figureWords(people.value.counts.get(status))} />)}</Facts> : people.error ? null : <InlineSkeleton label="Loading people" />}
    {people.value.failed.length ? <RecordList label="People whose run failed">{people.value.failed.map(run => <RecordRow key={run.id} name={run.address || "An address no longer on the list"} secondary={stepWords(run.stepId)} actions={retryable ? <Button busy={retry.busy} ariaLabel={`Retry ${run.address || "this person"}`} onClick={() => { void start([run.id]); }}>Retry</Button> : undefined}>{run.lastError}</RecordRow>)}</RecordList> : null}
    {failedCount?.kind === "measured" && failedCount.value > people.value.failed.length && people.value.failed.length >= LISTED ? <p className="os-caption">Showing the newest {LISTED} failed runs.{retryable ? " Retry all starts every failed run." : ""}</p> : null}
  </Panel>;
}
