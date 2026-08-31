// The connection transition buffer (memql#4744).
//
// sdk-core's connection surface is EVENTS, not history: `onStatusChange`
// hands you the current state and every change after it, and nothing keeps
// what came before. A diagnostics panel is asked "what has this connection
// been doing", which is precisely the question an event stream cannot
// answer -- so the app keeps the answer itself, bounded, in memory, for the
// lifetime of the Settings window and no longer. Nothing is persisted:
// a transition list that outlived the session would be a log, and a log is
// a different feature with different retention questions.

import type { ConnectionStatus, ConnectionStatusEvent } from "@znasllc-io/memql-sdk-core/client";

/** How many transitions the buffer keeps. Oldest are dropped. */
export const HISTORY_LIMIT = 50;

export interface ConnectionTransition {
  status: ConnectionStatus;
  /** Consecutive failed dial attempts; 0 while connected. */
  attempt: number;
  /** The failure that ended or is preventing the stream; "" when healthy. */
  error: string;
  /** Milliseconds since the epoch, from the clock passed in. */
  at: number;
  /**
   * True for the entry recorded when the panel first subscribed.
   *
   * `onStatusChange` fires SYNCHRONOUSLY on subscribe with the current
   * event, so the first entry is a reading, not a change. Rendering it as a
   * transition would tell an operator the connection had just done
   * something at the moment they opened a window, which is the one thing
   * they must not be told.
   */
  baseline: boolean;
}

export interface ConnectionHistory {
  transitions: readonly ConnectionTransition[];
  /** The most recent entry, baseline included; null before the first event. */
  current: ConnectionTransition | null;
  /**
   * When the stream last came back UP after being down -- the answer to
   * "has this dropped recently". Null when it has not reconnected in this
   * window's lifetime, which is the common and healthy case; the baseline
   * connect never counts, because it is not a recovery.
   */
  lastReconnectAt: number | null;
}

export const EMPTY_HISTORY: ConnectionHistory = {
  transitions: [],
  current: null,
  lastReconnectAt: null,
};

/**
 * Fold one event into the history. Pure, so the panel's whole behaviour is
 * testable without a socket.
 *
 * An event that changes NEITHER status nor attempt is dropped. The SDK
 * emits one event per dial attempt while reconnecting, and those carry
 * rising `attempt` values that a person reads as progress -- but a repeat of
 * an identical reading is not news, and a buffer that recorded it would
 * spend its fifty slots on the same line.
 */
export function recordTransition(
  history: ConnectionHistory,
  event: ConnectionStatusEvent,
  at: number,
): ConnectionHistory {
  const last = history.current;
  if (last && last.status === event.status && last.attempt === event.attempt) {
    return history;
  }

  const entry: ConnectionTransition = {
    status: event.status,
    attempt: event.attempt,
    error: event.error,
    at,
    baseline: last === null,
  };

  // A recovery is a move INTO connected from a previous reading that was
  // not connected. The baseline entry can never be one: nothing is known
  // about what preceded it, and guessing would invent a drop that may not
  // have happened.
  const recovered = !entry.baseline && entry.status === "connected" && last?.status !== "connected";

  const transitions = [...history.transitions, entry];
  return {
    transitions: transitions.length > HISTORY_LIMIT ? transitions.slice(-HISTORY_LIMIT) : transitions,
    current: entry,
    lastReconnectAt: recovered ? at : history.lastReconnectAt,
  };
}
