import { afterEach, describe, expect, it, vi } from "vitest";

import {
  FLUSH_AT_LINES,
  FLUSH_INTERVAL_MS,
  MAX_LINES_PER_CALL,
  MAX_STACK_CHARS,
  QUEUE_CAP,
  captureStats,
  collapse,
  createCapture,
  flushCapture,
  installCapture,
  lineFrom,
  redactAttributes,
  redactMessage,
  report,
  resetCaptureForTest,
  resolveSession,
  setCaptureTransport,
  type CaptureLine,
  type CaptureOptions,
  type CaptureTarget,
} from "../../src/logs/capture";

// The front end's own capture (epic memql#4895, spec H): batches, collapses,
// caps, never throws, drops on rate_limited, and never carries a credential.

interface Sent {
  session: string;
  lines: CaptureLine[];
}

/** The core with every seam injected: a clock, a recording transport and a
 *  scheduler the test fires by hand. */
function harness(over: Partial<CaptureOptions> = {}) {
  const sent: Sent[] = [];
  const timers: { fn: () => void; ms: number; cancelled: boolean }[] = [];
  let clock = 1_700_000_000_000;
  const cap = createCapture({
    now: () => clock,
    storage: null,
    transport: async (session, lines) => {
      sent.push({ session, lines });
      return { accepted: lines.length, dropped: 0, reason: "" };
    },
    schedule: (fn, ms) => {
      const timer = { fn, ms, cancelled: false };
      timers.push(timer);
      return () => {
        timer.cancelled = true;
      };
    },
    ...over,
  });
  /** Fire every armed timer once. */
  const fire = (): void => {
    const pending = timers.filter((t) => !t.cancelled);
    timers.length = 0;
    for (const t of pending) t.fn();
  };
  return { cap, sent, timers, fire, advance: (ms: number) => void (clock += ms) };
}

function line(n: number, level: "warn" | "error" = "warn"): { level: "warn" | "error"; message: string } {
  return { level, message: `line ${n}` };
}

/** Let an unawaited send settle. A macrotask rather than a microtask or
 *  two: an async transport returning a rejected promise settles through a
 *  thenable job, which is one tick more than a fulfilment. */
async function settled(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe("batching", () => {
  it("flushes as soon as twenty lines are waiting", () => {
    const { cap, sent, timers } = harness();
    for (let i = 0; i < FLUSH_AT_LINES - 1; i += 1) cap.record(line(i));
    expect(sent).toHaveLength(0);
    // One timer armed, for the interval.
    expect(timers.filter((t) => !t.cancelled).map((t) => t.ms)).toEqual([FLUSH_INTERVAL_MS]);
    cap.record(line(FLUSH_AT_LINES - 1));
    expect(sent).toHaveLength(1);
    expect(sent[0]?.lines).toHaveLength(FLUSH_AT_LINES);
    // The interval timer was disarmed by the flush.
    expect(timers.every((t) => t.cancelled)).toBe(true);
  });

  it("flushes on the interval when fewer than twenty are waiting", () => {
    const { cap, sent, fire } = harness();
    cap.record(line(1));
    cap.record(line(2));
    expect(sent).toHaveLength(0);
    fire();
    expect(sent).toHaveLength(1);
    expect(sent[0]?.lines.map((l) => l.message)).toEqual(["line 1", "line 2"]);
  });

  it("sends at most fifty per call, keeping the cadence for what is left", () => {
    const { cap, sent, fire } = harness({ transport: null });
    for (let i = 0; i < 120; i += 1) cap.record(line(i));
    expect(cap.stats().queued).toBe(120);
    const sink: Sent[] = [];
    cap.setTransport(async (session, lines) => {
      sink.push({ session, lines });
      return { accepted: lines.length, dropped: 0, reason: "" };
    });
    expect(sink.map((s) => s.lines.length)).toEqual([MAX_LINES_PER_CALL]);
    fire();
    expect(sink.map((s) => s.lines.length)).toEqual([MAX_LINES_PER_CALL, MAX_LINES_PER_CALL]);
    fire();
    expect(sink.map((s) => s.lines.length)).toEqual([MAX_LINES_PER_CALL, MAX_LINES_PER_CALL, 20]);
    expect(sent).toHaveLength(0);
  });

  it("caps the queue at two hundred while there is no connection, dropping the OLDEST and counting them", () => {
    const { cap } = harness({ transport: null });
    for (let i = 0; i < QUEUE_CAP + 37; i += 1) cap.record(line(i));
    const stats = cap.stats();
    expect(stats.queued).toBe(QUEUE_CAP);
    expect(stats.droppedOverflow).toBe(37);
    const sink: Sent[] = [];
    cap.setTransport(async (session, lines) => {
      sink.push({ session, lines });
      return {};
    });
    // The first line still held is the thirty-eighth recorded.
    expect(sink[0]?.lines[0]?.message).toBe("line 37");
  });

  it("collapses identical lines within one flush into one carrying attributes.repeat", () => {
    const { cap, sent, fire } = harness();
    for (let i = 0; i < 5; i += 1) cap.record({ level: "error", message: "boom" });
    cap.record({ level: "warn", message: "boom" });
    cap.record({ level: "error", message: "other" });
    fire();
    expect(sent).toHaveLength(1);
    const lines = sent[0]?.lines ?? [];
    expect(lines.map((l) => [l.level, l.message, l.attributes?.repeat])).toEqual([
      ["error", "boom", 5],
      ["warn", "boom", undefined],
      ["error", "other", undefined],
    ]);
  });

  it("collapse keeps the first occurrence's stamp and attributes", () => {
    const a: CaptureLine = { at: "t1", level: "warn", message: "x", attributes: { section: "browse" } };
    const b: CaptureLine = { at: "t2", level: "warn", message: "x", attributes: { section: "backups" } };
    expect(collapse([a, b])).toEqual([{ at: "t1", level: "warn", message: "x", attributes: { section: "browse", repeat: 2 } }]);
  });
});

describe("it never throws, and never retries", () => {
  it("survives a transport that throws synchronously, and counts the lines as failed", () => {
    const { cap, fire } = harness({
      transport: () => {
        throw new Error("no wire");
      },
    });
    expect(() => {
      cap.record(line(1));
      fire();
    }).not.toThrow();
    expect(cap.stats().droppedFailed).toBe(1);
    expect(cap.stats().calls).toBe(1);
  });

  it("survives a transport that rejects, and counts the lines as failed", async () => {
    const { cap, fire } = harness({ transport: async () => Promise.reject(new Error("refused")) });
    cap.record(line(1));
    cap.record(line(2));
    fire();
    await settled();
    expect(cap.stats().droppedFailed).toBe(2);
  });

  it("drops what the cluster rate-limited and does not retry it", async () => {
    const { cap, sent, fire, timers } = harness({
      transport: async (session, lines) => {
        sent.push({ session, lines });
        return { accepted: 0, dropped: lines.length, reason: "rate_limited" };
      },
    });
    for (let i = 0; i < 3; i += 1) cap.record(line(i));
    fire();
    await settled();
    expect(cap.stats().droppedRateLimited).toBe(3);
    expect(cap.stats().queued).toBe(0);
    // Nothing re-armed: the lines are gone.
    expect(timers.every((t) => t.cancelled)).toBe(true);
    fire();
    expect(sent).toHaveLength(1);
  });

  it("reads the reply off a Result-shaped answer as well as a plain row", async () => {
    const { cap, fire } = harness({
      transport: async () => ({ rows: () => [{ accepted: 2, dropped: 0, reason: "" }] }),
    });
    cap.record(line(1));
    cap.record(line(2));
    fire();
    await settled();
    expect(cap.stats().sent).toBe(2);
  });

  it("survives a context that throws: the line is stamped as the shell's", () => {
    const { cap, sent, fire } = harness({
      context: () => {
        throw new Error("no shell");
      },
    });
    cap.record(line(1));
    fire();
    expect(sent[0]?.lines[0]?.app).toBeUndefined();
    expect(sent[0]?.lines[0]?.component).toBe("os.shell");
  });

  it("reports its own failure to the internal sink, never to the caller", () => {
    const failures: unknown[] = [];
    const { cap } = harness({
      onInternalError: (err) => failures.push(err),
      // A scheduler that throws is the capture path itself failing.
      schedule: () => {
        throw new Error("no timers here");
      },
    });
    expect(() => cap.record(line(1))).not.toThrow();
    expect(failures).toHaveLength(1);
  });
});

describe("what a line carries", () => {
  const context = { app: "files", section: "browse", href: "/desk?x=1" };

  it("is stamped with the focused window's app and section and the page path, and derives the component", () => {
    const made = lineFrom({ level: "warn", message: "hm" }, context, 1_700_000_000_000);
    expect(made.at).toBe("2023-11-14T22:13:20.000Z");
    expect(made.app).toBe("files");
    expect(made.component).toBe("os.files");
    expect(made.attributes).toEqual({ section: "browse", href: "/desk?x=1" });
  });

  it("omits the app and names the shell when no window is focused", () => {
    const made = lineFrom({ level: "error", message: "x" }, { app: "", section: "", href: "" }, 0);
    expect("app" in made).toBe(false);
    expect(made.component).toBe("os.shell");
    expect(made.attributes).toEqual({});
  });

  it("lets a caller that knows better name the app and section exactly -- the error boundary's case", () => {
    const made = lineFrom(
      { level: "error", message: "x", app: "fleet", section: "routing", component: "os.fleet" },
      context,
      0,
    );
    expect(made.app).toBe("fleet");
    expect(made.attributes?.section).toBe("routing");
  });

  it("strips every attribute key that looks like credential material, at every depth", () => {
    const made = lineFrom(
      {
        level: "error",
        message: "x",
        attributes: {
          token: "t",
          apiKey: "k",
          password: "p",
          Authorization: "a",
          clientSecret: "s",
          credentials: { user: "u" },
          safe: 1,
          nested: { secretKey: "q", ok: true, list: [{ passphrase: "z", keep: 2 }] },
        },
      },
      { app: "", section: "", href: "" },
      0,
    );
    expect(made.attributes).toEqual({ safe: 1, nested: { ok: true, list: [{ keep: 2 }] } });
  });

  it("redacts bearer material and prefixed credentials inside a message", () => {
    expect(redactMessage("401 with Authorization: Bearer abc.DEF-ghi_jkl")).toBe(
      "401 with Authorization: Bearer [redacted]",
    );
    expect(redactMessage("token mql_pat_ABCdef123-_x was refused")).toBe("token [redacted] was refused");
    expect(redactAttributes({ note: "Bearer xyz" })).toEqual({ note: "Bearer [redacted]" });
  });

  it("carries a stack, truncated to 4 KiB", () => {
    const error = new Error("deep");
    error.stack = "x".repeat(MAX_STACK_CHARS * 3);
    const made = lineFrom({ level: "error", message: "deep", error }, { app: "", section: "", href: "" }, 0);
    expect((made.attributes?.stack as string).length).toBe(MAX_STACK_CHARS);
  });

  it("truncates a message that would be refused for its size", () => {
    const made = lineFrom({ level: "error", message: "m".repeat(10_000) }, { app: "", section: "", href: "" }, 0);
    expect(made.message.length).toBe(4_000);
  });
});

describe("the session id", () => {
  function memory(): Pick<Storage, "getItem" | "setItem"> & { data: Map<string, string> } {
    const data = new Map<string, string>();
    return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v) };
  }

  it("is minted once per tab as os-<shortId> and kept in the storage handed to it", () => {
    const storage = memory();
    const first = resolveSession(storage);
    expect(first).toMatch(/^os-[0-9a-f]{16}$/);
    expect(resolveSession(storage)).toBe(first);
    expect([...storage.data.values()]).toEqual([first]);
  });

  it("replaces a stored value that is not a session id, and survives storage that throws", () => {
    const storage = memory();
    storage.setItem("memql-os-logs-session-v1", "not a session");
    expect(resolveSession(storage)).toMatch(/^os-[0-9a-f]{16}$/);
    const broken: Pick<Storage, "getItem" | "setItem"> = {
      getItem: () => {
        throw new Error("no storage");
      },
      setItem: () => {
        throw new Error("no storage");
      },
    };
    expect(resolveSession(broken)).toMatch(/^os-[0-9a-f]{16}$/);
    expect(resolveSession(null)).toMatch(/^os-[0-9a-f]{16}$/);
  });
});

describe("the installer", () => {
  afterEach(() => resetCaptureForTest());

  /** A window-shaped object, so the real console is never wrapped. */
  function target() {
    const listeners: Record<string, (event: never) => void> = {};
    const originalError = vi.fn();
    const originalWarn = vi.fn();
    const t: CaptureTarget = {
      console: { error: originalError, warn: originalWarn },
      addEventListener: (type, listener) => {
        listeners[type] = listener;
      },
    };
    return { t, listeners, originalError, originalWarn };
  }

  it("wraps console.error and console.warn, calling through to the originals, and records each", () => {
    const { t, originalError, originalWarn } = target();
    resetCaptureForTest();
    const cap = installCapture(t, { storage: null, schedule: () => () => {} });
    t.console.error("bad thing", new Error("why"));
    t.console.warn("careful");
    expect(originalError).toHaveBeenCalledTimes(1);
    expect(originalWarn).toHaveBeenCalledTimes(1);
    expect(cap.stats().queued).toBe(2);
  });

  it("installs once: a second call changes nothing", () => {
    const { t } = target();
    resetCaptureForTest();
    installCapture(t, { storage: null, schedule: () => () => {} });
    const wrapped = t.console.error;
    installCapture(t);
    expect(t.console.error).toBe(wrapped);
  });

  it("records window errors and unhandled rejections, and flushes on pagehide", async () => {
    const { t, listeners } = target();
    resetCaptureForTest();
    const sent: Sent[] = [];
    installCapture(t, {
      storage: null,
      schedule: () => () => {},
      transport: async (session, lines) => {
        sent.push({ session, lines });
        return {};
      },
    });
    listeners.error?.({ message: "boom", error: new Error("boom"), filename: "app.js", lineno: 3, colno: 9 } as never);
    listeners.unhandledrejection?.({ reason: new Error("nope") } as never);
    listeners.pagehide?.(undefined as never);
    expect(sent).toHaveLength(1);
    const lines = sent[0]?.lines ?? [];
    expect(lines.map((l) => l.message)).toEqual(["boom", "Unhandled rejection: nope"]);
    expect(lines[0]?.attributes).toMatchObject({ source: "window", filename: "app.js", line: 3, column: 9 });
    expect(typeof lines[0]?.attributes?.stack).toBe("string");
  });

  it("does not record what the capture path itself logs -- the re-entrancy guard", () => {
    const { t, originalError } = target();
    resetCaptureForTest();
    const cap = installCapture(t, {
      storage: null,
      schedule: () => () => {},
      transport: (): Promise<unknown> => {
        // The send logging through the console it wrapped.
        t.console.error("inside the send");
        return Promise.resolve({});
      },
    });
    for (let i = 0; i < FLUSH_AT_LINES; i += 1) t.console.warn(`w${i}`);
    // The original still saw the inner line; the queue did not.
    expect(originalError).toHaveBeenCalledWith("inside the send");
    expect(cap.stats().queued).toBe(0);
    expect(cap.stats().calls).toBe(1);
  });

  it("offers report() and flushCapture() over the same singleton, for the error boundary", () => {
    resetCaptureForTest();
    const sent: Sent[] = [];
    setCaptureTransport(async (session, lines) => {
      sent.push({ session, lines });
      return {};
    });
    report({ level: "error", app: "fleet", section: "routing", message: "render failed" });
    flushCapture();
    expect(sent).toHaveLength(1);
    expect(sent[0]?.session).toMatch(/^os-/);
    expect(sent[0]?.lines[0]).toMatchObject({ app: "fleet", component: "os.fleet", message: "render failed" });
    expect(captureStats().calls).toBe(1);
  });
});
