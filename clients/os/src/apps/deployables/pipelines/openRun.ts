// A pipeline run named in the address (epic memql#5479).
//
// A GitHub check run's details link is `https://os.<domain>/?pipelineRun=<run
// id>` (component/pipelines/url.go, RunPageURL): the one link the seam writes
// on every check run, and the way somebody reading a failed check on GitHub
// lands on the run page. MemQL OS has no router, so the parameter becomes an
// open intent -- read and scrubbed at boot, beside the concept link and the
// connect return, each removing only its own parameter so none can eat
// another's.

/** The query parameter the check run's details link carries. */
export const RUN_OPEN_PARAM = "pipelineRun";

/** A run id out of a query string, or null when there is no marker. An EMPTY value is a malformed link, not a request. */
export function readRunOpen(search: string): string | null {
  const params = new URLSearchParams(search);
  if (!params.has(RUN_OPEN_PARAM)) return null;
  const id = (params.get(RUN_OPEN_PARAM) ?? "").trim();
  return id === "" ? null : id;
}

/** The same query with this parameter removed, leading `?` included, or "". */
export function scrubbedRunSearch(search: string): string {
  const params = new URLSearchParams(search);
  params.delete(RUN_OPEN_PARAM);
  const rest = params.toString();
  return rest === "" ? "" : `?${rest}`;
}

let parked: string | null = null;

/** Read the marker, scrub it from the address bar, and park it. Called once, from main.tsx. */
export function captureRunOpen(win: Window): string | null {
  const runId = readRunOpen(win.location.search);
  if (runId === null) return null;
  parked = runId;
  win.history.replaceState({}, "", `${win.location.pathname}${scrubbedRunSearch(win.location.search)}${win.location.hash}`);
  return runId;
}

/** Take the parked run, ONCE: a value that survived would reopen the window on every re-render. */
export function takeParkedRunOpen(): string | null {
  const held = parked;
  parked = null;
  return held;
}

/** Tests only. */
export function resetParkedRunOpenForTest(): void {
  parked = null;
}
