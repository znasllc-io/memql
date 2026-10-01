// The Add a cluster panel's screens, as kit pages.
//
// ONE MODULE FOR EVERY SCREEN THE PANEL SHOWS -- the landing, the install and
// repair form, the connect form, the run, the done screen, the uninstall
// preview -- each a pure function from a small view model to the three page
// regions LiveView assigns and patches (head, body, action bar). The panel
// (addClusterPanel.ts) builds the view models from its state machines; the
// gallery (gallery/scenarios/install.ts) builds them by hand; the tests call
// both. Nothing here decides what is legal: every act a screen draws was
// already decided by its input, and an act that is not legal is simply not
// in it.
//
// BUILT FROM THE KIT (src/webview/ui/kit.ts) and nothing else, apart from the
// two shapes the kit does not have -- a list of choices, and a list of what
// an uninstall removes -- whose CSS is `ADD_CLUSTER_STYLES` below: layout and
// token colours only, no hex.
//
// THE WORDS LIVE HERE, where they can be read as a set. The design record
// (docs/internal/design/2026-09-28-vscode-extension-ux.md) is the voice: short,
// plain, one statement of each fact, errors as a sentence and the fix.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import type { RecoveryKeyState } from "../install/recoveryKey.js";
import type { RemovalRow, SharedToolRow } from "../install/removalPreview.js";
import { MAIN_BRANCH_CHOICE, isMainBranchChoice } from "../install/stackPin.js";
import type {
  ConnectFieldError,
  ConnectInputs,
  ConnectProbeState,
  FieldError,
  InputField,
  Inputs,
  LandingView,
} from "../state/addCluster.js";
import type { PreflightCheck } from "../state/preflight.js";
import { compareSemverDesc } from "../install/tags.js";
import {
  actionBar,
  button,
  codeBlock,
  disclosure,
  facts,
  field,
  head,
  logPane,
  notice,
  progress,
  skeleton,
  subhead,
  switchRow,
  textInput,
  type Act,
  type FactRow,
} from "./ui/kit.js";
import type { RegionParts } from "./ui/liveView.js";
import type { LogLine, ProgressState, ProgressUpdate } from "./ui/protocol.js";

// -----------------------------------------------------------------------------
// the flows, and the tab title each one wears
// -----------------------------------------------------------------------------

/** What the panel is doing, which is what its tab is called. */
export type PanelFlow = "add" | "install" | "repair" | "uninstall";

/**
 * The tab title follows the act: a person who opened "Uninstall" from a menu
 * should see a tab that says so, not "Add a cluster".
 */
export const TAB_TITLES: Readonly<Record<PanelFlow, string>> = {
  add: "Add a cluster",
  install: "Install MemQL",
  repair: "Repair MemQL",
  uninstall: "Uninstall MemQL",
};

/** The run's own words, as a heading, a state and a failure. */
const RUN_WORDS: Readonly<
  Record<"install" | "repair" | "uninstall", { title: string; busy: string; failed: string; done: string }>
> = {
  install: { title: "Installing MemQL", busy: "Installing", failed: "Couldn't install", done: "MemQL is installed" },
  repair: { title: "Repairing MemQL", busy: "Repairing", failed: "Couldn't repair", done: "MemQL is repaired" },
  uninstall: {
    title: "Uninstalling MemQL",
    busy: "Uninstalling",
    failed: "Couldn't uninstall",
    done: "MemQL is uninstalled",
  },
};

// -----------------------------------------------------------------------------
// the panel's own shapes: a list of choices, a list of what goes
// -----------------------------------------------------------------------------

/** The CSS for the two shapes the kit does not draw. Tokens only. */
export const ADD_CLUSTER_STYLES = `
  .ac-line { margin: 0 0 16px; max-width: 60ch; }
  .ac-narrow { max-width: 40rem; }
  .ac-choices { list-style: none; margin: 0; padding: 0; max-width: 40rem;
                border-top: 1px solid var(--memql-border); }
  .ac-choices > li { border-bottom: 1px solid var(--memql-border); }
  .ac-choice { display: flex; align-items: center; gap: 12px; box-sizing: border-box; width: 100%;
               margin: 0; padding: 10px 8px 10px 2px; font: inherit; line-height: 1.4; text-align: left;
               color: var(--memql-fg); background: none; border: 0; border-radius: var(--memql-radius);
               cursor: pointer; transition: background-color var(--memql-motion-dur) var(--memql-motion-ease); }
  .ac-choice:hover { background: var(--memql-raised); }
  .ac-choice:focus-visible { outline: 1px solid var(--memql-focus); outline-offset: -1px; }
  .ac-choice-text { display: flex; flex-direction: column; gap: 1px; flex: 1 1 auto; min-width: 0; }
  .ac-choice-label { font-weight: 500; }
  .ac-choice[data-tone="danger"] .ac-choice-label { color: var(--memql-danger); }
  .ac-choice-note { color: var(--memql-muted); font-size: 0.923em; }
  .ac-chevron { flex: none; color: var(--memql-subtle); }
  .ac-list { list-style: none; margin: 0; padding: 0; max-width: 40rem;
             border-top: 1px solid var(--memql-border); }
  .ac-row { display: flex; flex-wrap: wrap; align-items: baseline; column-gap: 10px; row-gap: 2px;
            padding: 8px 2px; border-bottom: 1px solid var(--memql-border); }
  .ac-row-name { font-weight: 500; }
  .ac-row-detail { flex: 1 1 12em; min-width: 0; color: var(--memql-muted); overflow-wrap: anywhere; }
  .ac-row-tag { margin-left: auto; color: var(--memql-muted); font-size: 0.923em; white-space: nowrap; }
  .ac-empty { margin: 0; color: var(--memql-muted); }
  .ac-pair { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); column-gap: 12px;
             max-width: 34rem; }
  .ac-word[data-tone="attention"] { color: var(--memql-warn); font-weight: 500; }
  .ac-word[data-tone="error"] { color: var(--memql-danger); font-weight: 500; }
  .ac-word-note { display: block; margin-top: 1px; color: var(--memql-muted); font-size: 0.923em; }
  .ac-column { box-sizing: border-box; max-width: 560px; margin: 8px auto 0; }
  .ac-column > .mq-subhead { margin-top: 24px; }
  .ac-quiet { margin: 10px 0 0; color: var(--memql-muted); }
  .ac-key .mq-code-text { letter-spacing: 0.04em; }
  select.mq-input { padding-right: 4px; }
  @media (max-width: 480px) {
    .ac-pair { grid-template-columns: minmax(0, 1fr); }
    .ac-row-tag { margin-left: 0; }
  }
`;

const CHEVRON = `<svg class="ac-chevron" viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false"><path d="M6 3.5 10.5 8 6 12.5" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"/></svg>`;

/** One choice on the landing: a line and a quiet note, the whole row clickable. */
interface ChoiceRow {
  act: string;
  value: string;
  label: string;
  note: string;
  tone?: "danger";
}

function choiceList(rows: readonly ChoiceRow[]): string {
  const items = rows
    .map((row) => {
      const tone = row.tone === undefined ? "" : ` data-tone="${row.tone}"`;
      return (
        `<li><button type="button" class="ac-choice" data-act="${escapeHtml(row.act)}" data-value="${escapeHtml(row.value)}"${tone}>` +
        `<span class="ac-choice-text"><span class="ac-choice-label">${escapeHtml(row.label)}</span>` +
        `<span class="ac-choice-note">${escapeHtml(row.note)}</span></span>${CHEVRON}</button></li>`
      );
    })
    .join("");
  return `<ul class="ac-choices" role="list">${items}</ul>`;
}

function rowList(rows: readonly RemovalRow[]): string {
  const items = rows
    .map((row) => {
      const detail = row.kept ? row.reason : row.detail;
      const tag =
        row.asks === "password" ? "Asks for your password" : row.asks === "approval" ? "Asks for approval" : "";
      return (
        `<li class="ac-row"><span class="ac-row-name">${escapeHtml(row.name)}</span>` +
        (detail === "" ? "" : `<span class="ac-row-detail">${escapeHtml(detail)}</span>`) +
        (tag === "" ? "" : `<span class="ac-row-tag">${escapeHtml(tag)}</span>`) +
        `</li>`
      );
    })
    .join("");
  return `<ul class="ac-list" role="list">${items}</ul>`;
}

const EMPTY_ACTIONS = "";

// -----------------------------------------------------------------------------
// the landing
// -----------------------------------------------------------------------------

export interface LandingInput {
  /** Undefined while detection is running: the page draws the shape of the list. */
  view?: LandingView;
}

/**
 * What this computer has, and the choices that apply to it.
 *
 * LOADING IS THE SHAPE OF THE LIST. Until detection settles there is no
 * verdict, so there is no state word and no choice -- only the skeleton of the
 * rows that are coming.
 */
export function landingScreen(input: LandingInput): RegionParts {
  const top = head({ title: TAB_TITLES.add });
  const view = input.view;
  if (view === undefined) {
    return {
      head: top,
      body: `<div class="ac-narrow">${skeleton({ shape: "list", rows: 2, label: "Checking this computer" })}</div>`,
      actions: EMPTY_ACTIONS,
    };
  }
  const line = view.line === undefined ? "" : `<p class="ac-line">${escapeHtml(view.line)}</p>`;
  return {
    head: top,
    body:
      line +
      choiceList(
        view.choices.map((choice) => ({
          act: "choose",
          value: choice.act,
          label: choice.label,
          note: choice.note,
          ...(choice.tone === undefined ? {} : { tone: choice.tone }),
        })),
      ),
    actions: actionBar({ state: view.state, tone: view.tone, acts: [] }),
  };
}

// -----------------------------------------------------------------------------
// install and repair: the form
// -----------------------------------------------------------------------------

export interface CollectInput {
  action: "install" | "repair";
  values: Inputs;
  errors: readonly FieldError[];
  /** Release tags, newest first; empty draws a text box for the version. */
  versionChoices: readonly string[];
  /** The checks, or undefined while they are gathered. */
  checks?: readonly PreflightCheck[];
  /** Whether "More options" is open, as the page last said. */
  moreOpen: boolean;
  /** Why this window cannot install at all (a remote window). */
  remoteProblem?: string;
  /** The password was refused three times, or the prompt could not be answered. */
  passwordProblem?: string;
}

/** The fields each action asks for, and which of them sit behind "More options". */
const VISIBLE_FIELDS: Readonly<Record<"install" | "repair", readonly InputField[]>> = {
  install: ["ownerFirstName", "ownerLastName", "ownerEmail"],
  repair: [],
};
const MORE_FIELDS: Readonly<Record<"install" | "repair", readonly InputField[]>> = {
  install: ["domain", "version"],
  repair: ["ownerFirstName", "ownerLastName", "ownerEmail", "domain"],
};

const FIELD_LABEL: Readonly<Record<InputField, string>> = {
  domain: "Domain",
  ownerFirstName: "First name",
  ownerLastName: "Last name",
  ownerEmail: "Email",
  version: "Version",
};

/** The install form's fields, in the order they are asked for. The panel checks a posted field name against this. */
export const INPUT_FIELDS: readonly InputField[] = [
  "domain",
  "ownerFirstName",
  "ownerLastName",
  "ownerEmail",
  "version",
];

/** What the from-source lane is called in the picker: the label carries its cost. */
export const MAIN_CHOICE_LABEL = "Build from source (main, slower)";

/** The label the newest listed release carries: the value is the real tag, never a sentinel (memql#4429). */
export function latestLabel(tag: string): string {
  return `Latest (${tag})`;
}

/**
 * The version options, with the current value guaranteed present and IN
 * ORDER (memql#4429): a `<select>` silently drops a value that is not one of
 * its options, and hoisting it to the top would make a newest-first list look
 * mis-sorted.
 */
export function withCurrentInSortedPosition(choices: readonly string[], current: string): readonly string[] {
  const trimmed = current.trim();
  if (trimmed === "" || choices.includes(trimmed)) return choices;
  return [...choices, trimmed].sort(compareSemverDesc);
}

/** One entry in the version picker: what gets submitted, and what is read. */
export interface VersionChoice {
  value: string;
  label: string;
}

/**
 * The version picker's entries: newest first, Latest labelled, `main` last
 * (memql#3901, #4430).
 *
 * THE LATEST LABEL ATTACHES TO THE LISTING'S OWN NEWEST, not to whatever
 * sorts first: a current value the listing does not carry can sort above
 * everything listed, and calling that "Latest" would be a recommendation the
 * listing does not support. `main` is a lane, not a version, so it goes last
 * and its label says so.
 */
export function versionChoiceList(choices: readonly string[], current: string): readonly VersionChoice[] {
  const listed = choices.filter((c) => c !== MAIN_BRANCH_CHOICE);
  const newest = listed[0] ?? "";
  const releases = withCurrentInSortedPosition(listed, isMainBranchChoice(current) ? "" : current);
  return [
    ...releases.map((value) => ({ value, label: value === newest && newest !== "" ? latestLabel(value) : value })),
    { value: MAIN_BRANCH_CHOICE, label: MAIN_CHOICE_LABEL },
  ];
}

/** One version's words, for the "More options" summary. */
function versionLabel(value: string, newest: string): string {
  if (isMainBranchChoice(value)) return MAIN_CHOICE_LABEL;
  return value === newest && newest !== "" ? latestLabel(value) : value;
}

function fieldFor(input: CollectInput, name: InputField): string {
  const id = `f-${name}`;
  const error = input.errors.find((e) => e.field === name)?.message;
  const value = input.values[name];
  let control: string;
  if (name === "version" && input.action === "install" && input.versionChoices.length > 0) {
    // Offered only when the listing arrived: offline, the field is a text
    // box, and a text box accepting "main" would be an unlabelled branch
    // clone-stack.sh refuses (memql#3901).
    const options = versionChoiceList(input.versionChoices, value)
      .map(
        (choice) =>
          `<option value="${escapeHtml(choice.value)}"${choice.value === value ? " selected" : ""}>${escapeHtml(
            choice.label,
          )}</option>`,
      )
      .join("");
    control = `<select class="mq-input" id="${id}" data-field="version"${error === undefined ? "" : ` aria-invalid="true" aria-describedby="${id}-error"`}>${options}</select>`;
  } else {
    control = textInput({
      id,
      field: name,
      value,
      type: name === "ownerEmail" ? "email" : "text",
      ...(name === "domain" ? { placeholder: "memql.localhost" } : {}),
      ...(error === undefined ? {} : { invalid: true, describedBy: `${id}-error` }),
      enterAct: "begin",
    });
  }
  return field({ id, label: FIELD_LABEL[name], controlHtml: control, ...(error === undefined ? {} : { error }) });
}

/** First and last name side by side, anything else one to a line. */
function fieldGroup(input: CollectInput, names: readonly InputField[]): string {
  let out = "";
  for (let i = 0; i < names.length; i += 1) {
    const name = names[i]!;
    if (name === "ownerFirstName" && names[i + 1] === "ownerLastName") {
      out += `<div class="ac-pair">${fieldFor(input, name)}${fieldFor(input, "ownerLastName")}</div>`;
      i += 1;
      continue;
    }
    out += fieldFor(input, name);
  }
  return out;
}

/** "Ada Lovelace · memql.localhost · Latest (v0.20.3)": what "More options" is holding. */
function moreSummary(input: CollectInput): string {
  const v = input.values;
  const parts: string[] = [];
  if (input.action === "repair") {
    const name = `${v.ownerFirstName} ${v.ownerLastName}`.trim();
    if (name !== "") parts.push(name);
  }
  if (v.domain.trim() !== "") parts.push(v.domain.trim());
  if (input.action === "install" && v.version.trim() !== "") {
    const newest = input.versionChoices.filter((c) => c !== MAIN_BRANCH_CHOICE)[0] ?? "";
    parts.push(versionLabel(v.version.trim(), newest));
  }
  return parts.join(" · ");
}

function checksList(checks: readonly PreflightCheck[] | undefined): string {
  if (checks === undefined) return skeleton({ shape: "facts", rows: 2, label: "Checking this computer" });
  const rows: FactRow[] = checks.map((check) => ({
    label: check.label,
    valueHtml:
      `<span class="ac-word" data-tone="${check.tone}">${escapeHtml(check.word)}</span>` +
      (check.note === undefined || check.note === "" ? "" : `<span class="ac-word-note">${escapeHtml(check.note)}</span>`),
  }));
  return facts(rows);
}

/**
 * The install or repair form: only what the person must decide is in view.
 *
 * AN INSTALL asks for the owner -- a person's name and email are facts about
 * them that nothing here can guess -- and keeps the domain and the version,
 * which have good answers already, behind "More options".
 *
 * A REPAIR asks for nothing it already knows: the owner and domain come off
 * the install's record and sit behind "More options", open only when one of
 * them is missing or wrong -- an error in a closed disclosure is an error
 * nobody can see.
 */
export function collectScreen(input: CollectInput): RegionParts {
  const flow = input.action;
  // A repair whose record is missing an answer asks for it in plain view: the
  // person must decide it, so it is not an option.
  const unanswered = flow === "repair" && MORE_FIELDS.repair.some((name) => input.values[name].trim() === "");
  const visible = unanswered ? [...VISIBLE_FIELDS[flow], ...MORE_FIELDS[flow]] : VISIBLE_FIELDS[flow];
  const more = unanswered ? [] : MORE_FIELDS[flow];
  const moreOpen = input.moreOpen || input.errors.some((e) => more.includes(e.field));
  const blocked = input.remoteProblem !== undefined || (input.checks ?? []).some((c) => c.tone === "error");

  const notices =
    (input.remoteProblem === undefined ? "" : notice({ tone: "error", line: input.remoteProblem })) +
    (input.passwordProblem === undefined ? "" : notice({ tone: "warn", line: input.passwordProblem }));

  const body =
    notices +
    fieldGroup(input, visible) +
    (more.length === 0
      ? ""
      : disclosure({
          act: "toggleMore",
          id: "more-options",
          label: "More options",
          open: moreOpen,
          // What it holds, while it is closed; once open the fields say it.
          ...(moreOpen ? {} : { meta: moreSummary(input) }),
          bodyHtml: fieldGroup(input, more),
        })) +
    subhead("Checks") +
    checksList(input.checks);

  const verb = flow === "install" ? "Install" : "Repair";
  const acts: Act[] = [{ act: "back", label: "Cancel" }];
  if (!blocked) acts.push({ act: "begin", label: verb, tone: "primary" });
  const problems = input.errors.length > 0;
  return {
    head: head({ title: TAB_TITLES[flow] }),
    body,
    actions: actionBar({
      state: blocked ? "Can't start" : problems ? "Check the details" : flow === "install" ? "Ready to install" : "Ready to repair",
      tone: blocked || problems ? "warn" : "idle",
      acts,
    }),
  };
}

// -----------------------------------------------------------------------------
// connect to a cluster elsewhere
// -----------------------------------------------------------------------------

export interface ConnectInput {
  values: ConnectInputs;
  errors: readonly ConnectFieldError[];
  /** Why the write refused an otherwise valid form. */
  failure: string;
  probe: ConnectProbeState;
  /** "Connects to api.example.com:443", or "" before a domain is typed. */
  derivation: string;
  /** The composed endpoint, shown in the endpoint box when it is empty. */
  composedEndpoint: string;
  moreOpen: boolean;
}

/** The connect form's field names on the page: distinct from the install form's `domain`. */
export const CONNECT_FIELD_IDS = {
  name: "connectName",
  domain: "connectDomain",
  endpoint: "connectEndpoint",
  token: "connectToken",
} as const;

function sentence(text: string): string {
  return text === "" ? text : text[0]!.toUpperCase() + text.slice(1);
}

/**
 * The form for a cluster that runs somewhere else: a name and a domain, and
 * the two answers that already have defaults behind "More options".
 */
export function connectScreen(input: ConnectInput): RegionParts {
  const errorOf = (f: keyof ConnectInputs): string | undefined => {
    const message = input.errors.find((e) => e.field === f)?.message;
    return message === undefined ? undefined : sentence(message);
  };
  const box = (
    f: keyof ConnectInputs,
    label: string,
    opts: { placeholder?: string; hint?: string; type?: "text" | "password"; value?: string },
  ): string => {
    const id = `c-${f}`;
    const error = errorOf(f);
    // An error replaces the hint: one line under a box, not two.
    const hint = error === undefined ? (opts.hint ?? "") : "";
    const describedBy = error !== undefined ? `${id}-error` : hint !== "" ? `${id}-hint` : undefined;
    return field({
      id,
      label,
      ...(hint === "" ? {} : { hint }),
      ...(error === undefined ? {} : { error }),
      controlHtml: textInput({
        id,
        field: CONNECT_FIELD_IDS[f],
        value: opts.value ?? input.values[f],
        type: opts.type ?? "text",
        enterAct: "connect",
        ...(opts.placeholder === undefined ? {} : { placeholder: opts.placeholder }),
        ...(error === undefined ? {} : { invalid: true }),
        ...(describedBy === undefined ? {} : { describedBy }),
      }),
    });
  };
  const moreProblem = errorOf("endpoint") !== undefined || errorOf("token") !== undefined;
  const moreHasValue = input.values.endpoint.trim() !== "" || input.values.token.trim() !== "";

  const probe = input.probe;
  const notices =
    (input.failure === "" ? "" : notice({ tone: "error", line: sentence(input.failure) })) +
    (probe.state === "failed"
      ? notice({ tone: "warn", line: `Can't reach ${probe.endpoint}.`, next: sentence(probe.reason) })
      : "");

  const body =
    notices +
    box("name", "Name", { placeholder: "staging" }) +
    box("domain", "Domain", { placeholder: "example.com", hint: input.derivation }) +
    disclosure({
      act: "toggleMore",
      id: "more-options",
      label: "More options",
      open: input.moreOpen || moreProblem || moreHasValue,
      bodyHtml:
        box("endpoint", "Endpoint", {
          hint: "Change it only if the cluster uses another address.",
          value: input.values.endpoint.trim() === "" ? input.composedEndpoint : input.values.endpoint,
        }) +
        box("token", "Access token (optional)", { type: "password", hint: "Leave empty to sign in with your browser." }),
    });

  const checking = probe.state === "running";
  const primary: Act = checking
    ? { act: "connect", label: "Checking", tone: "primary", busy: true }
    : { act: "connect", label: probe.state === "failed" ? "Add anyway" : "Connect", tone: "primary" };
  return {
    head: head({ title: "Connect to a cluster" }),
    body,
    actions: actionBar({
      state: checking ? "Checking the cluster" : probe.state === "failed" ? "Not reachable" : "Not added yet",
      tone: checking ? "busy" : probe.state === "failed" ? "warn" : "idle",
      acts: [{ act: "connectCancel", label: "Cancel" }, primary],
    }),
  };
}

// -----------------------------------------------------------------------------
// a run: install, repair, uninstall
// -----------------------------------------------------------------------------

/** Where a run stands, as the screen draws it. */
export type RunPhase =
  /** In flight; Cancel is offered. */
  | "running"
  /** Cancel was pressed; the current step is finishing. */
  | "stopping"
  /** A step failed and other steps are still finishing: no acts yet. */
  | "finishing"
  /** It failed and has come to rest. */
  | "failed"
  /** Cancel took effect. */
  | "stopped"
  /** Every step is through; the page is finishing up (adding it to the list). No acts. */
  | "settling";

/** One failure, as its notice says it. */
export interface FailureView {
  /** The step id: the remedy's key, never the command. */
  id: string;
  /** The line: the script's own words, or the plain-words guidance. */
  line: string;
  /** The fix, when it is not a command. */
  next?: string;
  /** The command that fixes it, typed into a terminal for the person. */
  remedy?: string;
}

export interface RunInput {
  mode: "install" | "repair" | "uninstall";
  phase: RunPhase;
  /** 0-100; undefined before the plan arrives (the bar is indeterminate). */
  percent?: number;
  status: string;
  stepText: string;
  startedAt?: number;
  endedAt?: number;
  failures: readonly FailureView[];
  /** Whether an unchanged retry could succeed; Retry is absent when not. */
  retryable: boolean;
  logsOpen: boolean;
  /** Lines to draw into the document (the gallery); the panel streams them instead. */
  logLines?: readonly LogLine[];
  /** A pinned clock, for the gallery and tests. */
  now?: number;
}

/** The "Show logs" disclosure every run screen carries, with Copy and Open in Output on its header. */
export function logsDisclosure(open: boolean, lines: readonly LogLine[] = [], label = "Install log"): string {
  return disclosure({
    act: "toggleLogs",
    id: "run-logs",
    label: "Show logs",
    openLabel: "Hide logs",
    open,
    bodyHtml: logPane({
      id: "run-log",
      ariaLabel: label,
      lines,
      empty: "Waiting for output",
      acts: [
        { act: "copyLog", label: "Copy" },
        { act: "openOutput", label: "Open in Output" },
      ],
    }),
  });
}

function progressState(phase: RunPhase): ProgressState {
  switch (phase) {
    case "running":
    case "settling":
      return "running";
    case "stopping":
    case "stopped":
      return "stopping";
    case "finishing":
    case "failed":
      return "failed";
  }
}

function failureNotice(failure: FailureView): string {
  return notice({
    tone: "error",
    line: failure.line,
    ...(failure.next === undefined || failure.next === "" ? {} : { next: failure.next }),
    ...(failure.remedy === undefined || failure.remedy === ""
      ? {}
      : {
          codeHtml: codeBlock({
            text: failure.remedy,
            act: { act: "remedy", value: failure.id, label: "Run in terminal" },
          }),
        }),
  });
}

/** The progress values a run screen shows, for LiveView.progress between renders. */
export function runProgressUpdate(input: RunInput): {
  percent?: number;
  status: string;
  stepText?: string;
  startedAt?: number;
  endedAt?: number;
  state: ProgressState;
  title: string;
} {
  return {
    ...(input.percent === undefined ? {} : { percent: input.percent }),
    status: input.status,
    stepText: input.stepText,
    ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
    ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
    state: progressState(input.phase),
    title: RUN_WORDS[input.mode].title,
  };
}

/**
 * The one progress screen, for an install, a repair or an uninstall.
 *
 * WHAT IS ON IT: the mark, the act's own title, the bar, one status line and
 * "Step n of m · elapsed" (kit.progress), then -- on a failure -- one notice
 * per failed step with its fix, then "Show logs". The step checklist that used
 * to stand under the bar is gone: the bar, the status and the log already say
 * everything it said.
 *
 * WHAT THE BAR OFFERS is decided by the phase alone: Cancel while running;
 * nothing while stopping or while other steps finish after a failure (Retry
 * pressed then was dropped -- memql#5118 audit); Cancel and Retry (only when
 * retryable) once a failure has come to rest; Back and Resume once a stop has.
 */
export function runScreen(input: RunInput): RegionParts {
  const words = RUN_WORDS[input.mode];
  const update = runProgressUpdate(input);
  const body =
    progress({
      title: update.title,
      ...(update.percent === undefined ? {} : { percent: update.percent }),
      status: update.status,
      stepText: input.stepText,
      ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
      ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
      state: update.state,
      ...(input.now === undefined ? {} : { now: input.now }),
    }) +
    input.failures.map(failureNotice).join("") +
    logsDisclosure(input.logsOpen, input.logLines ?? [], input.mode === "uninstall" ? "Uninstall log" : "Install log");

  let bar: string;
  switch (input.phase) {
    case "running":
      bar = actionBar({ state: words.busy, tone: "busy", acts: [{ act: "cancel", label: "Cancel" }] });
      break;
    case "stopping":
      bar = actionBar({ state: "Stopping after the current step", tone: "busy", acts: [] });
      break;
    case "finishing":
      bar = actionBar({ state: "Finishing other steps", tone: "busy", acts: [] });
      break;
    case "settling":
      bar = actionBar({ state: "Finishing", tone: "busy", acts: [] });
      break;
    case "failed":
      bar = actionBar({
        state: words.failed,
        tone: "error",
        acts: [
          { act: "leave", label: "Cancel" },
          ...(input.retryable ? [{ act: "retry", label: "Retry", tone: "primary" as const }] : []),
        ],
      });
      break;
    case "stopped":
      bar = actionBar({
        state: "Not finished",
        tone: "idle",
        acts: [
          { act: "leave", label: "Back" },
          { act: "resume", label: "Resume", tone: "primary" },
        ],
      });
      break;
  }
  return { head: "", body, actions: bar };
}

// -----------------------------------------------------------------------------
// done: installed, repaired, or a local cluster added to the list
// -----------------------------------------------------------------------------

export interface RecoveryKeyView {
  state: RecoveryKeyState;
  /** The plaintext, for `claimed`; drawn only once `revealed`. */
  value: string;
  revealed: boolean;
  copied: boolean;
}

export interface DoneInput {
  kind: "installed" | "repaired" | "added";
  /** The cluster's name in the list. */
  name: string;
  /** Its gRPC address, `api.<domain>:443`. */
  address: string;
  /** MemQL OS for it, `https://os.<domain>/`. */
  osUrl: string;
  /** The editor holds a live session with it: the next act is MemQL OS, not sign-in. */
  signedIn: boolean;
  /** A passkey can be set up for the owner account the install created. */
  canEnrol: boolean;
  /** Nobody owns the cluster yet and a claim link was recovered: claiming is how to get in. */
  claim: boolean;
  recoveryKey?: RecoveryKeyView;
  /** The cluster is here but could not be written into the list (the detail is in the output). */
  notListed?: boolean;
  /** "16 steps", before the elapsed time. */
  stepText?: string;
  /** The run's own clock, when there was a run. */
  startedAt?: number;
  endedAt?: number;
  logsOpen?: boolean;
  logLines?: readonly LogLine[];
  now?: number;
}

const MASKED_KEY = "•••• •••• •••• •••• •••• ••••";

/** What a person can do about a recovery key nobody holds: true, and names no screen that is not there. */
export const RECOVERY_KEY_REPLACEABLE = "An owner can replace it later.";

function recoveryKeyBlock(key: RecoveryKeyView | undefined): string {
  if (key === undefined) return "";
  switch (key.state) {
    case "claimed": {
      if (key.value === "") return "";
      const copy: Act = { act: "copyRecoveryKey", label: key.copied ? "Copied" : "Copy" };
      const acts = key.revealed ? [copy] : [{ act: "revealRecoveryKey", label: "Show" }, copy];
      return (
        subhead("Recovery key") +
        `<div class="mq-code ac-key"><code class="mq-code-text">${escapeHtml(key.revealed ? key.value : MASKED_KEY)}</code>` +
        `${acts.map(button).join("")}</div>` +
        `<p class="ac-quiet">Shown once. Save it somewhere outside this computer.</p>`
      );
    }
    case "alreadyClaimed":
      return `<p class="ac-quiet">Your recovery key was saved during an earlier install.</p>`;
    case "awaitingOwner":
      return `<p class="ac-quiet">Your recovery key is created after you first sign in.</p>`;
    case "revealLost":
      // NO PLACE IS NAMED. MemQL OS has no recovery-key screen to send anyone
      // to (the rotation is an owner's admin call, or `memql recovery-key
      // claim --reclaim` inside the identity pod); a pointer to a page that
      // does not exist is worse than none.
      return notice({
        tone: "warn",
        line: "Nobody holds this cluster's recovery key.",
        next: RECOVERY_KEY_REPLACEABLE,
      });
    case "none":
      return "";
  }
}

const DONE_TITLE: Readonly<Record<DoneInput["kind"], string>> = {
  installed: "MemQL is installed",
  repaired: "MemQL is repaired",
  added: "Local cluster added",
};

/**
 * The end of an install, a repair, or of adding the local cluster to the list.
 *
 * ONE NEXT ACT, as the one button: Sign in, or Open MemQL OS when the editor
 * is already signed in. Setting up a passkey and claiming an unowned cluster
 * are text acts beside it, when they apply. The facts say where the cluster
 * is; they do not claim it answered -- a cluster added from the list may well
 * be stopped.
 *
 * THE RECOVERY KEY is shown once, behind Show so a shared screen does not
 * leak it, with Copy beside it; it goes when the page does.
 */
/** The done screen's progress values, for LiveView.progress: the bar the run ended on, settled. */
export function doneProgressUpdate(input: DoneInput): ProgressUpdate {
  return {
    percent: 100,
    status: doneStatus(input),
    stepText: input.stepText ?? "",
    ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
    ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
    state: "done",
    title: DONE_TITLE[input.kind],
  };
}

function doneStatus(input: DoneInput): string {
  return input.notListed === true ? "Not added to your clusters" : input.signedIn ? "Signed in" : "Ready to sign in";
}

export function doneScreen(input: DoneInput): RegionParts {
  const title = DONE_TITLE[input.kind];
  const status = doneStatus(input);
  const rows: FactRow[] = [
    { label: "Address", value: input.address, mono: true },
    { label: "MemQL OS", value: input.osUrl, mono: true },
  ];
  const body =
    progress({
      title,
      percent: 100,
      status,
      stepText: input.stepText ?? "",
      ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
      ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
      state: "done",
      ...(input.now === undefined ? {} : { now: input.now }),
    }) +
    `<div class="ac-column">${facts(rows)}${recoveryKeyBlock(input.recoveryKey)}</div>` +
    (input.startedAt === undefined ? "" : logsDisclosure(input.logsOpen === true, input.logLines ?? []));

  // BACK, as a quiet text act: to the landing, which looks at this computer
  // again. Leaving is what the recovery key's confirmation guards, so the
  // page offers it rather than leaving closing the tab as the only way off.
  const back: Act = { act: "back", label: "Back" };
  let acts: Act[];
  if (input.notListed === true) {
    acts = [back, { act: "retryHandoff", label: "Add to clusters", tone: "primary" }];
  } else {
    acts = [back];
    // One of the two at most: a cluster with an owner to enrol has nobody
    // left to claim it.
    if (input.canEnrol && !input.signedIn) acts.push({ act: "enrolPasskey", label: "Set up a passkey" });
    else if (input.claim && !input.signedIn) acts.push({ act: "claimCluster", label: "Claim this cluster" });
    acts.push(
      input.signedIn
        ? { act: "openOs", label: "Open MemQL OS", tone: "primary" }
        : { act: "signIn", label: "Sign in", tone: "primary" },
    );
  }
  const state = input.kind === "added" ? "In your clusters" : input.kind === "repaired" ? "Repaired" : "Installed";
  return {
    head: "",
    body,
    actions: actionBar({ state, tone: "live", acts }),
  };
}

// -----------------------------------------------------------------------------
// a cluster elsewhere, added
// -----------------------------------------------------------------------------

export interface AddedInput {
  name: string;
  address: string;
  osUrl: string;
  /** The entry carries a pasted access token, so there is nothing to sign in to first. */
  hasToken: boolean;
  /** Whether the reachability check passed before it was added. */
  reachable: boolean;
}

/** A remote cluster is in the list. The next act is signing in to it. */
export function addedScreen(input: AddedInput): RegionParts {
  return {
    // The bar says it is in the list; the head does not say it again.
    head: head({ title: input.name }),
    body:
      facts([
        { label: "Address", value: input.address, mono: true },
        { label: "MemQL OS", value: input.osUrl, mono: true },
      ]) +
      (input.reachable ? "" : `<p class="ac-quiet">It didn't answer when it was added. Sign in once it's running.</p>`),
    actions: actionBar({
      state: "In your clusters",
      tone: "live",
      acts: [
        // Back to the landing, to add another.
        { act: "back", label: "Back" },
        input.hasToken
          ? { act: "openOs", label: "Open MemQL OS", tone: "primary" }
          : { act: "signIn", label: "Sign in", tone: "primary" },
      ],
    }),
  };
}

// -----------------------------------------------------------------------------
// uninstall: what goes, what stays, and the choices
// -----------------------------------------------------------------------------

/** The typed confirmation the delete-data switch asks for. */
export const DELETE_DATA_PHRASE = "delete memql data";

export interface UninstallPreviewInput {
  /** The preview is being read: the page draws the list's shape. */
  loading: boolean;
  /** No local cluster was found to uninstall. `removeFromList`: a list entry still points at it. */
  nothingHere?: { removeFromList: boolean };
  /**
   * What an uninstall would remove could not be worked out -- the install's
   * record is unreadable, or the installer's own files are missing. Shown as
   * the failure it is, never as an empty computer.
   */
  unreadable?: boolean;
  rows: readonly RemovalRow[];
  sharedTools: readonly SharedToolRow[];
  /** The shared removals switched on. */
  chosen: ReadonlySet<string>;
  /** The delete-data consent, offered when a cluster MemQL did not make would otherwise be kept. */
  deleteData?: { on: boolean; phrase: string };
  /** The cluster's name, for the destructive confirmation sentence. */
  clusterName: string;
}

/** Whether the typed phrase is the phrase, exactly. */
export function phraseMatches(phrase: string): boolean {
  return phrase === DELETE_DATA_PHRASE;
}

/**
 * What an uninstall will remove, what it keeps, and the removals a person
 * opts into.
 *
 * EVERY OPT-IN IS A SWITCH, OFF: the shared tools, the certificate authority,
 * and -- when a cluster MemQL did not make would otherwise be kept -- deleting
 * that cluster and its data, in the danger tone, with the typed phrase under
 * it only while it is on. A tool that cannot be removed without another
 * (mkcert withdraws the certificate authority) is unavailable, with a line
 * saying which, until that other one is on.
 *
 * THE ONE BUTTON IS WHAT WILL HAPPEN. "Uninstall and delete data" appears only
 * once the phrase matches; with the data switch on and the phrase not yet
 * right there is nothing the run could do that the person has agreed to, so
 * there is no button; with nothing to remove at all there is none either.
 */
export function uninstallPreviewScreen(input: UninstallPreviewInput): RegionParts {
  const top = head({ title: TAB_TITLES.uninstall });
  if (input.loading) {
    return {
      head: top,
      body: skeleton({ shape: "list", rows: 3, label: "Reading what is installed" }),
      actions: actionBar({ state: "Checking this computer", tone: "busy", acts: [{ act: "uninstallBack", label: "Cancel" }] }),
    };
  }
  if (input.unreadable === true) {
    return {
      head: top,
      // The details are one click away on the bar (Open in Output), so the
      // notice does not also say where they are.
      body: notice({ tone: "error", line: "Couldn't work out what would be removed." }),
      actions: actionBar({
        state: "Nothing was removed",
        tone: "warn",
        acts: [
          { act: "uninstallBack", label: "Back" },
          { act: "openOutput", label: "Open in Output" },
          { act: "uninstallReload", label: "Try again", tone: "primary" },
        ],
      }),
    };
  }
  if (input.nothingHere !== undefined) {
    return {
      head: top,
      body: `<p class="ac-line">No local cluster was found on this computer.</p>`,
      actions: actionBar({
        // Why "Remove from list" is here, when it is: the entry outlived the cluster.
        state: input.nothingHere.removeFromList ? "Still in your clusters" : "Nothing to uninstall",
        tone: "idle",
        acts: [
          { act: "uninstallBack", label: "Back" },
          ...(input.nothingHere.removeFromList
            ? [{ act: "removeFromList", label: "Remove from list", tone: "primary" as const }]
            : []),
        ],
      }),
    };
  }

  const confirmed = input.deleteData !== undefined && input.deleteData.on && phraseMatches(input.deleteData.phrase);
  // The kept cluster moves into "Will be removed" the moment the consent is
  // whole, so the list says what the button will do. SAID ONCE: that row is
  // the one place "every database in it" appears; the switch that consents to
  // it says only that it is final.
  const rows = input.rows.map((row) => {
    if (row.id !== "removeCluster" || !row.kept) return row;
    return confirmed
      ? { ...row, kept: false, reason: "", detail: `${input.clusterName}, and every database in it` }
      : { ...row, reason: input.deleteData === undefined ? row.reason : "Kept unless you delete its data" };
  });
  // The cluster first, wherever the consent moved it from.
  const removed = rows.filter((row) => !row.kept).sort((a, b) => clusterFirst(a.id) - clusterFirst(b.id));
  const kept = rows.filter((row) => row.kept);

  let body = subhead("Will be removed");
  body += removed.length === 0 ? `<p class="ac-empty">Nothing yet. Turn on what you want removed below.</p>` : rowList(removed);
  if (kept.length > 0) body += subhead("Kept") + rowList(kept);

  if (input.sharedTools.length > 0) {
    body += subhead("Also remove");
    for (const tool of input.sharedTools) {
      const needs = tool.requires === undefined ? undefined : input.sharedTools.find((t) => t.id === tool.requires);
      const unavailable = needs !== undefined && !input.chosen.has(needs.id);
      body += switchRow({
        id: `shared-${tool.id}`,
        label: tool.name,
        note: unavailable ? `Turn on ${lowerFirst(needs!.name)} first.` : tool.note,
        checked: input.chosen.has(tool.id) && !unavailable,
        ...(unavailable ? { disabled: true } : {}),
        data: { "switch-act": "shared", value: tool.id },
      });
    }
  }

  if (input.deleteData !== undefined) {
    const d = input.deleteData;
    body += subhead("Data");
    body += switchRow({
      id: "delete-data",
      label: "Delete the cluster's data",
      note: "This can't be undone.",
      checked: d.on,
      tone: "danger",
      data: { "switch-act": "deleteData" },
    });
    if (d.on) {
      const typed = d.phrase !== "";
      const matches = phraseMatches(d.phrase);
      body += field({
        id: "delete-phrase",
        label: `Type "${DELETE_DATA_PHRASE}" to confirm`,
        ...(typed && !matches ? { error: "Doesn't match yet." } : {}),
        controlHtml: textInput({
          id: "delete-phrase",
          field: "deletePhrase",
          value: d.phrase,
          ...(typed && !matches ? { invalid: true, describedBy: "delete-phrase-error" } : {}),
        }),
      });
    }
  }

  const wouldRemove = removed.length > 0 || [...input.chosen].some((id) => input.sharedTools.some((t) => t.id === id));
  const dataOn = input.deleteData?.on === true;
  const acts: Act[] = [{ act: "uninstallBack", label: "Cancel" }];
  let state = "Ready to uninstall";
  // NO CONFIRMATION SENTENCE OVER THE BAR. The list above already reads "The
  // cluster · memql, and every database in it" under "Will be removed", and
  // the switch says it can't be undone: a third telling above the button is
  // the same fact again, not a further consent.
  if (dataOn) {
    if (confirmed) {
      acts.push({ act: "uninstallStart", label: "Uninstall and delete data", tone: "danger" });
    } else {
      state = "Type the phrase to confirm";
    }
  } else if (wouldRemove) {
    acts.push({ act: "uninstallStart", label: "Uninstall", tone: "danger" });
  } else {
    state = "Nothing to remove";
  }
  return {
    head: top,
    body,
    actions: actionBar({ state, tone: "idle", acts }),
  };
}

function clusterFirst(id: string): number {
  return id === "removeCluster" ? 0 : 1;
}

function lowerFirst(text: string): string {
  return text === "" ? text : text[0]!.toLowerCase() + text.slice(1);
}

// -----------------------------------------------------------------------------
// uninstall finished
// -----------------------------------------------------------------------------

export interface UninstalledInput {
  removed: number;
  kept: number;
  /** The machine is clean but the editor's own records of it were not ("" when they were). */
  followUpProblem: string;
  /** Of those records, the list entry is the one left behind: the person can remove it themselves. */
  stillListed?: boolean;
  startedAt?: number;
  endedAt?: number;
  logsOpen: boolean;
  logLines?: readonly LogLine[];
  now?: number;
}

/** The finished uninstall's progress values, for LiveView.progress. */
export function uninstalledProgressUpdate(input: UninstalledInput): ProgressUpdate {
  return {
    percent: 100,
    status: input.kept === 0 ? "Nothing left behind" : "Kept what was here before MemQL",
    stepText: input.kept === 0 ? `${input.removed} removed` : `${input.removed} removed · ${input.kept} kept`,
    ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
    ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
    state: "done",
    title: RUN_WORDS.uninstall.done,
  };
}

/** It is off this computer, apart from what was here before MemQL. */
export function uninstalledScreen(input: UninstalledInput): RegionParts {
  const update = uninstalledProgressUpdate(input);
  const body =
    progress({
      title: update.title ?? RUN_WORDS.uninstall.done,
      percent: 100,
      status: update.status,
      stepText: update.stepText ?? "",
      ...(input.startedAt === undefined ? {} : { startedAt: input.startedAt }),
      ...(input.endedAt === undefined ? {} : { endedAt: input.endedAt }),
      state: "done",
      ...(input.now === undefined ? {} : { now: input.now }),
    }) +
    // THE PROBLEM'S OWN TEXT IS DETAIL (a file and an errno), and it is in the
    // MemQL Install output; the notice says which record was left and what to
    // do about it.
    (input.followUpProblem === ""
      ? ""
      : input.stillListed === true
        ? notice({ tone: "warn", line: "It's uninstalled, but it's still in your clusters.", next: "Remove it from the list in the Clusters view." })
        : notice({
            tone: "warn",
            line: "It's uninstalled, but MemQL still has a record of it.",
            acts: [{ act: "openOutput", label: "Open in Output", tone: "secondary" }],
          })) +
    logsDisclosure(input.logsOpen, input.logLines ?? [], "Uninstall log");
  return {
    head: "",
    body,
    actions: actionBar({ state: "Uninstalled", tone: "idle", acts: [{ act: "leave", label: "Back" }] }),
  };
}
