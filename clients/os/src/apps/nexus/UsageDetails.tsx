import { Button, Caption, Fact, Facts, Notice, RecordList, RecordRow, Subhead } from "../../kit";
import { formatMoney, formatTokens } from "./rows";
import { summarizeUsage, usageLabel } from "./modelUsage";
import type { ModelUsageState } from "./useModelUsage";

export function UsageDetails({ usage }: { usage: ModelUsageState }) {
  const total = summarizeUsage(usage.calls, usage.complete);
  return <section aria-label="Recorded token usage" className="os-app-stack">
    <Subhead>Token usage</Subhead>
    <Caption>{usageLabel(total)}</Caption>
    {total.calls > 0 ? <><Facts><Fact label="Input tokens" value={formatTokens(total.input)} /><Fact label="Output tokens" value={formatTokens(total.output)} /><Fact label="Model calls shown" value={String(total.calls)} /></Facts>
      {total.estimated ? <Caption>These token counts include estimates recorded during execution.</Caption> : null}
      {total.incomplete ? <Caption>This is an incomplete usage total. More receipts or missing measurements may change it.</Caption> : null}
      <details className="nexus-technical"><summary>Model usage receipts</summary><RecordList as="ul" label="Usage receipts">{usage.calls.map(call => <RecordRow key={call.id} name={call.model || "Model not reported"} state={call.estimated ? "Estimated tokens" : "Reported tokens"}>
        <span>{call.input === null ? "Input not reported" : `${formatTokens(call.input)} input`}</span><span>{call.output === null ? "Output not reported" : `${formatTokens(call.output)} output`}</span><span>{call.cacheKind ? "Saved response reused" : call.billing === "local" ? "Local inference" : call.cost === null ? "Cost not reported" : formatMoney(call.cost)}</span>
      </RecordRow>)}</RecordList></details>
    </> : usage.loading ? <Caption>Reading the recorded usage…</Caption> : usage.error ? null : <Caption>No completed model usage receipts were returned.</Caption>}
    {usage.hasMore ? <Button tone="quiet" disabled={usage.loading} onClick={usage.loadMore}>Load more usage</Button> : null}
    {!usage.error ? <Button tone="quiet" disabled={usage.loading} onClick={usage.refresh}>Refresh usage</Button> : null}
    {usage.error ? <Notice tone="warn" sentence="Usage could not be refreshed." detail={usage.error}><Button tone="quiet" onClick={usage.refresh}>Read usage again</Button></Notice> : null}
  </section>;
}
