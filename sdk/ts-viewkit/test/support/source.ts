// Reading view-kit's own source, for the tests that check the source rather
// than the behaviour.
//
// Two of them need it -- the anti-drift stylesheet guard (every class emitted
// has a rule) and the genericity guard (no concept id in a renderer) -- and
// both need the same distinction: what is CODE and what is a COMMENT. A doc
// comment naming a class or a concept is documentation; a string literal is
// an emission. One shared reader means the two cannot disagree about which is
// which.

import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

// Compiled tests live at dist-test/test/support/, so src/ is four levels up.
export const SRC_DIR = path.resolve(fileURLToPath(import.meta.url), "../../../../src");

export interface SourceFile {
  readonly file: string;
  readonly source: string;
}

export function sourceFiles(exclude: readonly string[] = []): SourceFile[] {
  return fs
    .readdirSync(SRC_DIR)
    .filter((file) => file.endsWith(".ts") && !exclude.includes(file))
    .map((file) => ({ file, source: fs.readFileSync(path.join(SRC_DIR, file), "utf8") }));
}

// stripComments removes line and block comments while KEEPING string literal
// contents. A hand-rolled scanner rather than a regex: `//` inside a string
// and a quote inside a comment each defeat the regex version, and a guard
// that can be fooled by either is worth nothing.
export function stripComments(source: string): string {
  let out = "";
  let i = 0;
  let quote: string | undefined;
  while (i < source.length) {
    const ch = source[i];
    const next = source[i + 1];
    if (quote) {
      out += ch;
      if (ch === "\\") {
        out += next ?? "";
        i += 2;
        continue;
      }
      if (ch === quote) quote = undefined;
      i += 1;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      out += ch;
      i += 1;
      continue;
    }
    if (ch === "/" && next === "/") {
      while (i < source.length && source[i] !== "\n") i += 1;
      continue;
    }
    if (ch === "/" && next === "*") {
      i += 2;
      while (i < source.length && !(source[i] === "*" && source[i + 1] === "/")) i += 1;
      i += 2;
      continue;
    }
    out += ch;
    i += 1;
  }
  return out;
}
