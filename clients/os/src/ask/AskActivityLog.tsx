import { X } from "lucide-react";
import type { AskActivity, AskTurn } from "./conversationSession";
export function AskActivityLog({ turns, dictation = [], onClose }: { turns: AskTurn[]; dictation?: AskActivity[]; onClose: () => void }) {
  if (dictation.length) turns = [...turns, { id: "dictation", prompt: "Dictation", answer: "", state: "done", startedAt: dictation[0]!.at, activity: dictation }];
  return <aside className="os-ask-activity" aria-label="Conversation activity">
    <header><strong>Activity</strong><button type="button" aria-label="Close activity" onClick={onClose}><X size={15} /></button></header>
    {turns.every(turn => turn.activity.length === 0) ? <p className="os-caption">Model calls and completed actions appear here.</p> : null}
    {turns.map(turn => {
      const calls = new Map<string, AskActivity>();
      for (const event of turn.activity) calls.set(event.id, { ...calls.get(event.id), ...event });
      if (!calls.size) return null;
      return <section key={turn.id}><p className="os-caption os-ask-activity-question">{turn.prompt}</p>{[...calls.values()].map(event => <details key={event.id}>
        <summary><span>{event.kind === "model" ? event.model || event.provider || "Model call" : event.name}</span><small>{event.phase === "running" ? "Working" : event.phase === "failed" ? "Failed" : event.phase === "fallback" ? "Fallback" : event.elapsedMs ? `${(event.elapsedMs / 1000).toFixed(1)}s` : "Completed"}</small></summary>
        <dl><dt>Time</dt><dd>{new Date(event.at).toLocaleString()}</dd>{event.provider ? <><dt>Route</dt><dd>{event.provider}</dd></> : null}{event.model ? <><dt>Model</dt><dd>{event.model}</dd></> : null}{event.kind === "model" ? event.call && event.phase !== "running" ? <>
          <dt>Tokens</dt><dd>{event.call.inputTokens} in · {event.call.outputTokens} out{event.call.tokensEstimated ? " (estimated)" : ""}</dd>
          <dt>Cost</dt><dd>{event.call.cacheKind ? "No model call" : event.call.billing === "local" ? "Local inference" : event.call.pricingConfigured ? `$${event.call.totalCost.toFixed(6)}${event.call.tokensEstimated ? " (estimated)" : ""}` : "Not reported"}</dd>
          {event.call.firstTokenMs > 0 ? <><dt>First response</dt><dd>{(event.call.firstTokenMs / 1000).toFixed(2)}s</dd></> : null}
          {([ ["Vendor", event.call.vendor], ["Policy", event.call.policy], ["Rule", event.call.rule], ["Surface", event.call.executionSurface], ["Served model", event.call.servedModel], ["Reuse", event.call.cacheKind] ] as const).map(([label, value]) => value ? <div key={label}><dt>{label}</dt><dd>{value}</dd></div> : null)}
        </> : <><dt>Usage</dt><dd>Awaiting call completion</dd></> : null}{event.expectedMs ? <><dt>Expected</dt><dd>About {Math.round(event.expectedMs / 1000)}s · {event.estimateSource}</dd></> : null}</dl>
        {event.error ? <p className="os-ask-error">{event.error}</p> : null}
        {event.arguments && Object.keys(event.arguments).length ? <pre>{JSON.stringify(event.arguments, null, 2)}</pre> : null}
      </details>)}</section>;
    })}
  </aside>;
}
