// The gallery photographs the email pages under the variables VS Code gives a
// webview in Dark Modern and Light Modern (editors/vscode/gallery/harness.ts).
// A page that reads a variable the harness lacks is photographed with its
// fallback, or with nothing, instead of what the editor shows; so every
// --vscode-* name the pages read has its theme's value there, or is one of the
// few VS Code itself leaves unset in those themes (and the page then reads it
// with a fallback of its own).

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { emailPage } from "../src/emailPage.js";
import { newEmailPage } from "../src/newEmailPage.js";
import { GALLERY_THEMES, VSCODE_THEME_VARS, VSCODE_UNSET_VARS } from "../../vscode/gallery/harness.js";

/**
 * What the two pages are made of: their static markup and styles, the sources
 * of their view scripts, and the view bundles (dist-test, built before the
 * tests), which also hold what the modules the views import write at run time.
 */
function pageTexts(): string[] {
  return [
    emailPage({ title: "Welcome.email.json", script: "email-view.js", nonce: "gallery" }),
    newEmailPage({ script: "new-email-view.js", nonce: "gallery" }),
    readFileSync("src/emailView.ts", "utf8"),
    readFileSync("src/newEmailView.ts", "utf8"),
    readFileSync("dist-test/email-view.js", "utf8"),
    readFileSync("dist-test/new-email-view.js", "utf8"),
  ];
}

const pageVariables = () => [...new Set(pageTexts().flatMap(text => text.match(/--vscode-[A-Za-z0-9-]+/g) ?? []))].sort();

test("every theme variable the email pages read has a value in the gallery, in both themes", () => {
  const read = pageVariables();
  assert.ok(read.includes("--vscode-focusBorder") && read.includes("--vscode-toolbar-hoverBackground"), "the pages' variables were found");
  for (const theme of GALLERY_THEMES) {
    const missing = read.filter(name => !Object.hasOwn(VSCODE_THEME_VARS[theme], name) && !VSCODE_UNSET_VARS.includes(name));
    assert.deepEqual(missing, [], `${theme}: the gallery has no value for these`);
  }
});

test("a variable VS Code leaves unset in these themes has no value in the gallery, and the pages read it with a fallback", () => {
  for (const name of VSCODE_UNSET_VARS) {
    for (const theme of GALLERY_THEMES) assert.equal(Object.hasOwn(VSCODE_THEME_VARS[theme], name), false, `${theme} gives ${name} a value`);
    for (const text of pageTexts()) {
      const uses = text.split(name).length - 1;
      const withFallback = text.split(`var(${name},`).length - 1;
      assert.equal(withFallback, uses, `${name} is read without a fallback`);
    }
  }
});
