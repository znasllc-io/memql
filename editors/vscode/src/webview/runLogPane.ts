// The run's actual output, behind a disclosure (memql#4455).
//
// WHAT CHANGED AND WHY. Verbatim subprocess output used to be the run screen:
// the step checklist carried each step's stderr, and the failure screen led
// with it. That is the right content in the wrong place -- for the twelve
// minutes an install is going well, the operator does not want kubectl's
// account of it, and reading a product's install as a wall of shell output is
// what makes it feel like a troubleshooting session rather than a product.
//
// So the output has ONE home on these surfaces, and it is this pane. Nothing
// else renders `StepProgress.log`: not the checklist, not the failure summary,
// not the facts. What the failure summary renders instead is the step's
// DESCRIPTION, the capability's own `reason` sentence -- written for humans by
// contract -- and the remedy, which is an action-adjacent fact and stays
// outside the pane where it can be acted on without opening anything.
//
// AND FAILURE OPENS IT. Hiding the log behind a click at the exact moment
// something broke would be design spite; `AddClusterState` sets `logsOpen` on
// the first failure and the pane anchors on that step. De-emphasising the log
// is for the common case, not for the case where the log IS the product.
//
// REDACTION IS NOT OPTIONAL HERE. Everything through this pane goes through
// `redactForDisplay` -- the one redactor for human surfaces (memql#4194) --
// which masks the operator's home directory and scrubs anything shaped like a
// credential. A key an install seeded reaches stderr more easily than anyone
// would like.
//
// Deliberately free of `vscode` imports (cmd/memql-lsp/vscodeimportrule_test.go).
//
// Refs: #4455 #4194 #4452

import * as os from "node:os";

import { escapeHtml } from "@znasllc-io/memql-view-kit";

import { redactForDisplay } from "../install/secrets.js";
import type { StepProgress } from "../state/addCluster.js";
import { renderDisclosure } from "./screenLayout.js";

/**
 * How many lines the run has produced, across every step.
 *
 * THE HONEST SIZE GOES ON THE CLOSED CONTROL, so "Show logs -- 214 lines" tells
 * an operator whether opening it is worth the scroll, and an empty run renders
 * a disabled "No output yet" instead of a toggle that opens onto nothing.
 * Counted off the raw log rather than the redacted one: redaction replaces
 * within a line and never adds or removes one, so the two agree -- and counting
 * the redacted text would mean redacting every line to render a number.
 */
export function logLineCount(steps: readonly StepProgress[]): number {
  let count = 0;
  for (const step of steps) {
    if (step.log === "") continue;
    count += step.log.split("\n").length;
  }
  return count;
}

export interface LogPaneInput {
  steps: readonly StepProgress[];
  /** From the STATE module, never from the DOM -- see the header. */
  open: boolean;
  /**
   * Whether the pane is still pinned to the tail.
   *
   * TRUE UNTIL THE OPERATOR SCROLLS UP. A pane that jumped to the bottom while
   * somebody was reading the middle of it would be unusable during exactly the
   * part of a run that produces output; scrolling back to the bottom re-arms
   * it. The flag rides to the webview as an attribute and the panel's one
   * script acts on it -- there is no scroll position in this HTML.
   */
  follow: boolean;
  /**
   * The step the pane scrolls to when it opens -- the first failure, or none.
   *
   * NAMED BY ID rather than by index, because a retry reorders nothing but does
   * change which steps are pending, and an index would drift onto a step that
   * did not fail.
   */
  focusStepId?: string;
  /** The home directory to mask. A parameter for the tests; callers default. */
  home?: string;
}

/**
 * The disclosure, and the pane behind it.
 *
 * A STEP WITH NO OUTPUT IS NOT DRAWN. A run has thirteen steps and most of them
 * say nothing at all; rendering an empty block each would bury the two that
 * spoke under eleven that did not.
 */
export function renderRunLogPane(input: LogPaneInput): string {
  const home = input.home ?? os.homedir();
  const lines = logLineCount(input.steps);
  if (lines === 0) {
    return renderDisclosure({
      act: "toggleLogs",
      summary: "",
      open: false,
      body: "",
      empty: "No output yet",
    });
  }

  const blocks = input.steps
    .filter((step) => step.log !== "")
    .map((step) => {
      const name = step.description === "" ? step.id : step.description;
      // The anchor rides on the failed step so the panel's script can bring it
      // into view. It is an attribute rather than a fragment id because the
      // document is replaced wholesale on every render, and a `#hash` would
      // re-navigate the page each time.
      const focus = step.id === input.focusStepId ? " data-log-focus=\"true\"" : "";
      // THE EXIT CODE LIVES HERE AND NOWHERE ELSE (memql#4456). It used to ride
      // on the checklist row as well; D4 gives envelope facts exactly one home,
      // and this is it -- beside the output that explains the number. Only on a
      // failure: on any other state it is absent or stale, and a step that
      // succeeded showing "exit 5" would be alarming and wrong.
      const code =
        step.state === "failed" && step.exitCode !== null
          ? ` <span class="log-step-code data">exit ${escapeHtml(String(step.exitCode))}</span>`
          : "";
      return `<div class="log-step" data-status="${escapeHtml(step.state)}"${focus}>
  <div class="log-step-name">${escapeHtml(name)}${code}</div>
  <pre class="log-step-output data">${escapeHtml(redactForDisplay(step.log, home))}</pre>
</div>`;
    })
    .join("");

  return renderDisclosure({
    act: "toggleLogs",
    summary: `Show logs -- ${lines} ${lines === 1 ? "line" : "lines"}`,
    summaryOpen: "Hide logs",
    open: input.open,
    paneClass: "log-pane",
    paneData: { "log-pane": "true", follow: String(input.follow) },
    body: blocks,
  });
}
