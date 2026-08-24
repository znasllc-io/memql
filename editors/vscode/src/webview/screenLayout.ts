// The order every deployment surface renders in, stated once (memql#4453).
//
// WHY THIS FILE EXISTS. Actions used to render LAST on every screen in both
// panels -- heading, lede, facts or a step checklist, and then, below the fold,
// the buttons. Nothing about that order was load-bearing; it was accretion, one
// screen at a time, each one appending its `<div class="actions">` to the end of
// a template literal. The result is a deployment page where the thing an
// operator opened it to DO is the thing they have to scroll for.
//
// So the order is a function rather than a convention. A screen hands over its
// parts and this decides where they go:
//
//   1. the heading
//   2. ACTIONS      -- what this screen offers, visible without scrolling
//   3. status       -- the progress block, or the screen's headline facts
//   4. details      -- forms, fact rows, step specifics
//   5. logs         -- last, and collapsed (memql#4455)
//
// WHAT THIS DOES NOT DECIDE IS WHICH BUTTONS EXIST. `deploy/instanceActions.ts`
// owns that, including the doctrine that no button is offered whose only
// outcome is a refusal. This file moves buttons; it never adds or removes one.
// A screen that renders no actions passes none and gets no actions row, which
// is how a reader's view of an instance stays a reader's view.
//
// THE FAILURE SCREEN IS NOT AN EXCEPTION, and it is the one most likely to be
// made into one. Retry belongs at the top for exactly the reason every other
// action does: an operator reading a failure is an operator deciding what to do
// next, and the failure summary is the input to that decision, not a queue in
// front of it.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go),
// which is what lets `test/screenOrdering.test.ts` assert the order on rendered
// HTML under bare `node --test`.
//
// Refs: #4453 #4455 #4452

import { escapeHtml } from "@znasllc-io/memql-view-kit";

/**
 * One screen's parts, in the slots the doctrine names.
 *
 * EVERY SLOT BUT THE TITLE IS OPTIONAL, and an absent slot renders nothing at
 * all rather than an empty container -- a screen with no logs should not carry
 * a hollow log region, and a `<div class="actions"></div>` with nothing in it
 * draws a gap where an operator will look for a button.
 *
 * The slot values are ALREADY-RENDERED HTML the caller composed, and are
 * interpolated verbatim; only `title` is escaped here. That is the same
 * arrangement `brandHeader` already has, and the same obligation: every value
 * that reaches a slot went through `escapeHtml` on its way in.
 */
export interface ScreenParts {
  /** The screen's own heading. Escaped here. */
  title: string;
  /** The buttons this screen offers. Rendered second, always above the fold. */
  actions?: string;
  /** The progress block, or this screen's headline facts. */
  status?: string;
  /** Forms, fact rows, step specifics -- everything below the decision. */
  details?: string;
  /** The collapsed log or diagnostics disclosure. Always last. */
  logs?: string;
}

/**
 * A screen, in the one order every screen renders in.
 *
 * The blank-line joining is deliberate rather than cosmetic: the ordering test
 * asserts on the POSITION of `class="actions"` against the status block in the
 * output string, and predictable separators keep those assertions about the
 * order rather than about whitespace.
 */
export function renderScreen(parts: ScreenParts): string {
  const actions = parts.actions ?? "";
  return [
    `<h1>${escapeHtml(parts.title)}</h1>`,
    actions === "" ? "" : `<div class="actions">${actions}</div>`,
    parts.status ?? "",
    parts.details ?? "",
    parts.logs ?? "",
  ]
    .filter((part) => part !== "")
    .join("\n");
}

/**
 * A collapsed section, whose open/closed state lives in the EXTENSION.
 *
 * NOT `<details>`, AND THAT IS THE WHOLE POINT (memql#4455). Both panels
 * re-render by assigning `webview.html`, which replaces the entire document --
 * during a run that happens on every `stepLog`, roughly once a second. A
 * `<details open>` an operator opened is DOM state, so it would close itself a
 * second later, and it would do it while they were reading. So the control is a
 * button, the pane is emitted only when the state says open, and the toggle is
 * a message like every other control on these pages.
 *
 * ONE COMPONENT, TWO CONSUMERS. The run log disclosure and the deployment
 * pages' Diagnostics section are the same thing -- material that is worth
 * keeping and not worth leading with -- and a second implementation would be a
 * second answer to what "collapsed" looks like in this extension.
 *
 * AN EMPTY SECTION SAYS SO RATHER THAN OFFERING TO OPEN NOTHING. A toggle that
 * opens onto a blank pane teaches an operator that the control is broken, at
 * the exact moment they are looking for output that has not arrived yet.
 */
export interface DisclosureInput {
  /** The `data-act` id the toggle posts. The panel maps it to a state flag. */
  act: string;
  /** The label when closed, e.g. `Show logs -- 214 lines`. Escaped here. */
  summary: string;
  /** The label when open. Defaults to `Hide` + the summary's own noun. */
  summaryOpen?: string;
  /** Whether the pane is currently open, from the STATE module. */
  open: boolean;
  /** The pane's contents, already rendered. Emitted only when open. */
  body: string;
  /**
   * What to say instead of a toggle when there is nothing to disclose.
   *
   * When set, this renders a DISABLED control carrying this text and no pane.
   * Callers that can never be empty leave it undefined.
   */
  empty?: string;
  /** An extra class on the pane, for the consumer's own styling. */
  paneClass?: string;
  /**
   * `data-*` attributes for the pane, for a consumer whose pane the panel's
   * script has to find and act on -- the log pane's follow-tail is the only
   * one today.
   *
   * A TYPED MAP RATHER THAN A STRING the caller appends to `paneClass`. That
   * was the first shape of this and it smuggled markup through an attribute
   * value: anything the caller got wrong would have escaped the class
   * attribute and become markup, on the one surface in this extension whose
   * whole security story is that every value goes through `escapeHtml`.
   */
  paneData?: Readonly<Record<string, string>>;
}

export function renderDisclosure(input: DisclosureInput): string {
  if (input.empty !== undefined) {
    return `<div class="disclosure">
  <button class="disclosure-toggle" type="button" disabled>${escapeHtml(input.empty)}</button>
</div>`;
  }
  const label = input.open ? (input.summaryOpen ?? `Hide ${input.summary}`) : input.summary;
  const data = Object.entries(input.paneData ?? {})
    .map(([key, value]) => ` data-${escapeHtml(key)}="${escapeHtml(value)}"`)
    .join("");
  const pane = input.open
    ? `\n  <div class="disclosure-pane ${escapeHtml(input.paneClass ?? "")}"${data}>${input.body}</div>`
    : "";
  return `<div class="disclosure" data-open="${input.open}">
  <button class="disclosure-toggle" type="button" data-act="${escapeHtml(
    input.act,
  )}" aria-expanded="${input.open}">${escapeHtml(label)}</button>${pane}
</div>`;
}

/**
 * The one piece of behaviour a disclosure needs on the webview side, shared by
 * both panels (memql#4455).
 *
 * WHY THERE IS ANY SCRIPT AT ALL. Everything else about the disclosure is
 * state-held and re-rendered -- open/closed, the size line, which step is
 * anchored. Scroll POSITION is the exception: it is not expressible in HTML,
 * and the document is replaced on every `stepLog`, so without this the pane
 * would jump back to the top roughly once a second during the exact stretch of
 * a run that produces output.
 *
 * WHAT IT DOES NOT DO IS DECIDE. It reads `data-follow` and `data-log-focus`,
 * both written by the renderer from panel state, and it reports where the
 * operator scrolled to. The intent lives in the state module; this moves a
 * scrollbar and posts a boolean.
 *
 * A STRING CONSTANT SHARED BY TWO PANELS rather than copied into each. The two
 * would drift, and the drift is invisible: each panel's pane would still
 * scroll, just differently, and nobody compares them.
 *
 * NO BACKTICKS AND NO `${` IN HERE -- both panels inline this inside their own
 * template literal, so either would be consumed before the browser saw it.
 */
export const LOG_PANE_SCRIPT = `
  // The log pane's scroll, which is the one thing about the disclosure that
  // cannot live in the extension's state (memql#4455).
  const logPane = document.querySelector('[data-log-pane]');
  if (logPane) {
    // THE FAILURE ANCHOR OUTRANKS THE TAIL. When a step has failed the pane was
    // opened FOR that step, and landing on the newest line instead would put
    // the operator at the bottom of output belonging to whatever ran last.
    // Scrolled WITHIN the pane rather than via scrollIntoView, which would also
    // scroll the page and carry the actions row off the top of it.
    const focus = logPane.querySelector('[data-log-focus]');
    if (focus) {
      logPane.scrollTop = focus.offsetTop - logPane.offsetTop;
    } else if (logPane.dataset.follow === 'true') {
      logPane.scrollTop = logPane.scrollHeight;
    }
    logPane.addEventListener('scroll', () => {
      // A few pixels of slack: a pane pinned to the bottom is regularly a
      // fraction of a pixel off it after a repaint, and an exact comparison
      // would disarm follow-tail on its own scroll.
      const atBottom = logPane.scrollHeight - logPane.scrollTop - logPane.clientHeight < 8;
      vscode.postMessage({ type: 'logsFollow', value: atBottom });
    });
  }
`;
