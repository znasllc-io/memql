// The page the runtime checks run in: a real kit document (pageDocument, the
// real stylesheet, the real PAGE_RUNTIME) plus the HTML of every re-render
// the checks patch it with.
//
// WHY A BROWSER. The page-to-host half of runtime.ts is checked under
// `node --test` against a hand-made document (test/pageRuntime.test.ts). The
// host-to-page half -- a patch morphing live DOM around a focused field, the
// bar and the clock moving under `progress`, log lines streaming into a pane
// that follows its tail -- only means anything against a real DOM, so it runs
// here, in headless Chrome, driven by gallery/check.mjs.
//
// The checks themselves are plain browser script (runtime.checks.js): this
// module only builds what they act on, so every region below is exactly what
// a panel would render.

import { pageDocument } from "../../src/webview/ui/document.js";
import { actionBar, button, disclosure, field, head, logPane, progress, switchRow, textInput } from "../../src/webview/ui/kit.js";

/** The one nonce the page carries; the injected scripts ride it, as a panel's own would. */
const NONCE = "checks";

interface BodyState {
  name?: string;
  nameError?: string;
  other?: string;
  phraseType?: "text" | "password";
  checked?: boolean;
  status?: string;
  items?: readonly string[];
  pickLabel?: string;
}

/** The body region in a given state; each state is one host re-render. */
function body(s: BodyState): string {
  return (
    field({ id: "f-name", label: "Name", error: s.nameError, controlHtml: textInput({ id: "f-name", field: "name", value: s.name ?? "" }) }) +
    field({ id: "f-other", label: "Other", controlHtml: textInput({ id: "f-other", field: "other", value: s.other ?? "" }) }) +
    field({ label: "Phrase", controlHtml: textInput({ field: "phrase", value: "", type: s.phraseType ?? "text" }) }) +
    switchRow({ id: "s1", label: "Switch", note: "A note", checked: s.checked ?? false, data: { "switch-act": "shared", value: "k3d" } }) +
    progress({ title: "Installing MemQL", percent: 10, status: s.status ?? "Checking this computer", stepText: "Step 1 of 16", startedAt: 1000, state: "running" }) +
    disclosure({ act: "toggleLogs", id: "logs", label: "Show logs", openLabel: "Hide logs", open: true, bodyHtml: logPane({ id: "log1", ariaLabel: "Log", lines: [] }) }) +
    `<div id="list">${(s.items ?? ["a", "b", "c"]).map((x) => `<p id="i-${x}">${x}</p>`).join("")}</div>` +
    button({ act: "pick", value: "a", label: s.pickLabel ?? "Pick" })
  );
}

/** Every re-render the checks send as a `patch`, by name. */
function variants(): Record<string, string> {
  return {
    nameError: body({ name: "hello", nameError: "A cluster named hello is already in the list." }),
    otherX: body({ name: "hello", other: "X" }),
    otherCleared: body({ name: "hello", other: "" }),
    switchOff: body({ name: "hello", other: "", checked: false }),
    staleStatus: body({ name: "hello", other: "", status: "An older status" }),
    phrasePassword: body({ name: "hello", other: "", phraseType: "password" }),
    reordered: body({ name: "hello", other: "", phraseType: "password", items: ["c", "a", "b", "d"] }),
    relabelled: body({ name: "hello", other: "", phraseType: "password", items: ["c", "a", "b", "d"], pickLabel: "Picked" }),
  };
}

/**
 * The whole check page. A recording `acquireVsCodeApi` goes in before the
 * page script, the re-renders as data, and `checksJs` after it; the page's own
 * CSP stays in force, so a check that needed it loosened would fail here too.
 */
export function runtimeCheckDocument(checksJs: string): string {
  const html = pageDocument({
    nonce: NONCE,
    title: "Runtime checks",
    themeAttr: ` data-memql-theme="light"`,
    screen: "checks",
    escapeAct: "back",
    head: head({ title: "Runtime checks" }),
    body: body({}),
    actions: actionBar({ state: "Installing", tone: "busy", acts: [{ act: "cancel", label: "Cancel" }] }),
  });
  // `<` is escaped so no re-render's markup can end the script element early.
  const data = JSON.stringify(variants()).replace(/</g, "\\u003c");
  const shim =
    `<script nonce="${NONCE}">window.__posts = []; window.__variants = ${data};` +
    `window.acquireVsCodeApi = function () { var state; return { postMessage: function (m) { window.__posts.push(m); },` +
    ` getState: function () { return state; }, setState: function (s) { state = s; } }; };</script>`;
  return html
    .replace("<head>", () => `<head>\n${shim}`)
    .replace("</body>", () => `<script nonce="${NONCE}">\n${checksJs}\n</script>\n</body>`);
}
