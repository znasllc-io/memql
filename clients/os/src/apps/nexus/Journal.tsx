import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { Button, Caption, RecordList, RecordRow, Notice, Subhead, formatDuration } from "../../kit";
import type { Journal as JournalState } from "./useNexus";
import { formatMoney, formatTokens, observationKindWord, servedWord } from "./rows";

export function JournalPanel({ journal }: { journal: JournalState }) {
  const empty =
    journal.state === "ready" &&
    journal.modelCalls.length === 0 &&
    journal.observations.length === 0;

  return (
    <section className="os-nexus-journal" aria-label="The journal for this run">
      <Subhead>Activity</Subhead>
      {journal.state === "loading" && !journal.readAt ? <RecordListSkeleton label="Loading activity" /> : null}
      {journal.error === "" ? null : (
        <Notice
          tone="error"
          sentence="Activity could not be read."
          detail={journal.error}
        />
      )}

      {journal.error ? <Button onClick={journal.read}>Retry</Button> : null}
      {empty ? <Caption>No activity recorded yet.</Caption> : null}

      {journal.modelCalls.length === 0 ? null : (
        <div className="os-nexus-journal-group">
          <Subhead meta={journal.state === "ready" && !journal.error ? journal.modelCalls.length : undefined}>Model calls</Subhead>
          <RecordList as="ul" label="Model calls">{journal.modelCalls.map(call => <RecordRow key={call.id} name={call.model || "--"} secondary={call.error || call.stepKey} state={servedWord(call.served)}>
            <span>{formatTokens(call.inputTokens === null && call.outputTokens === null ? null : (call.inputTokens ?? 0) + (call.outputTokens ?? 0))} tok</span>
            <span>{formatMoney(call.cost)}</span><span>{call.latencyMs === null ? "--" : formatDuration(call.latencyMs)}</span>
          </RecordRow>)}</RecordList>
        </div>
      )}

      {journal.observations.length === 0 ? null : (
        <div className="os-nexus-journal-group">
          <Subhead meta={journal.state === "ready" && !journal.error ? journal.observations.length : undefined}>Observations</Subhead>
          <RecordList as="ul" label="Observations">{journal.observations.map(observation => <RecordRow key={observation.id} name={observationKindWord(observation.kind)} secondary={observation.content} state={observation.stepKey} />)}</RecordList>
        </div>
      )}

    </section>
  );
}
