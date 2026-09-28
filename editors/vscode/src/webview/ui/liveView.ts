// The host half of a live page: assign a document once per screen, then keep
// it current by message.
//
// THE PROBLEM IT REMOVES. A panel that re-renders by assigning `webview.html`
// replaces the whole document -- and an install re-rendered once per line of
// output. Every repaint reset scroll, focus, the caret, text selection and the
// log pane's position, restarted every animation, and re-sent the full
// stylesheet and the entire log across the bridge. This class makes a render
// cheap and non-destructive: a NEW screen (or a theme change, see
// `invalidate`) assigns a document; the SAME screen posts a `patch` carrying
// only the regions whose HTML changed, and runtime.ts morphs them in place.
// Progress and log lines are separate streams (`progress`, `log`) that never
// touch a region's HTML at all.
//
// THE READY HANDSHAKE. A webview drops messages while it is hidden or still
// loading, and a panel without `retainContextWhenHidden` is rebuilt from the
// last ASSIGNED html when it is shown again -- without any of the patches that
// followed. So the page posts `ready` on every load, and `handleMessage` then
// brings that fresh document up to date: the regions that differ from what was
// assigned, the latest progress, and the whole buffered log. Until the first
// `ready`, renders of the same screen are only remembered (re-assigning the
// document every time would reload a page that never gets far enough to say
// it is ready).
//
// PROGRESS VALUES WIN. Within one screen the page re-applies the latest
// `progress` message after every patch, so a region re-rendered from older
// state cannot move the bar backwards. A panel that uses `progress()` should
// route every change of those values through it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go):
// the target is two methods, which a WebviewPanel's webview satisfies through
// a two-line adapter and a test satisfies with a recorder.

import { PAGE_LOG_LIMIT } from "./runtime.js";
import type { HostToPage, LogLine, ProgressUpdate } from "./protocol.js";

export type { HostToPage, LogLine, PageToHost, ProgressUpdate } from "./protocol.js";

/** What LiveView drives: a webview's `html` setter and its `postMessage`. */
export interface LiveTarget {
  setHtml(html: string): void;
  postMessage(msg: unknown): unknown;
}

/** The three page regions, as already-rendered HTML (kit `head`, the body, kit `actionBar`). */
export interface RegionParts {
  head: string;
  body: string;
  actions: string;
}

const REGIONS: readonly (keyof RegionParts)[] = ["head", "body", "actions"];

/** The regions of `next` whose HTML differs from `prev`, or undefined when none do. */
function changedRegions(prev: RegionParts, next: RegionParts): Record<string, string> | undefined {
  let out: Record<string, string> | undefined;
  for (const name of REGIONS) {
    if (prev[name] !== next[name]) (out ??= {})[name] = next[name];
  }
  return out;
}

export interface LiveViewOptions {
  /** The most log lines kept for re-sending on `ready` (default: the page's own limit). */
  maxLogLines?: number;
}

export class LiveView {
  /** The screen the current document was assigned for; undefined forces the next render to assign. */
  private screen: string | undefined;
  /** Whether the current document has posted `ready` since it was assigned or last reloaded. */
  private ready = false;
  /** The parts the current document was BUILT from: what a reloaded page shows. */
  private assigned: RegionParts | undefined;
  /** The parts the page shows now, after the patches it was sent. */
  private shown: RegionParts | undefined;
  /** The latest parts a render asked for. */
  private latest: RegionParts | undefined;
  private lastProgress: ProgressUpdate | undefined;
  private buffer: LogLine[] = [];
  /** Whether the log was ever reset: a reloaded page must then drop any lines its document was built with. */
  private logWasReset = false;
  private readonly maxLogLines: number;

  constructor(
    private readonly target: LiveTarget,
    private readonly wrap: (parts: RegionParts, screenKey: string) => string,
    options: LiveViewOptions = {},
  ) {
    this.maxLogLines = Math.max(1, Math.floor(options.maxLogLines ?? PAGE_LOG_LIMIT));
  }

  /**
   * Show a screen. A new `screenKey` (or the first render, or one after
   * `invalidate`) assigns a whole document; the same key posts only the
   * regions that changed, or nothing when none did.
   */
  render(screenKey: string, parts: RegionParts): void {
    const next: RegionParts = { head: parts.head, body: parts.body, actions: parts.actions };
    this.latest = next;
    if (screenKey !== this.screen || this.assigned === undefined) {
      this.screen = screenKey;
      this.assigned = next;
      this.shown = next;
      this.ready = false;
      this.target.setHtml(this.wrap(next, screenKey));
      return;
    }
    if (!this.ready || this.shown === undefined) return;
    const regions = changedRegions(this.shown, next);
    if (regions === undefined) return;
    this.shown = next;
    this.send({ type: "patch", regions });
  }

  /** The live values of the page's progress region. Remembered, and re-sent on every `ready`. */
  progress(p: ProgressUpdate): void {
    this.lastProgress = { ...p };
    if (this.ready) this.send({ type: "progress", ...this.lastProgress });
  }

  /**
   * Append lines to the page's log panes, or replace them with `reset`.
   *
   * The buffer keeps the most recent lines (bounded, oldest dropped) so a page
   * that reloads gets the whole log back; only the NEW lines cross the bridge
   * while the page is up.
   */
  log(lines: readonly LogLine[], opts: { reset?: boolean } = {}): void {
    const reset = opts.reset === true;
    if (reset) {
      this.buffer = [];
      this.logWasReset = true;
    }
    const fresh = lines.length > this.maxLogLines ? lines.slice(lines.length - this.maxLogLines) : lines;
    for (const line of fresh) this.buffer.push({ ...line });
    if (this.buffer.length > this.maxLogLines) this.buffer.splice(0, this.buffer.length - this.maxLogLines);
    if (!this.ready || (fresh.length === 0 && !reset)) return;
    this.send({ type: "log", lines: fresh.map((line) => ({ ...line })), reset });
  }

  /**
   * Consume the page's `ready`: bring the freshly loaded document up to date
   * with the latest regions, progress and the buffered log. Returns true when
   * the message was `ready` (so the panel's own handler can skip it).
   */
  handleMessage(msg: unknown): boolean {
    if (typeof msg !== "object" || msg === null || (msg as { type?: unknown }).type !== "ready") return false;
    this.ready = true;
    if (this.assigned !== undefined && this.latest !== undefined) {
      const regions = changedRegions(this.assigned, this.latest);
      if (regions !== undefined) this.send({ type: "patch", regions });
      this.shown = this.latest;
    }
    if (this.lastProgress !== undefined) this.send({ type: "progress", ...this.lastProgress });
    if (this.buffer.length > 0 || this.logWasReset) {
      this.send({ type: "log", lines: this.buffer.map((line) => ({ ...line })), reset: true });
    }
    return true;
  }

  /** The next render assigns a whole document (a theme or appearance change restyles every rule). */
  invalidate(): void {
    this.screen = undefined;
  }

  /** The buffered log, oldest first (for Copy and Open in Output). */
  logLines(): readonly LogLine[] {
    return this.buffer;
  }

  private send(msg: HostToPage): void {
    this.target.postMessage(msg);
  }
}
