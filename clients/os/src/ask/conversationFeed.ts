import type { AskTurn } from "./conversationSession";

/** Background completions arrive where they finished, with the original request
 * still in place. A later quick answer cannot hide an earlier task's result. */
export function conversationFeed(turns: AskTurn[]) {
  const entries = turns.flatMap(turn => {
    // The queue receipt records when the acknowledgment was ready. Keep it
    // distinct from the request's start and the eventual background result.
    const acknowledgedAt = turn.acknowledgement?.trim() ? (turn.acknowledgedAt && Number.isFinite(Date.parse(turn.acknowledgedAt)) ? turn.acknowledgedAt : turn.activity.find(event =>
      event.kind === "run" && (event.phase === "queued" || event.phase === "waiting") && Number.isFinite(Date.parse(event.at)),
    )?.at) : undefined;
    const completedAt = turn.state === "done" ? turn.endedAt : undefined;
    const first = { turn, completion: false, key: turn.id, at: turn.startedAt, workState: turn.state, responseAt: acknowledgedAt ?? completedAt };
    if (!(turn.acknowledgement || turn.background) || !turn.endedAt || (turn.state !== "done" && turn.state !== "error")) return [first];
    return [
      { ...first, responseAt: acknowledgedAt, turn: { ...turn, answer: turn.acknowledgement || "", state: "queued" as const, error: undefined, activity: [], endedAt: undefined } },
      { turn, completion: true, key: `${turn.id}:result`, at: turn.endedAt, workState: turn.state, responseAt: completedAt },
    ];
  });
  return entries.map(entry => ({ ...entry, showAssistant: !entry.turn.answerOnly && Boolean(entry.turn.answer.trim() || entry.turn.question || entry.turn.error || !entry.turn.background) }))
    .sort((a, b) => a.at.localeCompare(b.at));
}
