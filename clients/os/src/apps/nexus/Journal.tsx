import { UsageDetails } from "./UsageDetails";
import type { ModelUsageState } from "./useModelUsage";
import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { Button, Caption, Fact, Facts, RecordList, RecordRow, Notice, Subhead, formatDuration } from "../../kit";
import type { Journal as JournalState } from "./useNexus";
import { formatMoney, formatTokens, observationKindWord, servedWord } from "./rows";

export function JournalPanel({ journal, usage }: { journal: JournalState; usage?: ModelUsageState }) {
  const empty = journal.state === "ready" && !journal.modelCalls.length && !journal.observations.length;
  const calls = journal.modelCalls;
  const sum = (key: "inputTokens" | "outputTokens") => calls.length && calls.every(call => call[key] !== null) ? calls.reduce((total, call) => total + call[key]!, 0) : null;
  const input = sum("inputTokens"), output = sum("outputTokens");
  return <section className="os-nexus-journal" aria-label="Activity for this run">
    <Subhead>Activity</Subhead>
    {journal.hasNewer ? <Caption>Earlier activity</Caption> : null}
    {journal.state === "loading" && !journal.readAt ? <RecordListSkeleton label="Loading activity" /> : null}
    {journal.error ? <Notice tone="error" sentence="Activity could not be read." detail={journal.error}><Button onClick={journal.read}>Retry</Button></Notice> : null}
    {empty ? <Caption>No activity recorded yet.</Caption> : null}
    {journal.observations.length ? <RecordList as="ul" label="Recorded actions and outcomes">{journal.observations.map(observation => <li key={observation.id}><RecordRow name={observationKindWord(observation.kind)} secondary={observation.content} /><details className="nexus-technical"><summary>Event details</summary><Facts><Fact label="Step reference" value={observation.stepKey} copyValue={observation.stepKey} /><Fact label="Recorded at" value={observation.createdAt} /></Facts></details></li>)}</RecordList> : null}
    {usage ? <UsageDetails usage={usage} /> : null}
    {calls.length ? <section>{!usage ? <><Subhead>Token usage</Subhead><Facts><Fact label="Input" value={input === null ? "Not fully reported" : formatTokens(input)} /><Fact label="Output" value={output === null ? "Not fully reported" : formatTokens(output)} /></Facts>
      {journal.hasOlder || journal.hasNewer ? <Caption>Usage for the activity shown on this page.</Caption> : null}</> : null}
      <details className="nexus-technical"><summary>Models and usage details</summary><RecordList as="ul" label="Model activity">{calls.map(call => <RecordRow key={call.id} name={call.model || "Model not recorded"} secondary={call.error || undefined} state={servedWord(call.served)}>{!usage ? <><span>{call.inputTokens === null ? "Input not reported" : `${formatTokens(call.inputTokens)} input`}</span><span>{call.outputTokens === null ? "Output not reported" : `${formatTokens(call.outputTokens)} output`}</span><span>{formatMoney(call.cost)}</span></> : null}<span>{call.latencyMs === null ? "Duration not reported" : formatDuration(call.latencyMs)}</span></RecordRow>)}</RecordList></details>
    </section> : null}
    {journal.hasOlder || journal.hasNewer ? <nav className="os-panel-actions" aria-label="Activity pages"><Button onClick={journal.newer} disabled={!journal.hasNewer || journal.state === "loading"}>Newer activity</Button><Button onClick={journal.older} disabled={!journal.hasOlder || journal.state !== "ready"}>Older activity</Button></nav> : null}
  </section>;
}
