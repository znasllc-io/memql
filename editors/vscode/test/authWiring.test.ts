// The wiring of the sign-in surface, asserted over the SOURCE (memql#3515).
//
// -----------------------------------------------------------------------------
// WHY A STRUCTURAL TEST AND NOT A BEHAVIOURAL ONE
// -----------------------------------------------------------------------------
//
// The defect this file exists for was not a wrong answer -- every function
// involved was correct and tested. `src/auth/deviceCodeUi.ts` exported a
// sign-in that falls back to the device grant when the host cannot do loopback,
// and NOTHING IMPORTED IT. What `memQL: Sign In` actually ran was a second
// function of the same name, private to `src/extension.ts`, that ran loopback
// alone. A host that genuinely cannot open a browser sat out the callback
// deadline and was told it failed, with the code to hand it a device code
// sitting one directory away, green under `node --test`.
//
// No behavioural test catches that, because the behaviour under test is which
// function the command calls -- and both functions pass their own tests. The
// two rules below are what "reachable" means, spelled out:
//
//   EVERY EXPORT OF THE UI ADAPTER IS IMPORTED. deviceCodeUi.ts is an adapter,
//   not a library: every function it exports exists to be driven by a command.
//   An unimported one is by definition unreachable, which is the defect exactly.
//   (This rule is deliberately NOT applied to the rest of src/auth: those
//   modules do export functions for their own unit tests -- `clampInterval`,
//   `codeChallengeS256` -- and a rule that flagged those would be noise.)
//
//   ONE FUNCTION, ONE NAME. Two `signInToCluster`s meant nothing at either call
//   site said which one won, and a reader who found the exported one first
//   concluded the fallback shipped. It did not.
//
// The scan is over TEXT, which is enough for both rules: an import clause is
// unambiguous, and a `function NAME` declaration is what a reader greps for.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

// These cases read the SOURCE tree, and they run from the BUNDLE
// (dist-test/test, see esbuild.test.js). Walking up to the directory that
// actually carries src/auth/deviceCodeUi.ts pins the one thing this file needs
// without hard-coding how deep the bundle happens to sit or what the working
// directory is when `node --test` runs.
function extensionRoot(): string {
  let dir = __dirname;
  for (;;) {
    if (fs.existsSync(path.join(dir, "src", "auth", "deviceCodeUi.ts"))) return dir;
    const parent = path.dirname(dir);
    if (parent === dir) throw new Error("could not locate the extension source tree");
    dir = parent;
  }
}

const SRC = path.join(extensionRoot(), "src");

function sourceFiles(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return sourceFiles(full);
    return entry.name.endsWith(".ts") ? [full] : [];
  });
}

/** Every named import in a file, as `resolved/target.ts::symbol`. */
function namedImports(file: string): Set<string> {
  const source = fs.readFileSync(file, "utf8");
  const clause = /import\s+(?:type\s+)?\{([^}]*)\}\s+from\s+["']([^"']+)["']/g;
  const out = new Set<string>();
  for (const match of source.matchAll(clause)) {
    const specifier = match[2];
    if (!specifier.startsWith(".")) continue;
    const target = path.normalize(
      path.join(path.dirname(file), specifier.replace(/\.js$/, ".ts"))
    );
    for (const raw of match[1].split(",")) {
      const name = raw.trim().replace(/^type\s+/, "").split(/\s+as\s+/)[0].trim();
      if (name !== "") out.add(`${target}::${name}`);
    }
  }
  return out;
}

function exportedFunctions(file: string): string[] {
  const source = fs.readFileSync(file, "utf8");
  return [...source.matchAll(/^export\s+(?:async\s+)?function\s+([A-Za-z0-9_]+)/gm)].map(
    (m) => m[1]
  );
}

test("every function the sign-in UI adapter exports is imported by something", () => {
  const adapter = path.join(SRC, "auth", "deviceCodeUi.ts");
  const imported = new Set<string>();
  for (const file of sourceFiles(SRC)) {
    if (path.normalize(file) === path.normalize(adapter)) continue;
    for (const entry of namedImports(file)) imported.add(entry);
  }

  const unreachable = exportedFunctions(adapter).filter(
    (name) => !imported.has(`${path.normalize(adapter)}::${name}`)
  );
  assert.deepEqual(
    unreachable,
    [],
    "exported from src/auth/deviceCodeUi.ts and imported by nothing: a capability " +
      "that cannot be reached from any command. Wire it, or delete it."
  );
});

test("no two modules declare a function named signInToCluster", () => {
  const declaring = sourceFiles(SRC).filter((file) =>
    /(?:^|\s)(?:export\s+)?(?:async\s+)?function\s+signInToCluster\b/m.test(
      fs.readFileSync(file, "utf8")
    )
  );

  assert.equal(
    declaring.length,
    1,
    `signInToCluster is declared in ${declaring.length} modules (${declaring
      .map((f) => path.relative(SRC, f))
      .join(", ")}). Two functions of one name means nothing at a call site says ` +
      "which one runs, and one of them is dead."
  );
});
