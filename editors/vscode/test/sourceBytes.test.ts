// No source file in this package may contain a raw NUL byte (memql#4422).
//
// WHY THIS IS WORTH A TEST. A single 0x00 in a `.ts` file makes the whole file
// BINARY to the standard toolchain, and every tool then fails silently rather
// than loudly:
//
//   * `grep` skips it. No match, no "binary file matches" line, exit 1 -- so a
//     search for a symbol it defines returns nothing and reads as "this is
//     referenced nowhere".
//   * `file(1)` reports "data".
//   * `git diff` shows "Binary files a/… and b/… differ" instead of the diff,
//     so a one-line change to it is unreviewable.
//
// THIS IS NOT HYPOTHETICAL AND IT WAS NOT CHEAP. `src/webview/runPanel.ts`
// carried two of them in panelKey()'s template literal. The survey behind this
// epic's design record used grep, grep skipped that file, and the record --
// brainstormed, approved and committed -- states "all six webview panels".
// There are SEVEN files and NINE panel classes. The wrong count then
// propagated into four issue bodies before anyone re-derived it. A design
// record is read as settled fact by everyone downstream, so a silent skip can
// become a specification error nobody thinks to check.
//
// `src/run/preflight.ts` and `test/runLog.test.ts` carried one each, found by
// this sweep's first run. Same cause every time: a NUL written as a RAW BYTE
// where an escape was meant.
//
// THE FIX IS ALWAYS BEHAVIOUR-PRESERVING. Write the escape:
//
//   `${a}\u0000${b}`   not   `${a}<0x00>${b}`
//
// Same string, same key, same comparisons -- and the file stays text. There is
// no legitimate reason for a raw NUL in TypeScript source, which is what makes
// a blanket ban the right shape here rather than an allow-list.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

// dist-test/test/<name>.js, so the package root is two levels up.
const ROOT = path.resolve(__dirname, "..", "..");

/** The trees that ship or drive this extension. */
const SCANNED = ["src", "test", "test-host", "scripts"];

/** Every source file under a directory, as raw bytes. */
function walk(dir: string, out: { file: string; bytes: Buffer }[]): void {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(full, out);
    } else if (/\.(ts|mjs|js|json)$/.test(entry.name)) {
      // readFileSync WITHOUT an encoding, so nothing decodes and nothing can
      // be lost on the way in -- the bytes are the subject here.
      out.push({ file: path.relative(ROOT, full), bytes: fs.readFileSync(full) });
    }
  }
}

function sources(): { file: string; bytes: Buffer }[] {
  const out: { file: string; bytes: Buffer }[] = [];
  for (const dir of SCANNED) walk(path.join(ROOT, dir), out);
  return out;
}

test("the sweep reaches every source tree", () => {
  // The reachable positive. The assertion below is "nothing was found", which
  // a sweep over an empty list satisfies perfectly -- so first establish that
  // there is something to look at, and that each named tree contributed.
  const files = sources();
  assert.ok(files.length > 200, `only ${files.length} files scanned; the walk is not reaching the tree`);
  for (const dir of SCANNED) {
    assert.ok(
      files.some((f) => f.file.startsWith(`${dir}${path.sep}`)),
      `nothing scanned under ${dir}/ -- the walk missed a whole tree`,
    );
  }
});

test("no source file contains a raw NUL byte", () => {
  const offenders: string[] = [];
  for (const { file, bytes } of sources()) {
    const at = bytes.indexOf(0);
    if (at >= 0) {
      const line = bytes.subarray(0, at).toString("latin1").split("\n").length;
      const count = bytes.filter((b) => b === 0).length;
      offenders.push(`${file}:${line} (${count} NUL byte${count === 1 ? "" : "s"})`);
    }
  }
  assert.deepEqual(
    offenders,
    [],
    'a raw NUL makes the file binary: grep skips it in silence, `file` says "data", and git shows "Binary files differ" instead of the diff. Write \\u0000 instead -- same string, same behaviour, and the file stays greppable.',
  );
});

test("the detector finds a NUL when there is one", () => {
  // Proves the assertion above discriminates. `Buffer.indexOf(0)` answering -1
  // over a clean tree and >= 0 here is the whole mechanism; without this, a
  // detector that always answered -1 would report a clean tree forever.
  //
  // THIS CASE WALKED INTO ITS OWN SUBJECT, which is worth recording. Its first
  // draft wrote the fixture with what was meant to be an escape and landed the
  // RAW BYTE instead -- so the file introducing the ban was briefly the last
  // file in the tree still violating it, and the sweep above is what caught
  // it. The two lines below now show both halves: the escape TypeScript
  // interprets (one NUL in the resulting STRING, none in this file), and the
  // escaped escape (six literal characters, no NUL anywhere).
  const withNul = Buffer.from("const key = `a\u0000b`;\n", "latin1");
  assert.equal(withNul.indexOf(0), 14);
  assert.equal(Buffer.from("const key = `a\\u0000b`;\n", "latin1").indexOf(0), -1);
});
