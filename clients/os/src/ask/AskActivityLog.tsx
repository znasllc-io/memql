import { Fragment, useEffect, useRef } from "react";
import { ArrowLeft, Square } from "lucide-react";
import { Head } from "../kit";
import type { AskActivity, AskTurn } from "./conversationSession";
export function AskActivityLog({ turns, dictation = [], onClose, onStop }: { turns: AskTurn[]; dictation?: AskActivity[]; onClose: () => void; onStop?: () => void }) {
  const back = useRef<HTMLButtonElement>(null);
  useEffect(() => { back.current?.focus(); }, []);
  if (dictation.length) {
    const latest = new Map(dictation.map(event => [event.id, event]));
    const running = [...latest.values()].some(event => event.phase === "running");
    turns = [...turns, { id: "dictation", prompt: "Dictation", answer: "", state: running ? "streaming" : "done", startedAt: dictation[0]!.at, activity: dictation }];
  }
  return <section className="os-ask-activity" aria-label="Conversation activity" tabIndex={-1} onKeyDown={event => { if (event.key === "Escape") { event.stopPropagation(); onClose(); } }}>
    <Head title="Activity" navigation={false}>
      <button ref={back} type="button" className="os-icon-button" aria-label="Close activity" title="Back to conversation" onClick={onClose}><ArrowLeft size={17} /></button>
      {onStop ? <button type="button" className="os-icon-button" aria-label="Stop reply" title="Stop this work" onClick={onStop}><Square size={13} /></button> : null}
    </Head>
    {turns.every(turn => turn.activity.length === 0) ? <p className="os-caption">Model calls and completed actions appear here.</p> : null}
    {turns.map(turn => {
      const calls = new Map<string, AskActivity>();
      for (const event of turn.activity) calls.set(event.id, { ...calls.get(event.id), ...event });
      if (!calls.size) return null;
      return <section key={turn.id} className="os-ask-activity-turn"><h4 className="os-ask-activity-question">{turn.prompt}</h4><div className="os-ask-activity-steps">{[...calls.values()].map(event => <details key={event.id}>
        <summary><span>{activityName(event)}</span><small>{activityState(event, turn)}</small></summary>
        <dl><dt>Time</dt><dd>{new Date(event.at).toLocaleString()}</dd>{event.provider ? <><dt>Route</dt><dd>{event.provider}</dd></> : null}{event.model ? <><dt>Model</dt><dd>{event.model}</dd></> : null}{event.kind === "model" ? event.call && event.phase !== "running" ? <>
          <dt>Tokens</dt><dd>{event.call.inputTokens} in · {event.call.outputTokens} out{event.call.tokensEstimated ? " (estimated)" : ""}</dd>
          <dt>Cost</dt><dd>{event.call.cacheKind ? "No model call" : event.call.billing === "local" ? "Local inference" : event.call.pricingConfigured ? `$${event.call.totalCost.toFixed(6)}${event.call.tokensEstimated ? " (estimated)" : ""}` : "Not reported"}</dd>
          {event.call.firstTokenMs > 0 ? <><dt>First response</dt><dd>{(event.call.firstTokenMs / 1000).toFixed(2)}s</dd></> : null}
          {([ ["Prompt", event.call.promptName], ["Vendor", event.call.vendor], ["Policy", event.call.policy], ["Rule", event.call.rule], ["Surface", event.call.executionSurface], ["Served model", event.call.servedModel], ["Reuse", event.call.cacheKind] ] as const).map(([label, value]) => value ? <Fragment key={label}><dt>{label}</dt><dd>{value}</dd></Fragment> : null)}
        </> : <><dt>Usage</dt><dd>{turn.state === "streaming" ? "Awaiting call completion" : "Not reported"}</dd></> : null}{event.expectedMs ? <><dt>Expected</dt><dd>About {Math.round(event.expectedMs / 1000)}s · {event.estimateSource}</dd></> : null}</dl>
        {event.error || (event.kind === "run" && turn.error) ? <p className="os-ask-error">{event.error || turn.error}</p> : null}
        {event.arguments && Object.keys(event.arguments).length ? <pre>{JSON.stringify(event.arguments, null, 2)}</pre> : null}
      </details>)}</div></section>;
    })}
  </section>;
}

function activityName(event: AskActivity): string {
  if (event.kind === "run") return "Request";
  const purpose = event.call?.purpose || event.name || ({ goalComplexityTriage: "Understanding request", authoringDesign: "Designing automation", authoringEmit: "Writing automation", authoringRepair: "Repairing automation" } as Record<string, string>)[event.call?.promptName ?? ""];
  if (purpose) return event.call?.attempt ? `${purpose} · attempt ${event.call.attempt}` : purpose;
  return event.kind === "model" ? "Generating response" : "Action";
}

function activityState(event: AskActivity, turn: AskTurn): string {
  if (event.phase === "running") {
    if (turn.state === "error") return "Completion not reported";
    if (turn.state === "interrupted") return "Updates interrupted";
    if (turn.state === "done") return event.kind === "run" ? "Completed" : "Completion not reported";
    return "Working";
  }
  if (event.phase === "failed") return "Failed";
  if (event.phase === "waiting") return "Needs input";
  if (event.phase === "cancelled") return "Stopped";
  if (event.phase === "fallback") return "Fallback";
  return event.elapsedMs ? `${(event.elapsedMs / 1000).toFixed(1)}s` : "Completed";
}
