import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/**
 * THE EXTENSION'S NAME IS THE EXTENSION'S TO SPELL (memql#5390, fix round 1).
 *
 * MemQL OS tells an operator which editor extension to install. It is an
 * extension for TWO editors, and its own README and package.json call it
 * "MemQL for Visual Studio Code and Cursor" -- so a shell string saying "MemQL
 * for VS Code" reads to a Cursor user as "not for you". That is exactly what
 * Settings -> Language shipped, and nothing caught it, because no gate covered
 * OS copy at all.
 *
 * This is that gate, and it is deliberately narrow. It does NOT police the
 * words "VS Code": the OS says them legitimately (the desktop's "Open in VS
 * Code" action, the handoff's "VS Code did not answer"), and the extension's
 * own README says them once. It polices the PRODUCT NAME -- the phrase
 * "MemQL for ..." -- which is the extension's brand and the thing an operator
 * searches a marketplace for.
 *
 * THE EXPECTED NAME IS READ FROM THE EXTENSION, never written down here. A
 * literal would be a second copy of the name in a third language, which is the
 * defect this file exists to catch; read from source, a rename in
 * editors/vscode fails every OS string that has not followed it.
 *
 * THE SCOPE IS `clients/os/src`, AND THAT IS LOAD-BEARING, not lazy: this
 * comment quotes the wrong spelling twice in order to explain itself, so a
 * scan widened to `clients/os` would fail on the gate's own explanation.
 */

const here = dirname(fileURLToPath(import.meta.url));
const REPO = join(here, "..", "..", "..");
const EXTENSION = join(REPO, "editors", "vscode");
const OS_SRC = join(here, "..", "src");

/** Every "MemQL for ..." run: letters, digits and the punctuation a product
 *  name uses, stopping at a template hole, a quote or a tag. */
const PRODUCT_PHRASE = /MemQL for [A-Za-z0-9 .+&'-]*/g;

/** The extension's own name, from its README's H1. */
function productName(): string {
  const h1 = readFileSync(join(EXTENSION, "README.md"), "utf8").split("\n")[0] ?? "";
  const name = h1.replace(/^#\s*/, "").trim();
  if (!name.startsWith("MemQL for ")) {
    throw new Error(`editors/vscode/README.md's first line is ${JSON.stringify(h1)}; expected the product's "MemQL for ..." name`);
  }
  return name;
}

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) out.push(...sourceFiles(path));
    else if (/\.tsx?$/.test(entry.name)) out.push(path);
  }
  return out;
}

describe("the editor extension's name in OS copy", () => {
  it("is the name the extension's own README and package.json use", () => {
    const name = productName();
    // The README's H1 and the manifest's description must agree about WHICH
    // editors, or this gate would pin a name half the product has moved off.
    const description = String(
      (JSON.parse(readFileSync(join(EXTENSION, "package.json"), "utf8")) as { description?: unknown }).description ?? "",
    );
    for (const editor of name.slice("MemQL for ".length).split(" and ")) {
      expect(description, `editors/vscode/package.json's description does not name ${editor}`).toContain(editor);
    }
    // The name names more than one editor: this gate exists because dropping
    // the second one is invisible until a user of it reads the row.
    expect(name.split(" and ").length).toBeGreaterThan(1);
  });

  it("is spelled the same way everywhere MemQL OS names it", () => {
    const name = productName();
    const wrong: string[] = [];
    for (const file of sourceFiles(OS_SRC)) {
      for (const [phrase] of readFileSync(file, "utf8").matchAll(PRODUCT_PHRASE)) {
        if (!phrase.trim().startsWith(name)) {
          wrong.push(`${relative(REPO, file)}: ${JSON.stringify(phrase.trim())}`);
        }
      }
    }
    expect(
      wrong,
      `MemQL OS names the editor extension by a spelling the extension does not use.\n` +
        `Its own README and package.json call it ${JSON.stringify(name)}.\n` +
        wrong.join("\n"),
    ).toEqual([]);
  });

  it("is named at all, so the gate above is not passing on an empty set", () => {
    const named = sourceFiles(OS_SRC).filter((file) => PRODUCT_PHRASE.test(readFileSync(file, "utf8")));
    PRODUCT_PHRASE.lastIndex = 0;
    expect(named.length).toBeGreaterThan(0);
  });
});
