import { artifactContentPath } from "../../files/actions/download";

// A failed step's last lines (design record D13: "a failed step's last lines
// inline with open the full log"), read from the step's full log in the
// Library.
//
// WHY THE LIBRARY FILE AND NOT A ROW. The runner archives every step that ran
// as `<stepKey>.log`, owned by the pipeline's owner (epic memql#5478), and no
// row carries a log line -- the seam keeps the last lines in memory only, for
// the check run's text. So the page reads the tail of the one copy that
// exists, through the route Files downloads through: `GET
// /artifacts/{id}/content`, which re-resolves the file under the caller's own
// actor and honours a single suffix Range. A file the caller cannot read is a
// 404, which is the same answer as a file that is not there.
//
// THE LAST 16 KiB, then the last 40 lines of it -- the bound the runner holds
// its own tail to. A node whose downloader cannot stream answers the whole
// body (200) rather than the range; the tail is cut here either way.

export const TAIL_BYTES = 16 * 1024;
export const TAIL_LINES = 40;

export interface LogTail {
  lines: string[];
  /** Earlier lines exist that are not shown: open the full log for them. */
  truncated: boolean;
}

/** The last lines of a text, the first partial line dropped when the text was cut. */
export function tailOf(text: string, cut: boolean, maxLines = TAIL_LINES): LogTail {
  let lines = text.replace(/\r\n/g, "\n").split("\n");
  if (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  if (cut && lines.length > 1) lines = lines.slice(1);
  const truncated = cut || lines.length > maxLines;
  return { lines: lines.slice(-maxLines), truncated };
}

export async function fetchLogTail(input: {
  artifactId: string;
  bearer: () => Promise<string | null>;
  signal?: AbortSignal;
  baseUrl?: string;
  fetchImpl?: typeof fetch;
}): Promise<LogTail> {
  const fetchImpl = input.fetchImpl ?? fetch;
  const token = await input.bearer();
  const headers: Record<string, string> = { Range: `bytes=-${TAIL_BYTES}` };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const response = await fetchImpl(artifactContentPath(input.baseUrl ?? import.meta.env.BASE_URL, input.artifactId), {
    method: "GET",
    credentials: "same-origin",
    headers,
    ...(input.signal ? { signal: input.signal } : {}),
  });
  if (!response.ok) {
    throw new Error(response.status === 404 ? "The log is not readable here." : `The cluster refused the log (${response.status}).`);
  }
  const text = await response.text();
  if (response.status === 206) {
    // "bytes <start>-<end>/<size>": a start past zero means earlier bytes exist.
    const range = /bytes (\d+)-\d+\/\d+/.exec(response.headers.get("Content-Range") ?? "");
    return tailOf(text, range !== null && Number(range[1]) > 0);
  }
  return tailOf(text.length > TAIL_BYTES ? text.slice(-TAIL_BYTES) : text, text.length > TAIL_BYTES);
}
