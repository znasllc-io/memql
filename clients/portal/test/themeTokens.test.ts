// EVERY COLOR CLASS THE PORTAL USES MUST RESOLVE TO A REAL TOKEN.
//
// Found while adding the local-models surfaces (epic memql#4676): twenty
// occurrences of `text-fg-muted` across nine files. The theme defines
// `--color-muted` and `--color-subtle`; it has never defined `--color-fg-muted`.
// So every one of those elements had NO styling from that class -- rendered in
// the inherited foreground colour, at full contrast, where the author meant
// secondary.
//
// IT WAS INVISIBLE TO EVERYTHING WE RUN. Tailwind does not fail on an unknown
// utility, it simply emits nothing. The typechecker sees a string. jsdom has
// no stylesheet at all, so every component test rendered the class and asserted
// nothing about its effect. The only instrument that could have caught it was a
// human looking at the pixels -- and the difference between "muted" and
// "default" is exactly the size that survives a glance.
//
// So the gate reads the SOURCE OF TRUTH -- brand/theme.css's --color-* set --
// and checks it against every color utility in the portal's TSX.

import { describe, expect, it } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

// Vite rewrites import.meta.url to an /@fs/ URL under the test runner, so the
// paths are resolved from the module's own directory rather than from it.
const HERE = dirname(fileURLToPath(import.meta.url));
const SRC = resolve(HERE, "..", "src");
const THEME = resolve(HERE, "..", "..", "..", "brand", "theme.css");

// Tailwind's own palette and keywords, which are valid without a --color-*
// token of ours. Numeric scales (gray-500) are matched separately.
const BUILT_IN = new Set([
  "white", "black", "transparent", "current", "inherit", "none", "auto",
]);

const NUMERIC_SCALE = /-\d{2,3}$/;

// The utilities whose value is a COLOR. Deliberately not every utility: a
// `text-xs` or a `border-2` is a size, and folding those in would need an
// allowlist of sizes that grows forever.
const COLOR_UTILITIES = ["text", "bg", "border", "ring", "fill", "stroke", "decoration", "outline", "shadow", "from", "via", "to", "accent", "caret", "divide", "placeholder"];

function definedColorTokens(): Set<string> {
  const css = readFileSync(THEME, "utf8");
  const out = new Set<string>();
  for (const match of css.matchAll(/--color-([a-z0-9-]+)\s*:/g)) {
    out.add(match[1]!);
  }
  return out;
}

function tsxFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      out.push(...tsxFiles(full));
      continue;
    }
    if (entry.endsWith(".tsx") || entry.endsWith(".ts")) out.push(full);
  }
  return out;
}

// classCandidates pulls plausible utility tokens out of a source file. It
// looks only inside className-ish string literals, so a prose sentence
// containing "text-book" is not mistaken for a utility.
function classCandidates(source: string): string[] {
  const out: string[] = [];
  for (const match of source.matchAll(/className=(?:"([^"]*)"|\{`([^`]*)`\}|\{"([^"]*)"\})/g)) {
    const body = match[1] ?? match[2] ?? match[3] ?? "";
    for (const raw of body.split(/[\s`${}()?:]+/)) {
      const token = raw.trim();
      if (token !== "") out.push(token);
    }
  }
  // The UI kit builds class strings in Record<> maps rather than on the
  // element, so those are scanned too -- they are where a tone's colour
  // actually lives.
  for (const match of source.matchAll(/"([a-z-]*(?:bg|text|border|ring)-[a-z0-9-]+(?:\s+[a-z0-9:/[\]-]+)*)"/g)) {
    for (const raw of match[1]!.split(/\s+/)) {
      if (raw.trim() !== "") out.push(raw.trim());
    }
  }
  return out;
}

// colorNameOf returns the token a color utility references, or null when the
// class is not a color utility at all.
function colorNameOf(cls: string): string | null {
  // Strip variants (hover:, dark:, md:) and any leading negation.
  const bare = cls.split(":").pop() ?? cls;
  for (const util of COLOR_UTILITIES) {
    if (!bare.startsWith(util + "-")) continue;
    const value = bare.slice(util.length + 1);
    // Arbitrary values (text-[var(--x)]) and opacity suffixes are out of
    // scope: they name their own source.
    if (value.startsWith("[")) return null;
    return value.split("/")[0] ?? null;
  }
  return null;
}

describe("the portal's color classes", () => {
  it("all resolve to a token brand/theme.css actually defines", () => {
    const defined = definedColorTokens();
    expect(defined.size).toBeGreaterThan(10);

    const offenders = new Map<string, string[]>();
    for (const file of tsxFiles(SRC)) {
      const source = readFileSync(file, "utf8");
      for (const cls of classCandidates(source)) {
        const name = colorNameOf(cls);
        if (name === null || name === "") continue;
        if (BUILT_IN.has(name) || NUMERIC_SCALE.test(name)) continue;
        if (defined.has(name)) continue;
        // A prefix of a defined token is a size or a keyword we do not model
        // (text-sm, border-t). Only flag names that LOOK like a colour: they
        // share a stem with something the theme defines.
        const looksLikeAColour = [...defined].some(
          (token) => token.startsWith(name.split("-")[0] + "-") || name.startsWith(token + "-"),
        );
        if (!looksLikeAColour) continue;

        const rel = file.slice(SRC.length + 1);
        offenders.set(cls, [...(offenders.get(cls) ?? []), rel]);
      }
    }

    if (offenders.size > 0) {
      const detail = [...offenders.entries()]
        .map(([cls, files]) => `  ${cls}  (${[...new Set(files)].join(", ")})`)
        .join("\n");
      throw new Error(
        `These color utilities reference no --color-* token in brand/theme.css, so they emit ` +
          `NOTHING -- Tailwind does not fail on an unknown utility, the typechecker sees a ` +
          `string, and jsdom has no stylesheet, so the element renders unstyled and every test ` +
          `passes:\n${detail}\n\n` +
          `Fix by using a token the theme defines, or by adding one to brand/theme.css -- which ` +
          `is shared with component/identity/web and is the one place both consumers read.`,
      );
    }
  });
});
