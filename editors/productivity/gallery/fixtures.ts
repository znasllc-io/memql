// What the gallery's scenarios are built from: the emails the engine rendered
// (gallery/build.mjs has TestWriteEditorGalleryFixtures in
// component/compose/emaildoc write them into dist-gallery/fixtures), and the
// host's side of a page, played by a script after the page's own.

import * as fs from "node:fs";
import * as path from "node:path";

import { withHostScript } from "../../vscode/gallery/harness.js";

/** One email as the engine wrote it: the layout it comes from, its document, and its render in preview mode. */
export interface EmailFixture {
  /** The layout's id ("builtin:article"); empty for an email written in HTML. */
  id: string;
  name: string;
  description: string;
  document: { blocks?: unknown[]; [key: string]: unknown };
  subject: string;
  text: string;
  issues: { code: string; block?: string; message: string }[];
  html: string;
}

/** The emails, by file name without ".json": "article" (as it ships), "article-finished", "confirmation-hero-missing", "custom". */
export type Fixtures = ReadonlyMap<string, EmailFixture>;

export function readFixtures(dir: string): Fixtures {
  const fixtures = new Map<string, EmailFixture>();
  for (const name of fs.readdirSync(dir).filter(file => file.endsWith(".json")).sort()) {
    fixtures.set(name.replace(/\.json$/, ""), JSON.parse(fs.readFileSync(path.join(dir, name), "utf8")) as EmailFixture);
  }
  return fixtures;
}

/** The email a scenario needs; a missing one means the fixtures are not the ones this gallery was written for. */
export function email(fixtures: Fixtures, name: string): EmailFixture {
  const found = fixtures.get(name);
  if (!found) throw new Error(`gallery: no email fixture "${name}" (npm run gallery writes them; is TestWriteEditorGalleryFixtures up to date?)`);
  return found;
}

/** JSON that can sit in an inline script: no "<" to end it early (a line separator is fine in a string since ES2019). */
const scriptJSON = (value: unknown) => JSON.stringify(value).replace(/</g, "\\u003c");

/** What the person does once the email has rendered, so a capture shows the part of the page a scenario is about. */
export interface Then {
  /** Click this block's row in the outline, which brings the block into view in the preview. */
  reveal?: string;
  /** Scroll the element with this id into view, as the person scrolls the inspector to it. */
  show?: string;
}

/**
 * The page with the host's side played after its view script: each message is
 * delivered in order, from the webview's own origin, as VS Code delivers what
 * a panel posts. `then` runs once the first render has loaded and the page has
 * laid it out (the view handles that load first), or at once when none is coming.
 */
export function withHostMessages(html: string, messages: readonly unknown[], then: Then = {}): string {
  return withHostScript(html, `(function () {
  var messages = ${scriptJSON(messages)}, then = ${scriptJSON(then)};
  messages.forEach(function (data) { window.dispatchEvent(new MessageEvent("message", { data: data, origin: window.origin })); });
  function act() {
    if (then.reveal) {
      var row = Array.prototype.find.call(document.querySelectorAll('[role="treeitem"]'), function (item) { return item.dataset.block === then.reveal; });
      if (row) row.click();
    }
    if (then.show) { var shown = document.getElementById(then.show); if (shown) shown.scrollIntoView({ block: "start" }); }
  }
  var pending = document.querySelector('iframe[data-pending="1"]');
  if (pending) pending.addEventListener("load", act); else act();
})();`);
}
