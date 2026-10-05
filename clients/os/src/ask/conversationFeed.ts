import type { AskTurn } from "./conversationSession";

/** Background completions arrive where they finished, with the original request
 * still in place. A later quick answer cannot hide an earlier task's result. */
export function conversationFeed(turns: AskTurn[]) {
  const entries = turns.flatMap(turn => {
    const first = { turn, completion: false, key: turn.id, at: turn.startedAt, workState: turn.state };
    if (!(turn.acknowledgement || turn.background) || !turn.endedAt || (turn.state !== "done" && turn.state !== "error")) return [first];
    return [
      { ...first, turn: { ...turn, answer: turn.acknowledgement || "", state: "queued" as const, error: undefined, activity: [], endedAt: undefined } },
      { turn, completion: true, key: `${turn.id}:result`, at: turn.endedAt, workState: turn.state },
    ];
  });
  return entries.sort((a, b) => a.at.localeCompare(b.at));
}
