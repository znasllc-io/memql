// The messages a MemQL page and its panel host exchange, typed once.
//
// ONE PROTOCOL FOR EVERY PANEL. A page's document is assigned once per screen;
// everything that changes while that screen is showing travels as one of the
// messages below, through `LiveView` on the host (liveView.ts) and the single
// page script on the other side (runtime.ts). Declaring the shapes here, in a
// module both halves and the tests import, is what keeps the two from
// drifting: the runtime is a string and cannot import a type, so this file is
// the contract it is checked against in review and in test/pageRuntime.test.ts.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

/** One line of a live log, as the page renders it: an optional step label, then the text. */
export interface LogLine {
  /** The running step's short label, drawn as a quiet prefix. */
  label?: string;
  /** The line itself. Always inserted as text, never as markup. */
  text: string;
  /** `error` for a line worth the danger colour, `muted` for chatter. */
  tone?: "error" | "muted";
}

/** Where a long operation stands. */
export type ProgressState = "running" | "stopping" | "failed" | "done";

/** The live values of a progress region (kit `progress`). */
export interface ProgressUpdate {
  /** 0 to 100. Absent means the bar is indeterminate. */
  percent?: number;
  /** The one status line: the running step's label or its current phase. */
  status: string;
  /** The position, e.g. "Step 6 of 16". The elapsed clock is appended to it. */
  stepText?: string;
  /** Epoch milliseconds the operation started; drives the elapsed clock. */
  startedAt?: number;
  /** Epoch milliseconds it settled; freezes the clock at the true duration. */
  endedAt?: number;
  state: ProgressState;
  /** Replaces the title, for the settled screens ("MemQL is installed"). */
  title?: string;
}

/** Host to page. */
export type HostToPage =
  | { type: "patch"; regions: Readonly<Record<string, string>> }
  | ({ type: "progress" } & ProgressUpdate)
  | { type: "log"; lines: readonly LogLine[]; reset?: boolean }
  | { type: "setDisclosure"; id: string; open: boolean };

/**
 * Page to host.
 *
 * `ready` is posted once per loaded document. A click on any `[data-act]`
 * element posts `{ type: <data-act>, value?: <data-value>, ...every other
 * data-* attribute of that element, camelCased }`; a disclosure toggle adds
 * `open`. So the act messages are open-ended by design, and a panel narrows
 * them by `type`.
 */
export type PageToHost =
  | { type: "ready" }
  | { type: "input"; field: string; value: string }
  | { type: string; id: string; checked: boolean; [data: string]: unknown }
  | { type: string; value?: string; open?: boolean; [data: string]: unknown };

/**
 * A message from the page, or undefined for anything else.
 *
 * The webview is a separate process and `onDidReceiveMessage` hands over
 * `unknown`; this is the one check a panel needs before switching on `type`.
 */
export function pageMessage(msg: unknown): PageToHost | undefined {
  if (typeof msg !== "object" || msg === null) return undefined;
  const type = (msg as { type?: unknown }).type;
  return typeof type === "string" && type !== "" ? (msg as PageToHost) : undefined;
}
