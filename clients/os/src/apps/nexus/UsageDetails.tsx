import { Button, Caption, Fact, Facts, NextPage, Notice, RecordList, RecordRow, RecordListSkeleton, Subhead } from "../../kit";
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
    </> : usage.loading ? <RecordListSkeleton label="Reading the recorded usage" /> : usage.error ? null : <Caption>No completed model usage receipts were returned.</Caption>}
    {total.calls > 0 ? <NextPage feed={{ hasMore: usage.hasMore, loadingMore: usage.loading,
      loadMoreError: usage.error, progress: usage.progress ?? usage.calls.length, scope: usage.scope,
      loadMore: usage.hasMore ? usage.loadMore : usage.refresh }} label="Loading more usage" /> : null}
    {!usage.error ? <Button tone="quiet" disabled={usage.loading} onClick={usage.refresh}>Refresh usage</Button> : null}
    {usage.error && total.calls === 0 ? <Notice tone="warn" sentence="Usage could not be refreshed." detail={usage.error}><Button tone="quiet" onClick={usage.refresh}>Read usage again</Button></Notice> : null}
  </section>;
}
