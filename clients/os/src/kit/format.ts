// The shell's time and duration voice.
//
// It was written for the Fleet and moved here when the Users app needed the
// same two readings (memql#4734). That is the rule this directory states: a
// helper earns a place in the kit when a SECOND surface needs it -- promoting
// on the first use invents an abstraction from one example, and waiting for
// the third means the second one has already forked.
//
// Two time formatters, answering different questions. `moment` is "when did
// this happen" for a fact an operator reads once; `freshness` is "how long
// ago" for the value this surface is really about -- a machine's last
// heartbeat, where the ABSOLUTE time says nothing useful (a wall-clock
// reading against a thirty-second window is a subtraction the reader has to
// do) and the elapsed time is the whole point.

/** An absolute instant, for a fact rather than a liveness reading. */
export function formatMoment(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "--";
  const parsed = new Date(trimmed);
  if (Number.isNaN(parsed.getTime())) return trimmed;
  return parsed.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

/**
 * Elapsed time against `now`, coarsening as it grows.
 *
 * "never" and "--" are DIFFERENT answers and both appear: a machine with no
 * `lastSeenAt` has never checked in, which is not the same as one whose value
 * we could not read. A FUTURE timestamp reads as "just now" rather than as a
 * negative duration -- that is clock skew between the cluster and this
 * browser, and "-12s ago" would send an operator to debug the wrong thing.
 */
export function formatFreshness(value: string, now: Date): string {
  const trimmed = value.trim();
  if (trimmed === "") return "never";
  const parsed = Date.parse(trimmed);
  if (Number.isNaN(parsed)) return trimmed;

  const seconds = Math.floor((now.getTime() - parsed) / 1000);
  if (seconds <= 1) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/** A call's duration. Zero and negative render as "--": a row whose
 *  `completedAt` never landed has no duration, and "0ms" would claim one. */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "--";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}
