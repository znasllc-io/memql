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

// -----------------------------------------------------------------------------
// THE SAME RULE OVER src/auth/store.ts, WITH A NAMED EXEMPTION LIST
// -----------------------------------------------------------------------------
//
// memql#3529 was this defect again, one module over: `runAuthenticated` --
// refresh once, retry once, then clear the tokens -- was exported, unit-tested,
// and imported by nothing. Everything that dialed with a bearer handled a 401
// however it happened to, which for the connection manager meant reporting the
// cluster as unreachable.
//
// That issue asked whether this guard could be widened to store.ts too. It can,
// but NOT as the blanket rule deviceCodeUi.ts gets, and the comment above says
// why: store.ts is a library as well as an adapter, and legitimately exports
// helpers that only its own unit tests drive. A rule that flagged those would
// be noise, and noise is how a guard stops being read.
//
// So the rule is "imported, or on this list" -- and the list is the point. Each
// entry is a deliberate statement that a function is reachable-by-test-only, and
// adding one is a decision somebody makes on purpose rather than a silence
// nobody notices. A capability that arrives WITHOUT being wired now has to be
// named here to go green, which is exactly the moment to ask whether it should
// have been wired instead.
const STORE_TEST_ONLY_EXPORTS = new Set<string>([
  // The two SecretStorage key-derivation helpers. They exist so a test can
  // assert the exact key a credential lands under -- which is the thing #3404's
  // index has to agree with, and the one detail no behavioural test can show.
  // Nothing outside this module should be composing these keys itself.
  "refreshTokenSecretKey",
  "accessTokenExpirySecretKey",

  // The two halves of runAuthenticated's decision, exported so they can be
  // driven directly rather than through a dial.
  //
  // `isUnauthorizedError` is a TEXT match over an error with no status field
  // (a refused WebSocket upgrade surfaces through `ws` as a plain
  // `Error: Unexpected server response: 401`). Being deliberately generous is
  // the risk in it, so the table of what it does and does not match is asserted
  // on the predicate itself, not inferred from six dial fixtures.
  //
  // `reauthenticationMessage` is the sentence an operator reads. It reaches
  // them as `AuthenticatedRunResult.message`, so there is no second caller to
  // wire -- what the test pins is the wording.
  "isUnauthorizedError",
  "reauthenticationMessage",
]);

test("every function src/auth/store.ts exports is imported, or is named test-only", () => {
  const store = path.join(SRC, "auth", "store.ts");
  const imported = new Set<string>();
  for (const file of sourceFiles(SRC)) {
    if (path.normalize(file) === path.normalize(store)) continue;
    for (const entry of namedImports(file)) imported.add(entry);
  }

  const unreachable = exportedFunctions(store).filter(
    (name) =>
      !imported.has(`${path.normalize(store)}::${name}`) && !STORE_TEST_ONLY_EXPORTS.has(name),
  );
  assert.deepEqual(
    unreachable,
    [],
    "exported from src/auth/store.ts and imported by nothing: either wire it at " +
      "the call sites that need it, or -- if it genuinely exists only for its own " +
      "unit tests -- add it to STORE_TEST_ONLY_EXPORTS above and say why.",
  );
});

test("the test-only list does not name a function that is in fact wired", () => {
  // A stale exemption is worse than none: it re-opens the hole the list exists
  // to keep shut, silently, for whatever gets added under that name next.
  const store = path.join(SRC, "auth", "store.ts");
  const imported = new Set<string>();
  for (const file of sourceFiles(SRC)) {
    if (path.normalize(file) === path.normalize(store)) continue;
    for (const entry of namedImports(file)) imported.add(entry);
  }

  const stale = [...STORE_TEST_ONLY_EXPORTS].filter((name) =>
    imported.has(`${path.normalize(store)}::${name}`),
  );
  assert.deepEqual(
    stale,
    [],
    "listed as test-only but actually imported by src/: drop it from " +
      "STORE_TEST_ONLY_EXPORTS so the guard covers it again.",
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
