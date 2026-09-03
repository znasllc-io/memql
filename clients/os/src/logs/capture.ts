import { newShortId } from "@znasllc-io/memql-sdk-core/client";

// The OS front end's own log capture (epic memql#4895, spec H "Capture").
//
// ===========================================================================
// A QUEUE THAT IS NOT REACT STATE, AND A PATH THAT CANNOT RE-ENTER ITSELF
// ===========================================================================
// Window errors, unhandled rejections, console.error and console.warn are
// gathered here and sent to `logsRecordClient` in batches. Two things about
// the shape are load-bearing rather than tidy.
//
// The queue is a module-level array and never React state. A render error is
// exactly the moment React is least able to help, and a capture that set
// state on every console.warn would re-render whatever surface produced the
// warning -- which is how one warning becomes a hundred.
//
// The send is unawaited and its rejection is swallowed, and `busy` makes
// anything the capture path itself throws or logs go to the ORIGINAL console
// only. A capture that could log its own failure through the wrapper it
// installed would record that failure, fail again recording it, and so on
// until the queue cap was the only thing standing between the tab and a
// stalled event loop.
//
// `createCapture` is the pure core -- clock, transport, context, storage and
// scheduler are all injected -- so the batching rules are testable with no
// globals. `installCapture` below is the thin installer that hooks the
// window and the console and hands the core the real ones.

/** The two levels captured. Info is not: it is noise at the rate the shell
 *  would produce it, and the console keeps it. */
export type CaptureLevel = "warn" | "error";

/**
 * One line as `logsRecordClient` takes it.
 *
 * A type alias rather than an interface, deliberately: an interface has no
 * implicit index signature and is not assignable to the generated
 * `Record<string, unknown>[]` the SDK method declares for `lines`.
 */
export type CaptureLine = {
  at: string;
  level: CaptureLevel;
  app?: string;
  component?: string;
  message: string;
  attributes?: Record<string, unknown>;
  subject?: string;
  subjectConcept?: string;
};

/** What the Shell knows about where a line came from: the focused window's
 *  app and section, and the page path. A blank app is the shell itself. */
export interface CaptureContext {
  app: string;
  section: string;
  href: string;
}

export type CaptureTransport = (session: string, lines: CaptureLine[]) => Promise<unknown>;

/** What a caller reports. `error` is read for its stack; `section` and `app`
 *  override the context when the caller knows better -- the error boundary
 *  knows the exact app id and section it wraps. */
export interface CaptureReport {
  level: CaptureLevel;
  message: string;
  app?: string;
  section?: string;
  component?: string;
  attributes?: Record<string, unknown>;
  subject?: string;
  subjectConcept?: string;
  error?: unknown;
}

export interface CaptureStats {
  /** Lines held, waiting for a flush or a connection. */
  queued: number;
  /** Lines the cluster accepted, as it reported them. */
  sent: number;
  /** Calls made. */
  calls: number;
  /** Lines let go because the queue was full. The oldest go first. */
  droppedOverflow: number;
  /** Lines the cluster refused with `rate_limited`. Never retried. */
  droppedRateLimited: number;
  /** Lines whose send threw or rejected. Never retried. */
  droppedFailed: number;
}

export interface Capture {
  /** The tab's session id, `os-<shortId>`. A correlation key, never an authority. */
  readonly session: string;
  record(report: CaptureReport): void;
  /** Send what is queued now, if there is a transport. */
  flush(): void;
  setTransport(transport: CaptureTransport | null): void;
  setContext(context: (() => CaptureContext) | null): void;
  /** True while the capture path is running. Anything logged then goes to the
   *  original console only. */
  readonly busy: boolean;
  stats(): CaptureStats;
}

export interface CaptureOptions {
  now?: () => number;
  transport?: CaptureTransport | null;
  context?: (() => CaptureContext) | null;
  /** `null` is "no storage" -- a session id is minted and not kept. The
   *  default only replaces `undefined`. */
  storage?: Pick<Storage, "getItem" | "setItem"> | null;
  /** The timer. Returns the cancel. Injected so a test drives the clock. */
  schedule?: (fn: () => void, ms: number) => () => void;
  /** Where the capture's OWN failures go. The installer points this at the
   *  original console; the core never reaches for a global. */
  onInternalError?: (err: unknown) => void;
}

/** Flush when this long has passed since the first queued line. */
export const FLUSH_INTERVAL_MS = 2_000;
/** ...or as soon as this many lines are waiting. */
export const FLUSH_AT_LINES = 20;
/** The engine refuses a call carrying more than this (spec L9). */
export const MAX_LINES_PER_CALL = 50;
/** Held while there is no connection, or between flushes. Oldest dropped. */
export const QUEUE_CAP = 200;
/** The engine refuses a message over 4 KiB; this keeps under it. */
export const MAX_MESSAGE_CHARS = 4_000;
/** A stack trace's budget inside `attributes`. */
export const MAX_STACK_CHARS = 4_096;
/** The engine refuses attributes over 8 KiB per line. */
export const MAX_ATTRIBUTES_BYTES = 8_192;
export const SESSION_STORAGE_KEY = "memql-os-logs-session-v1";

/**
 * Attribute keys that never leave the browser (the diagnostics report's
 * posture, `buildDiagnosticsReport.ts`): the engine's redactor would catch
 * most of these too, but a line is redacted where it is made.
 */
const SECRET_KEY = /(pass|token|secret|key|auth|credential)/i;
/** Bearer material and MemQL's prefixed credentials, wherever they appear in
 *  a message. */
const BEARER_IN_TEXT = /\bBearer\s+[A-Za-z0-9._~+/=-]+/g;
const PREFIXED_CREDENTIAL = /\bmql_[a-z]{3}_[A-Za-z0-9_-]+/g;
const SESSION_SHAPE = /^os-[A-Za-z0-9_-]{4,61}$/;
const REDACTION_DEPTH = 4;

function defaultSchedule(fn: () => void, ms: number): () => void {
  const timer = setTimeout(fn, ms);
  return () => clearTimeout(timer);
}

function sessionStorageOrNull(): Pick<Storage, "getItem" | "setItem"> | null {
  try {
    return globalThis.sessionStorage ?? null;
  } catch {
    return null;
  }
}

/** The tab's session id: read back when this tab already has one, minted
 *  otherwise. Every storage touch is guarded -- a private window and a full
 *  quota are normal cases, not failures. */
export function resolveSession(storage: Pick<Storage, "getItem" | "setItem"> | null): string {
  try {
    const held = storage?.getItem(SESSION_STORAGE_KEY);
    if (typeof held === "string" && SESSION_SHAPE.test(held)) return held;
  } catch {
    // Unreadable storage reads as no session.
  }
  const minted = `os-${newShortId()}`;
  try {
    storage?.setItem(SESSION_STORAGE_KEY, minted);
  } catch {
    // Best-effort: a session id that does not survive a reload is still a
    // session id for this one.
  }
  return minted;
}

export function redactMessage(message: string): string {
  return message.replace(BEARER_IN_TEXT, "Bearer [redacted]").replace(PREFIXED_CREDENTIAL, "[redacted]");
}

/** Strip every key that looks like credential material, recursively, to a
 *  bounded depth. Arrays are walked; anything deeper than the budget is
 *  replaced by a marker rather than trusted. */
export function redactAttributes(value: unknown, depth = 0): unknown {
  if (value === null || typeof value !== "object") {
    return typeof value === "string" ? redactMessage(value) : value;
  }
  if (depth >= REDACTION_DEPTH) return "[nested]";
  if (Array.isArray(value)) return value.map((member) => redactAttributes(member, depth + 1));
  const out: Record<string, unknown> = {};
  for (const [key, member] of Object.entries(value as Record<string, unknown>)) {
    if (SECRET_KEY.test(key)) continue;
    out[key] = redactAttributes(member, depth + 1);
  }
  return out;
}

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

function safeStringify(value: unknown): string {
  try {
    const text = JSON.stringify(value);
    return text === undefined ? String(value) : text;
  } catch {
    return String(value);
  }
}

/** The stack of whatever was thrown, or "" when it carries none. */
export function stackOf(error: unknown): string {
  if (error instanceof Error && typeof error.stack === "string") return error.stack;
  if (error !== null && typeof error === "object" && typeof (error as { stack?: unknown }).stack === "string") {
    return (error as { stack: string }).stack;
  }
  return "";
}

/** A console argument list as one line, the way a person reads it. */
export function messageOf(args: readonly unknown[]): string {
  return args
    .map((part) => {
      if (typeof part === "string") return part;
      if (part instanceof Error) return part.message;
      if (part === undefined) return "undefined";
      return safeStringify(part);
    })
    .join(" ");
}

function errorIn(args: readonly unknown[]): unknown {
  return args.find((part) => part instanceof Error);
}

function contextOf(read: (() => CaptureContext) | null): CaptureContext {
  if (read === null) return { app: "", section: "", href: "" };
  try {
    const ctx = read();
    return {
      app: typeof ctx.app === "string" ? ctx.app.trim() : "",
      section: typeof ctx.section === "string" ? ctx.section : "",
      href: typeof ctx.href === "string" ? ctx.href : "",
    };
  } catch {
    return { app: "", section: "", href: "" };
  }
}

/** One report, as the wire will carry it: redacted, bounded, stamped with
 *  where it came from. Exported so the rules are testable on their own. */
export function lineFrom(report: CaptureReport, context: CaptureContext, at: number): CaptureLine {
  const app = (report.app ?? context.app).trim();
  const section = report.section ?? context.section;
  const raw: Record<string, unknown> = { ...(report.attributes ?? {}) };
  if (section !== "") raw.section = section;
  if (context.href !== "") raw.href = context.href;
  const stack = stackOf(report.error);
  if (stack !== "") raw.stack = stack.slice(0, MAX_STACK_CHARS);
  let attributes = redactAttributes(raw) as Record<string, unknown>;
  if (byteLength(safeStringify(attributes)) > MAX_ATTRIBUTES_BYTES) {
    // The stack is the usual culprit and the least structured; it goes first.
    const { stack: _stack, ...rest } = attributes;
    attributes = rest;
    if (byteLength(safeStringify(attributes)) > MAX_ATTRIBUTES_BYTES) {
      attributes = { attributesDropped: true, ...(section !== "" ? { section } : {}) };
    }
  }
  const line: CaptureLine = {
    at: new Date(at).toISOString(),
    level: report.level,
    component: report.component ?? (app !== "" ? `os.${app}` : "os.shell"),
    message: redactMessage(report.message).slice(0, MAX_MESSAGE_CHARS),
    attributes,
  };
  // Omitted rather than blank: the engine validates `app` against a shape a
  // blank does not meet, and a blank app IS the shell, which `component`
  // already says.
  if (app !== "") line.app = app;
  if (report.subject !== undefined && report.subject !== "") line.subject = report.subject;
  if (report.subjectConcept !== undefined && report.subjectConcept !== "") line.subjectConcept = report.subjectConcept;
  return line;
}

/** Identical lines in one batch become one line carrying `attributes.repeat`.
 *  Identity is level + message; the first occurrence's stamp and attributes
 *  are kept. Order is first-occurrence order. */
export function collapse(lines: readonly CaptureLine[]): CaptureLine[] {
  const seen = new Map<string, { line: CaptureLine; repeat: number }>();
  for (const line of lines) {
    const key = `${line.level} ${line.message}`;
    const held = seen.get(key);
    if (held) held.repeat += 1;
    else seen.set(key, { line, repeat: 1 });
  }
  return [...seen.values()].map(({ line, repeat }) =>
    repeat === 1 ? line : { ...line, attributes: { ...(line.attributes ?? {}), repeat } },
  );
}

/** The engine's reply, read off either a `Result` (the SDK method's answer)
 *  or a plain row -- the transport may hand back either. */
function replyOf(value: unknown): { accepted: number; reason: string } {
  let row: unknown = value;
  if (row !== null && typeof row === "object" && typeof (row as { rows?: unknown }).rows === "function") {
    try {
      const rows = (row as { rows: () => unknown[] }).rows();
      row = rows[0] ?? {};
    } catch {
      row = {};
    }
  }
  if (row === null || typeof row !== "object") return { accepted: -1, reason: "" };
  const record = row as Record<string, unknown>;
  const accepted = typeof record.accepted === "number" ? record.accepted : -1;
  const reason = typeof record.reason === "string" ? record.reason : "";
  return { accepted, reason };
}

export function createCapture(options: CaptureOptions = {}): Capture {
  const now = options.now ?? (() => Date.now());
  const schedule = options.schedule ?? defaultSchedule;
  const session = resolveSession(options.storage === undefined ? sessionStorageOrNull() : options.storage);
  let transport: CaptureTransport | null = options.transport ?? null;
  let context: (() => CaptureContext) | null = options.context ?? null;
  const queue: CaptureLine[] = [];
  let cancelTimer: (() => void) | null = null;
  let busy = false;
  const stats: CaptureStats = {
    queued: 0,
    sent: 0,
    calls: 0,
    droppedOverflow: 0,
    droppedRateLimited: 0,
    droppedFailed: 0,
  };

  /** Run one step of the capture path. A step that runs while another is
   *  running is dropped -- that is the re-entrancy guard -- and a step that
   *  throws reports to the internal sink and never to the caller. */
  function guarded(step: () => void): void {
    if (busy) return;
    busy = true;
    try {
      step();
    } catch (err) {
      try {
        options.onInternalError?.(err);
      } catch {
        // The sink itself failing is the end of the line.
      }
    } finally {
      busy = false;
    }
  }

  function arm(): void {
    if (cancelTimer !== null) return;
    cancelTimer = schedule(() => {
      cancelTimer = null;
      flushNow();
    }, FLUSH_INTERVAL_MS);
  }

  function disarm(): void {
    if (cancelTimer === null) return;
    cancelTimer();
    cancelTimer = null;
  }

  function send(via: CaptureTransport, batch: CaptureLine[]): void {
    stats.calls += 1;
    let promise: Promise<unknown>;
    try {
      promise = Promise.resolve(via(session, batch));
    } catch {
      stats.droppedFailed += batch.length;
      return;
    }
    promise.then(
      (reply) => {
        const { accepted, reason } = replyOf(reply);
        if (reason === "rate_limited") {
          // Drop and count, never retry: a bucket that is empty now is empty
          // for the next two seconds as well, and a retry would spend it on
          // the same lines again.
          stats.droppedRateLimited += batch.length;
          return;
        }
        stats.sent += accepted >= 0 ? accepted : batch.length;
      },
      () => {
        stats.droppedFailed += batch.length;
      },
    );
  }

  /** Take one batch off the queue and send it. Called only from inside a
   *  guarded step. */
  function drain(): void {
    if (transport === null || queue.length === 0) return;
    disarm();
    const batch = collapse(queue.splice(0, MAX_LINES_PER_CALL));
    send(transport, batch);
    // What is left keeps the cadence rather than draining at once: the
    // engine's bucket refills two lines a second, and a burst sent as five
    // back-to-back calls would spend it on the first two.
    if (queue.length > 0) arm();
  }

  function flushNow(): void {
    guarded(drain);
  }

  return {
    session,
    get busy() {
      return busy;
    },
    record(report) {
      guarded(() => {
        queue.push(lineFrom(report, contextOf(context), now()));
        if (queue.length > QUEUE_CAP) {
          const overflow = queue.length - QUEUE_CAP;
          queue.splice(0, overflow);
          stats.droppedOverflow += overflow;
        }
        if (transport === null) return;
        if (queue.length >= FLUSH_AT_LINES) drain();
        else arm();
      });
    },
    flush() {
      flushNow();
    },
    setTransport(next) {
      transport = next;
      if (next === null) {
        disarm();
        return;
      }
      // A connection arriving finds whatever was held while there was none.
      if (queue.length > 0) flushNow();
    },
    setContext(next) {
      context = next;
    },
    stats() {
      return { ...stats, queued: queue.length };
    },
  };
}

// ---------------------------------------------------------------------------
// The installer: the one place the globals are touched
// ---------------------------------------------------------------------------

/** The slice of `window` the installer needs. Narrow so a test can hand it a
 *  plain object and never wrap the real console. */
export interface CaptureTarget {
  console: { error: (...args: unknown[]) => void; warn: (...args: unknown[]) => void };
  addEventListener: (type: string, listener: (event: never) => void) => void;
}

let singleton: Capture | null = null;
let hooked = false;

function instance(options: CaptureOptions = {}): Capture {
  if (singleton === null) singleton = createCapture(options);
  return singleton;
}

/**
 * Install once: wrap console.error and console.warn (calling through to the
 * originals -- nothing is silenced), listen for window errors, unhandled
 * rejections and pagehide. A second call is a no-op.
 *
 * The originals are captured HERE and the core's internal-error sink points
 * at them, so a fault inside the capture path prints once, through the
 * console as it was, and is never recorded.
 */
export function installCapture(
  target: CaptureTarget = window as unknown as CaptureTarget,
  options: CaptureOptions = {},
): Capture {
  const originalError = target.console.error.bind(target.console);
  const originalWarn = target.console.warn.bind(target.console);
  const cap = instance({
    ...options,
    onInternalError: options.onInternalError ?? ((err) => originalError("[memql-os logs]", err)),
  });
  if (hooked) return cap;
  hooked = true;

  const fromConsole = (level: CaptureLevel, args: unknown[]): void => {
    // `busy` is the re-entrancy check: a console call made BY the capture
    // path reaches the original above and stops here.
    if (cap.busy) return;
    cap.record({ level, message: messageOf(args), error: errorIn(args), attributes: { source: "console" } });
  };
  target.console.error = (...args: unknown[]) => {
    originalError(...args);
    fromConsole("error", args);
  };
  target.console.warn = (...args: unknown[]) => {
    originalWarn(...args);
    fromConsole("warn", args);
  };

  target.addEventListener("error", (event: ErrorEvent) => {
    cap.record({
      level: "error",
      message: event.message || messageOf([event.error ?? "Uncaught error"]),
      error: event.error,
      attributes: {
        source: "window",
        ...(event.filename ? { filename: event.filename } : {}),
        ...(event.lineno ? { line: event.lineno } : {}),
        ...(event.colno ? { column: event.colno } : {}),
      },
    });
  });
  target.addEventListener("unhandledrejection", (event: PromiseRejectionEvent) => {
    const reason: unknown = event.reason;
    cap.record({
      level: "error",
      message: `Unhandled rejection: ${messageOf([reason])}`,
      error: reason,
      attributes: { source: "unhandledrejection" },
    });
  });
  // A best-effort flush as the page goes away. The send is unawaited like
  // every other; what pagehide buys is the attempt.
  target.addEventListener("pagehide", () => cap.flush());
  return cap;
}

/** Where a line comes from, read at capture time. The Shell installs this
 *  with the focused window's app and section; `null` clears it. */
export function setCaptureContext(read: (() => CaptureContext) | null): void {
  instance().setContext(read);
}

/** The send. The connection provider sets it when a connection exists and
 *  clears it on disconnect; while it is null the queue holds up to the cap. */
export function setCaptureTransport(transport: CaptureTransport | null): void {
  instance().setTransport(transport);
}

/** Record a line from application code -- the error boundary's seam. */
export function report(line: CaptureReport): void {
  instance().record(line);
}

/** Send what is queued now. */
export function flushCapture(): void {
  instance().flush();
}

export function captureStats(): CaptureStats {
  return instance().stats();
}

export function captureSession(): string {
  return instance().session;
}

/** Forget the singleton and the hook flag. Tests only: the installer is
 *  once-per-page by design, and a suite needs a fresh page per case. */
export function resetCaptureForTest(): void {
  singleton = null;
  hooked = false;
}
