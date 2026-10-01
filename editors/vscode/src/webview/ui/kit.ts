// The MemQL page kit: every piece a webview page is built from, as pure string
// renderers.
//
// ONE VOCABULARY FOR EVERY PANEL. A page is three parts (docs/internal/design/
// 2026-09-28-vscode-extension-ux.md, "Page anatomy"): a head, a body of facts,
// groups, forms and notices, and an action bar on the floor. Each part has one
// renderer here, and the stylesheet that dresses them is kitStyles(), which
// brandStyleBlock() carries into every document. A panel composes these; it
// does not write its own button, fact row or notice.
//
// ESCAPING IS THE KIT'S JOB. Every text argument is escaped in here, so a
// caller hands over plain strings -- a cluster name, a step's reason, a line
// of stderr -- and cannot forget. An argument whose name ends in `Html` is
// already-rendered markup (usually the output of another renderer below) and
// is interpolated verbatim; that suffix is the whole of the contract, and it
// is the one place a caller takes on the obligation instead.
//
// RULES THAT ARE CHEAPER TO ENFORCE THAN TO REVIEW. `actionBar` throws on a
// fourth act or a second button, so every screen test that renders a bar
// catches the violation, rather than a reviewer counting buttons. The bar also
// puts the one button last whatever order it was given in.
//
// THE PAGE SCRIPT READS WHAT THIS WRITES. `data-act`, `data-value`,
// `data-field`, `data-region`, `data-part`, `data-disclosure` and `role=switch`
// are the hooks runtime.ts acts on; the message shapes are in protocol.ts.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { brandMarkSvg } from "../brandTokens.js";
import type { LogLine, ProgressState } from "./protocol.js";

export { kitStyles } from "./kitStyles.js";

// -----------------------------------------------------------------------------
// Acts
// -----------------------------------------------------------------------------

/**
 * How an act is dressed. `primary` and `danger` are buttons and there is at
 * most one of them on a bar; `text` is a quiet clickable label; `secondary`
 * is an outlined button for places that are not a bar (a notice, an empty
 * state).
 */
export type Tone = "primary" | "danger" | "text" | "secondary";

/** One thing a person can do, posted to the host as `{ type: act, value, ...data }`. */
export interface Act {
  /** The message type the page posts. */
  act: string;
  /** The label, which is also the promise. */
  label: string;
  /** Posted as `value`, for acts that carry a choice. */
  value?: string;
  /** Defaults to `text`. */
  tone?: Tone;
  /** In flight: drawn with a spinner and ignored by the page script until it settles. */
  busy?: boolean;
  /** A tooltip, for a label that is ambiguous out of context. */
  title?: string;
  /**
   * The act's full name for assistive tech, when the visible label leans on
   * the row it sits in ("Promote" on a rollout's row is "Promote memql-bff").
   */
  ariaLabel?: string;
  /** Extra `data-*` attributes, keys without the `data-` prefix; posted camelCased. */
  data?: Readonly<Record<string, string>>;
}

/** A `data-*` name the kit will write: lower-case words joined by single hyphens. */
const DATA_KEY = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

/**
 * `data-*` attributes from a caller's map.
 *
 * THE NAME IS VALIDATED, NOT ESCAPED, because an attribute NAME cannot be
 * escaped: a key carrying a quote or a space would end the attribute and start
 * markup. A bad key is a programming error, so it throws, and the test that
 * renders the screen finds it.
 */
function dataAttrs(data: Readonly<Record<string, string>> | undefined, reserved: readonly string[] = []): string {
  if (data === undefined) return "";
  return Object.entries(data)
    .map(([key, value]) => {
      if (!DATA_KEY.test(key)) throw new Error(`kit: "${key}" is not a usable data-* attribute name`);
      if (reserved.includes(key)) throw new Error(`kit: data-${key} is written by the kit itself`);
      return ` data-${key}="${escapeHtml(value)}"`;
    })
    .join("");
}

/** A button or a text act, carrying `data-act` (and `data-value`) for the page script. */
export function button(a: Act): string {
  const tone = a.tone ?? "text";
  const isText = tone === "text";
  const cls = isText ? "mq-textbtn" : "mq-btn";
  const toneAttr = isText ? "" : ` data-tone="${tone}"`;
  const value = a.value === undefined ? "" : ` data-value="${escapeHtml(a.value)}"`;
  const title = a.title === undefined ? "" : ` title="${escapeHtml(a.title)}"`;
  const aria = a.ariaLabel === undefined || a.ariaLabel === "" ? "" : ` aria-label="${escapeHtml(a.ariaLabel)}"`;
  const busy = a.busy === true ? ` aria-busy="true" aria-disabled="true"` : "";
  const spinner = a.busy === true ? `<span class="mq-spin" aria-hidden="true"></span>` : "";
  return (
    `<button type="button" class="${cls}"${toneAttr} data-act="${escapeHtml(a.act)}"${value}` +
    `${dataAttrs(a.data, ["act", "value", "tone"])}${title}${aria}${busy}>${spinner}${escapeHtml(a.label)}</button>`
  );
}

/** Whether an act is drawn as THE button of a bar. */
function isStrong(a: Act): boolean {
  return a.tone === "primary" || a.tone === "danger";
}

// -----------------------------------------------------------------------------
// Icons: inline SVG, because the CSP loads no images and no icon font
// -----------------------------------------------------------------------------

const SVG_OPEN = `<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" focusable="false"`;
const STROKE = `fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"`;

const CHEVRON_RIGHT = `<svg class="mq-chevron" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" focusable="false"><path d="M6 3.5 10.5 8 6 12.5" ${STROKE}/></svg>`;
const CHEVRON_LEFT = `<svg class="mq-chevron" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true" focusable="false"><path d="M10 3.5 5.5 8 10 12.5" ${STROKE}/></svg>`;

const NOTICE_ICONS: Readonly<Record<"info" | "warn" | "error", string>> = {
  info: `${SVG_OPEN} class="mq-notice-icon"><circle cx="8" cy="8" r="6.25" ${STROKE}/><path d="M8 7.25v4" ${STROKE}/><circle cx="8" cy="4.9" r="0.95" fill="currentColor"/></svg>`,
  warn: `${SVG_OPEN} class="mq-notice-icon"><path d="M8 2.25 14.5 13.5h-13z" ${STROKE}/><path d="M8 6.5v3.25" ${STROKE}/><circle cx="8" cy="11.6" r="0.95" fill="currentColor"/></svg>`,
  error: `${SVG_OPEN} class="mq-notice-icon"><circle cx="8" cy="8" r="6.25" ${STROKE}/><path d="M5.9 5.9l4.2 4.2M10.1 5.9l-4.2 4.2" ${STROKE}/></svg>`,
};

// -----------------------------------------------------------------------------
// Page anatomy: head, body, action bar
// -----------------------------------------------------------------------------

export interface HeadInput {
  title: string;
  /** A quiet figure beside the title: a version, a count. */
  meta?: string;
  /** A drill-down's way back, drawn above the title. */
  back?: { act: string; label: string; value?: string };
  /**
   * Record-management acts that do not change the thing's state (Edit,
   * Remove from list), as quiet text acts at the trailing edge. Anything that
   * changes state belongs on the action bar instead.
   */
  asideActs?: readonly Act[];
}

/** The page's head: at most a way back, then the title and its meta. */
export function head(i: HeadInput): string {
  const back =
    i.back === undefined
      ? ""
      : `<div class="mq-head-back"><button type="button" class="mq-back" data-act="${escapeHtml(i.back.act)}"` +
        `${i.back.value === undefined ? "" : ` data-value="${escapeHtml(i.back.value)}"`}>` +
        `${CHEVRON_LEFT}${escapeHtml(i.back.label)}</button></div>`;
  const meta = i.meta === undefined || i.meta === "" ? "" : `<span class="mq-head-meta">${escapeHtml(i.meta)}</span>`;
  const aside =
    i.asideActs === undefined || i.asideActs.length === 0
      ? ""
      : `<div class="mq-head-aside">${i.asideActs.map((a) => button({ ...a, tone: "text" })).join("")}</div>`;
  return `<header class="mq-head">${back}<div class="mq-head-row"><h1 class="mq-title">${escapeHtml(
    i.title,
  )}</h1>${meta}${aside}</div></header>`;
}

/** A group's heading. The subhead is the container language; there are no boxes. */
export function subhead(title: string, meta?: string): string {
  const m = meta === undefined || meta === "" ? "" : `<span class="mq-subhead-meta">${escapeHtml(meta)}</span>`;
  return `<h2 class="mq-subhead">${escapeHtml(title)}${m}</h2>`;
}

export interface FactRow {
  label: string;
  /** Plain text. An absent or empty value renders as a quiet dash: unavailable is not zero. */
  value?: string;
  /** Pre-rendered value markup; wins over `value`. */
  valueHtml?: string;
  /** The editor's monospace, for addresses, ids and versions. */
  mono?: boolean;
  /** A value that is itself a state of absence ("Not signed in"). */
  muted?: boolean;
}

/** Label/value pairs, as a description list. */
export function facts(rows: readonly FactRow[]): string {
  const body = rows
    .map((row) => {
      const value =
        row.valueHtml !== undefined
          ? row.valueHtml
          : row.value !== undefined && row.value !== ""
            ? escapeHtml(row.value)
            : `<span class="mq-none">—</span>`;
      const cls = row.mono === true ? ` class="mq-mono"` : "";
      const muted = row.muted === true ? ` data-muted="true"` : "";
      return `<dt>${escapeHtml(row.label)}</dt><dd${cls}${muted}>${value}</dd>`;
    })
    .join("");
  return `<dl class="mq-facts">${body}</dl>`;
}

/** The dot beside the state word. */
export type BarTone = "idle" | "live" | "busy" | "warn" | "error";

export interface ActionBarInput {
  /** The state in the words a person uses, sentence case, a few words. */
  state: string;
  /** What that state means, in one clause. */
  detail?: string;
  tone?: BarTone;
  /** The acts legal from this state: at most three, at most one button. */
  acts: readonly Act[];
  /** The sentence a destructive act asks the person to read first, above the acts. */
  confirm?: string;
}

/**
 * The floor of a page: the state in words, then the acts legal from it.
 *
 * THROWS on more than three acts or more than one `primary`/`danger` act. An
 * illegal act is ABSENT, never disabled -- the caller computes `acts` from the
 * state -- and the one button always renders last, where the eye lands. Every
 * other act is drawn as a text act whatever tone it was given, so the bar
 * holds one button however it was asked.
 */
export function actionBar(i: ActionBarInput): string {
  if (i.acts.length > 3) {
    throw new Error(`actionBar: at most three acts, got ${i.acts.length} (${i.acts.map((a) => a.label).join(", ")})`);
  }
  const strong = i.acts.filter(isStrong);
  if (strong.length > 1) {
    throw new Error(`actionBar: at most one button, got ${strong.length} (${strong.map((a) => a.label).join(", ")})`);
  }
  const ordered = [...i.acts.filter((a) => !isStrong(a)).map((a) => ({ ...a, tone: "text" as const })), ...strong];
  const tone = i.tone ?? "idle";
  const confirm = i.confirm === undefined || i.confirm === "" ? "" : `<p class="mq-actbar-confirm">${escapeHtml(i.confirm)}</p>`;
  const detail = i.detail === undefined || i.detail === "" ? "" : `<span class="mq-actbar-detail">${escapeHtml(i.detail)}</span>`;
  const acts = ordered.length === 0 ? "" : `<div class="mq-actbar-acts">${ordered.map(button).join("")}</div>`;
  return (
    `<div class="mq-actbar" data-tone="${tone}" role="group" aria-label="Actions">${confirm}` +
    `<div class="mq-actbar-state"><span class="mq-dot" data-tone="${tone}" aria-hidden="true"></span>` +
    `<span class="mq-actbar-word">${escapeHtml(i.state)}</span>${detail}</div>${acts}</div>`
  );
}

export interface PageInput {
  head: string;
  body: string;
  actionBar?: string;
}

/**
 * The three regions, in the shape the page script patches.
 *
 * Nothing between the tags: an empty action region must be `:empty` so the
 * sticky floor draws nothing at all on a page with no acts.
 */
export function page(i: PageInput): string {
  return (
    `<main class="mq-page"><div data-region="head">${i.head}</div>` +
    `<div data-region="body">${i.body}</div></main>` +
    `<footer data-region="actions">${i.actionBar ?? ""}</footer>`
  );
}

// -----------------------------------------------------------------------------
// Forms
// -----------------------------------------------------------------------------

export interface FieldInput {
  label: string;
  /** The control, pre-rendered (usually `textInput`). */
  controlHtml: string;
  hint?: string;
  error?: string;
  /**
   * The control's id. With it the label points at the control with `for`, and
   * the hint and error carry `<id>-hint` / `<id>-error` for the control's
   * `aria-describedby`. Without it the label wraps the control.
   */
  id?: string;
}

/** A labelled control: label above, control on the line, then the hint or the error. */
export function field(i: FieldInput): string {
  const idAttr = (suffix: string): string => (i.id === undefined ? "" : ` id="${escapeHtml(i.id)}-${suffix}"`);
  const hint = i.hint === undefined || i.hint === "" ? "" : `<p class="mq-field-hint"${idAttr("hint")}>${escapeHtml(i.hint)}</p>`;
  const error =
    i.error === undefined || i.error === ""
      ? ""
      : `<p class="mq-field-error" role="alert"${idAttr("error")}>${escapeHtml(i.error)}</p>`;
  const invalid = error === "" ? "" : ` data-invalid="true"`;
  if (i.id !== undefined) {
    return (
      `<div class="mq-field"${invalid}><label class="mq-field-label" for="${escapeHtml(i.id)}">${escapeHtml(i.label)}</label>` +
      `${i.controlHtml}${hint}${error}</div>`
    );
  }
  return (
    `<div class="mq-field"${invalid}><label class="mq-field-main"><span class="mq-field-label">${escapeHtml(i.label)}</span>` +
    `${i.controlHtml}</label>${hint}${error}</div>`
  );
}

const INPUT_TYPES = new Set(["text", "url", "email", "password"]);

export interface TextInputInput {
  /** Posted as `field` with every keystroke; the page never repaints for it. */
  field: string;
  value?: string;
  placeholder?: string;
  type?: "text" | "url" | "email" | "password";
  id?: string;
  invalid?: boolean;
  autofocus?: boolean;
  /** Ids of the hint/error that describe this control (see `field`). */
  describedBy?: string;
  /** An act posted as `{ type, field, value }` when Enter is pressed here. */
  enterAct?: string;
}

/** A one-line text control, `--memql-control-h` tall. */
export function textInput(i: TextInputInput): string {
  const type = i.type !== undefined && INPUT_TYPES.has(i.type) ? i.type : "text";
  const attrs = [
    `class="mq-input"`,
    `type="${type}"`,
    i.id === undefined ? "" : `id="${escapeHtml(i.id)}"`,
    `data-field="${escapeHtml(i.field)}"`,
    `value="${escapeHtml(i.value ?? "")}"`,
    i.placeholder === undefined ? "" : `placeholder="${escapeHtml(i.placeholder)}"`,
    i.invalid === true ? `aria-invalid="true"` : "",
    i.describedBy === undefined ? "" : `aria-describedby="${escapeHtml(i.describedBy)}"`,
    i.enterAct === undefined ? "" : `data-enter-act="${escapeHtml(i.enterAct)}"`,
    `spellcheck="false"`,
    `autocomplete="off"`,
    i.autofocus === true ? "autofocus" : "",
  ].filter((a) => a !== "");
  return `<input ${attrs.join(" ")}>`;
}

export interface SwitchRowInput {
  id: string;
  label: string;
  /** One quiet line under the label: what turning it on does. */
  note?: string;
  checked: boolean;
  /** `danger` for a switch whose "on" destroys something. */
  tone?: "danger";
  disabled?: boolean;
  /**
   * Extra `data-*` attributes on the input. `switch-act` names the message
   * type the page posts on change (default `switch`); `field` turns it into
   * an ordinary field that posts `input` with "true"/"false" instead.
   */
  data?: Readonly<Record<string, string>>;
}

/**
 * An on/off choice: a native checkbox with `role="switch"` inside its label.
 *
 * NATIVE, so Space, a click on the words and assistive tech all work with no
 * script, and the page script only has to read `.checked`. The track is drawn
 * in CSS and the THUMB POSITION carries the state, not the colour alone. The
 * accessible name is the label only (`aria-labelledby`); the note is the
 * description, rather than half of the name.
 */
export function switchRow(i: SwitchRowInput): string {
  const id = escapeHtml(i.id);
  const note = i.note === undefined || i.note === "" ? "" : `<span class="mq-switch-note" id="${id}-note">${escapeHtml(i.note)}</span>`;
  const describedBy = note === "" ? "" : ` aria-describedby="${id}-note"`;
  const tone = i.tone === "danger" ? ` data-tone="danger"` : "";
  return (
    `<label class="mq-switch" for="${id}"${tone}>` +
    `<input class="mq-switch-input" type="checkbox" role="switch" id="${id}"${i.checked ? " checked" : ""}` +
    `${i.disabled === true ? " disabled" : ""} aria-labelledby="${id}-label"${describedBy}${dataAttrs(i.data)}>` +
    `<span class="mq-switch-track" aria-hidden="true"></span>` +
    `<span class="mq-switch-text"><span class="mq-switch-label" id="${id}-label">${escapeHtml(i.label)}</span>${note}</span>` +
    `</label>`
  );
}

// -----------------------------------------------------------------------------
// Messages
// -----------------------------------------------------------------------------

export interface NoticeInput {
  tone: "info" | "warn" | "error";
  /** What happened, one sentence. */
  line: string;
  /** The fix, when it is not a button. */
  next?: string;
  /** A remedy command, pre-rendered (usually `codeBlock`). */
  codeHtml?: string;
  /** The fix as acts, when the extension can do it. */
  acts?: readonly Act[];
}

/** One sentence of what happened, then the way out. An error is announced (`role="alert"`). */
export function notice(i: NoticeInput): string {
  const role = i.tone === "error" ? ` role="alert"` : "";
  const next = i.next === undefined || i.next === "" ? "" : `<p class="mq-notice-next">${escapeHtml(i.next)}</p>`;
  const acts =
    i.acts === undefined || i.acts.length === 0 ? "" : `<div class="mq-notice-acts">${i.acts.map(button).join("")}</div>`;
  return (
    `<div class="mq-notice" data-tone="${i.tone}"${role}>${NOTICE_ICONS[i.tone]}<div class="mq-notice-body">` +
    `<p class="mq-notice-line">${escapeHtml(i.line)}</p>${next}${i.codeHtml ?? ""}${acts}</div></div>`
  );
}

/** A command on one line, in the editor's monospace, with an optional act beside it. */
export function codeBlock(i: { text: string; act?: Act }): string {
  const act = i.act === undefined ? "" : button(i.act);
  return `<div class="mq-code"><code class="mq-code-text">${escapeHtml(i.text)}</code>${act}</div>`;
}

export interface DisclosureInput {
  /** Posted on toggle, as `{ type: act, disclosure: id, open }`, so the host remembers. */
  act: string;
  label: string;
  /** The label while open; defaults to `label`. */
  openLabel?: string;
  open: boolean;
  /** A quiet figure beside the toggle ("214 lines"). */
  meta?: string;
  bodyHtml: string;
  /** The body's id, which the toggle controls. */
  id: string;
}

/**
 * A collapsed section behind a text toggle.
 *
 * THE BODY IS ALWAYS EMITTED, hidden when closed, so the page script can open
 * it at once on a click (and tell the host), and a live log inside keeps
 * receiving lines while it is closed.
 */
export function disclosure(i: DisclosureInput): string {
  const id = escapeHtml(i.id);
  const labels =
    i.openLabel === undefined || i.openLabel === i.label
      ? `<span>${escapeHtml(i.label)}</span>`
      : `<span data-when="closed">${escapeHtml(i.label)}</span><span data-when="open">${escapeHtml(i.openLabel)}</span>`;
  const meta = i.meta === undefined || i.meta === "" ? "" : `<span class="mq-disclosure-meta">${escapeHtml(i.meta)}</span>`;
  return (
    `<div class="mq-disclosure"><div class="mq-disclosure-head">` +
    `<button type="button" class="mq-disclosure-toggle" data-act="${escapeHtml(i.act)}" data-disclosure="${id}"` +
    ` aria-expanded="${i.open}" aria-controls="${id}">${CHEVRON_RIGHT}${labels}</button>${meta}</div>` +
    `<div class="mq-disclosure-body" id="${id}"${i.open ? "" : " hidden"}>${i.bodyHtml}</div></div>`
  );
}

/** One log line, as the page script also builds it. */
function logLine(line: LogLine): string {
  const tone =
    (line.tone === undefined ? "" : ` data-tone="${line.tone}"`) + (line.anchor === true ? ` data-anchor="true"` : "");
  const label = line.label === undefined || line.label === "" ? "" : `<span class="mq-log-label">${escapeHtml(line.label)}</span>`;
  return `<div class="mq-log-line"${tone}>${label}<span class="mq-log-text">${escapeHtml(line.text)}</span></div>`;
}

export interface LogPaneInput {
  id: string;
  /** Static lines. A pane fed by `LiveView.log` is rendered with none, and keeps what it streamed. */
  lines: readonly LogLine[];
  ariaLabel: string;
  /** Acts on the pane's header: Copy, Open in Output. */
  acts?: readonly Act[];
  /** What an empty pane says. */
  empty?: string;
}

/**
 * A scrolling, chronological log that follows its tail until the person
 * scrolls up (`data-follow`, maintained by the page script).
 *
 * `role="log"` for what it is, `aria-live="off"` because an install writes
 * hundreds of lines and a screen reader should not read them unasked; the
 * progress status line is the announced summary.
 */
export function logPane(i: LogPaneInput): string {
  const acts =
    i.acts === undefined || i.acts.length === 0
      ? ""
      : `<div class="mq-log-head">${i.acts.map((a) => button({ ...a, tone: "text" })).join("")}</div>`;
  const empty = i.empty === undefined ? "" : ` data-empty="${escapeHtml(i.empty)}"`;
  return (
    `<div class="mq-logbox">${acts}<div class="mq-log" id="${escapeHtml(i.id)}" data-region="log" data-follow="true"` +
    ` role="log" aria-live="off" aria-label="${escapeHtml(i.ariaLabel)}" tabindex="0"${empty}>` +
    `${i.lines.map(logLine).join("")}</div></div>`
  );
}

// -----------------------------------------------------------------------------
// Progress
// -----------------------------------------------------------------------------

export interface ProgressInput {
  /** The act's own words: "Installing MemQL". */
  title: string;
  /** 0 to 100; absent draws an indeterminate bar. */
  percent?: number;
  /** One status line, six words at most. */
  status: string;
  /** "Step 6 of 16"; the elapsed clock follows it. */
  stepText?: string;
  /** Epoch ms; the page script ticks the clock from it while running. */
  startedAt?: number;
  /** Epoch ms the run settled; freezes the clock. */
  endedAt?: number;
  state: ProgressState;
  /** The mark's size in px (default 40). */
  markSize?: number;
  /** Distinguishes two progress regions on one page (default `mq-progress`). */
  id?: string;
  /** The render's clock, for a deterministic elapsed value in tests and the gallery. */
  now?: number;
}

/** A whole number from 0 to 100, or undefined when there is no honest one. */
export function clampPercent(percent: number | undefined): number | undefined {
  if (percent === undefined || !Number.isFinite(percent)) return undefined;
  return Math.min(100, Math.max(0, Math.round(percent)));
}

/** Elapsed time as m:ss, or h:mm:ss past the hour. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = String(total % 60).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${s}` : `${m}:${s}`;
}

/**
 * The one progress screen: the mark, the title, the bar, the status line and
 * "Step 6 of 16 · 3:12".
 *
 * The bar's width is `data-percent` (no inline style survives this CSP), and
 * every part the page script updates in place carries a `data-part` hook, so a
 * `progress` message moves the bar -- which then animates -- without repainting
 * anything around it.
 *
 * The title is the page's h1, so a progress screen renders no `head()` above
 * it; the reason, the "Show logs" disclosure and the log that follow it sit on
 * the same centred column.
 */
export function progress(i: ProgressInput): string {
  const id = escapeHtml(i.id ?? "mq-progress");
  const percent = clampPercent(i.percent);
  const indeterminate = percent === undefined;
  const valueNow = indeterminate ? "" : ` aria-valuenow="${percent}"`;
  const valueText = escapeHtml(indeterminate ? i.status : `${percent}%, ${i.status}`);
  const fillPercent = indeterminate ? "" : ` data-percent="${percent}"`;
  const indeterminateAttr = indeterminate ? ` data-indeterminate="true"` : "";
  let elapsed = "";
  if (i.startedAt !== undefined && Number.isFinite(i.startedAt)) {
    const end = i.state !== "running" && i.endedAt !== undefined ? i.endedAt : (i.now ?? Date.now());
    elapsed = formatElapsed(end - i.startedAt);
  }
  const stepText = i.stepText ?? "";
  const sepHidden = stepText !== "" && elapsed !== "" ? "" : " hidden";
  const started = i.startedAt === undefined ? "" : ` data-started-at="${Math.round(i.startedAt)}"`;
  const ended = i.endedAt === undefined ? "" : ` data-ended-at="${Math.round(i.endedAt)}"`;
  return (
    `<section class="mq-progress" data-region="progress" data-state="${i.state}"${started}${ended}` +
    ` data-step-text="${escapeHtml(stepText)}" aria-labelledby="${id}-title">` +
    `<div class="mq-progress-mark" data-part="mark">${brandMarkSvg(i.markSize ?? 40)}</div>` +
    `<h1 class="mq-progress-title" id="${id}-title" data-part="title">${escapeHtml(i.title)}</h1>` +
    `<div class="mq-bar" data-part="bar" data-state="${i.state}" role="progressbar" aria-labelledby="${id}-title"` +
    ` aria-valuemin="0" aria-valuemax="100"${valueNow} aria-valuetext="${valueText}"${indeterminateAttr}>` +
    `<div class="mq-bar-fill" data-part="fill"${fillPercent}></div></div>` +
    `<p class="mq-progress-status" data-part="status" role="status" aria-live="polite">${escapeHtml(i.status)}</p>` +
    `<p class="mq-progress-meta" data-part="meta"><span data-part="step">${escapeHtml(stepText)}</span>` +
    `<span data-part="sep" aria-hidden="true"${sepHidden}> · </span><span data-part="elapsed">${elapsed}</span></p>` +
    `</section>`
  );
}

// -----------------------------------------------------------------------------
// Loading and empty
// -----------------------------------------------------------------------------

/** Widths that read as text of differing lengths, cycled so no two neighbours match. */
const WIDTHS = ["l", "m", "xl", "s", "l", "m"] as const;

function bar(w: string, extra = ""): string {
  return `<span class="mq-skel" data-w="${w}"${extra}></span>`;
}

function skeletonFacts(rows: number): string {
  let out = "";
  for (let r = 0; r < rows; r += 1) out += `${bar(r % 2 === 0 ? "s" : "xs")}${bar(WIDTHS[r % WIDTHS.length])}`;
  return `<div class="mq-skel-facts">${out}</div>`;
}

function skeletonList(rows: number): string {
  let out = "";
  for (let r = 0; r < rows; r += 1) out += `<div>${bar(WIDTHS[r % WIDTHS.length])}${bar(r % 2 === 0 ? "s" : "xs")}</div>`;
  return `<div class="mq-skel-list">${out}</div>`;
}

function skeletonForm(rows: number): string {
  let out = "";
  for (let r = 0; r < rows; r += 1) out += `<div>${bar(r % 2 === 0 ? "s" : "xs")}${bar("full", ` data-h="control"`)}</div>`;
  return `<div class="mq-skel-form">${out}</div>`;
}

export interface SkeletonInput {
  /** The geometry of what is loading. */
  shape: "facts" | "list" | "form" | "page";
  rows?: number;
  /** What is loading, for screen readers only ("Loading cluster details"). */
  label: string;
}

/**
 * Loading, drawn as the shape of the content it stands in for.
 *
 * The words are for screen readers only (`role="status"`, `aria-busy`); a
 * sighted reader sees quiet shapes where the content will land, and no
 * "Loading" caption. Never used for a read that failed.
 */
export function skeleton(i: SkeletonInput): string {
  const rows = Math.max(1, Math.min(12, Math.round(i.rows ?? (i.shape === "form" ? 3 : 4))));
  let shape: string;
  switch (i.shape) {
    case "facts":
      shape = skeletonFacts(rows);
      break;
    case "list":
      shape = skeletonList(rows);
      break;
    case "form":
      shape = skeletonForm(rows);
      break;
    case "page":
      shape =
        `<div class="mq-skel-head">${bar("xl", ` data-h="title"`)}${bar("s")}</div>` +
        skeletonFacts(Math.min(rows, 4)) +
        `<div class="mq-skel-sub">${bar("m")}</div>` +
        skeletonList(Math.max(2, rows - 1));
      break;
  }
  return (
    `<div class="mq-skeleton" data-shape="${i.shape}" role="status" aria-busy="true">` +
    `<span class="mq-sr">${escapeHtml(i.label)}</span><div aria-hidden="true">${shape}</div></div>`
  );
}

/** A real empty state, after a read that succeeded and found nothing. */
export function emptyState(i: { line: string; acts?: readonly Act[] }): string {
  const acts = i.acts === undefined || i.acts.length === 0 ? "" : `<div class="mq-acts">${i.acts.map(button).join("")}</div>`;
  return `<div class="mq-empty"><p class="mq-empty-line">${escapeHtml(i.line)}</p>${acts}</div>`;
}
