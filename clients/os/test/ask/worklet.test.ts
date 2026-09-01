import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { CAPTURE_BLOCK_FRAMES } from "../../src/ask/pcm16";
import { PCM16_PROCESSOR_NAME, PCM16_WORKLET_PATH } from "../../src/ask/micCapture";

// THE WORKLET CANNOT IMPORT ITS OWN CONSTANTS, so this reads them back out.
//
// public/pcm16-worklet.js is copied verbatim into the bundle root, outside the
// module graph and the typechecker -- deliberately, because the edge answers
// `script-src 'self'` and a blob: worklet URL is refused, so the module has to
// be a real same-origin file. The cost is that three values live in two files
// with no compiler between them, and every one of them fails SILENTLY:
//
//  - a renamed processor makes `new AudioWorkletNode(ctx, name)` throw at the
//    moment somebody presses the mic, and nowhere else;
//  - a moved or renamed file 404s inside addModule, which surfaces as "the
//    microphone could not be opened";
//  - a changed block size does not fail at all. It changes the chunk cadence
//    and nothing reports it.
//
// Nothing else in the suite touches this file: `npm test` never loads
// main.tsx, jsdom has no audio stack, and `vite build` copies public/ without
// reading it. So this is the only place the two halves are compared.

const root = join(dirname(fileURLToPath(import.meta.url)), "../..");
const worklet = readFileSync(join(root, "public", PCM16_WORKLET_PATH), "utf8");

describe("the pcm16 worklet", () => {
  it("registers the processor micCapture asks for", () => {
    expect(worklet).toContain(`registerProcessor("${PCM16_PROCESSOR_NAME}"`);
  });

  it("posts blocks of the size pcm16.ts documents", () => {
    const declared = /var BLOCK_FRAMES = (\d+);/.exec(worklet);
    expect(declared, "BLOCK_FRAMES is not declared where this test can read it").not.toBeNull();
    expect(Number(declared![1])).toBe(CAPTURE_BLOCK_FRAMES);
  });

  it("stays a module the CSP can load: no imports, no bundler syntax", () => {
    // `script-src 'self'` allows the file; a bare `import` in it would make
    // the browser fetch a second module the bundler never emitted.
    expect(worklet).not.toMatch(/^\s*import\s/m);
    expect(worklet).not.toMatch(/\bexport\b/);
  });

  it("keeps returning true when there is no input yet", () => {
    // Returning false there ends the processor permanently, and a microphone
    // that produces its first quantum a beat late would come up dead.
    expect(worklet).toContain("if (!channel) return true;");
  });
});
