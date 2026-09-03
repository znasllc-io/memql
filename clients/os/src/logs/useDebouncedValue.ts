import { useEffect, useState } from "react";

/** How long a typed search text settles before it becomes a call. */
export const TEXT_DEBOUNCE_MS = 250;

/**
 * A value that follows its input after a pause.
 *
 * The search text is a SERVER facet -- `text` on `logsTail` and `logsSearch`
 * -- so every change of it is a round trip and a re-baseline. Typing a word
 * is six changes, and six baselines a second is a stream that cannot be read
 * while it is being narrowed. The pause is short enough that the answer
 * still feels live and long enough that a word arrives as one question.
 */
export function useDebouncedValue<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}
