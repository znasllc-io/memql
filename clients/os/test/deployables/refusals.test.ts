import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

import { SERVER_SENTENCE_ONLY, copyFor, knownCodes } from "../../src/apps/deployables/packages/refusals";

// Every refusal code the engine can emit has a home in this build: copy in
// the table, or an explicit listing as "the server's sentence is the whole
// copy". Neither home is guessed at render time -- an unknown code renders
// under a neutral heading -- so the ONLY way a code arrives unnamed is that
// somebody added it engine-side and nobody told the OS. This reads the engine
// to find out.
//
// TWO SOURCES, because the catalogue is not the whole truth. refusal.go owns
// the named constants, but the pipeline also spells four codes inline at the
// site that raises them (deploy_failed, deployable_build_failed,
// deployable_publish_failed, archive_confirmation_mismatch). A test that read
// the constants alone would have passed while those four rendered as "This
// cluster refused".

const packagesDir = join(dirname(fileURLToPath(import.meta.url)), "../../../../component/packages");
const cataloguePath = join(packagesDir, "refusal.go");

/** The `CodeX = "..."` constants the catalogue declares. */
function cataloguedCodes(): string[] {
  const source = readFileSync(cataloguePath, "utf8");
  const out = new Set<string>();
  for (const m of source.matchAll(/^\s*Code\w+\s*=\s*"([a-z_]+)"/gm)) out.add(m[1]!);
  return [...out].sort();
}

/** Every code literal spelled inline at a raise site, across the package's
 *  non-test files: `refuse("x", ...)`, `refuseScoped("x", ...)` and a
 *  `Code: "x"` field on a Problem or Refusal literal. */
function inlineCodes(): string[] {
  const out = new Set<string>();
  for (const name of readdirSync(packagesDir)) {
    if (!name.endsWith(".go") || name.endsWith("_test.go")) continue;
    const source = readFileSync(join(packagesDir, name), "utf8");
    for (const m of source.matchAll(/\b(?:refuse|refuseScoped)\(\s*"([a-z_]+)"/g)) out.add(m[1]!);
    for (const m of source.matchAll(/\bCode:\s*"([a-z_]+)"/g)) out.add(m[1]!);
  }
  return [...out].sort();
}

function covered(code: string): boolean {
  return knownCodes().includes(code) || SERVER_SENTENCE_ONLY.includes(code);
}

describe("refusal copy coverage", () => {
  it("reads the engine's catalogue from the tree, and fails rather than skips when it is gone", () => {
    // A missing file is a FAILURE. A skip here would make the whole gate
    // silently vacuous the moment the catalogue moved, which is exactly when
    // the two sides are most likely to have diverged.
    expect(existsSync(cataloguePath), `${cataloguePath} is not where this test expects the catalogue`).toBe(true);
    expect(cataloguedCodes().length).toBeGreaterThan(0);
  });

  it("still finds the inline codes the pipeline spells at its raise sites", () => {
    // The reachable positive for the inline scan: these four are known to be
    // spelled inline today. If the scan stops seeing them the regexes have
    // drifted from the Go, not the Go from the OS.
    const inline = inlineCodes();
    for (const code of ["deploy_failed", "deployable_build_failed", "deployable_publish_failed", "archive_confirmation_mismatch"]) {
      expect(inline, `the inline scan no longer finds ${code}`).toContain(code);
    }
  });

  it("has copy, or an explicit server-sentence listing, for every code the engine emits", () => {
    const missing = [...new Set([...cataloguedCodes(), ...inlineCodes()])].filter((code) => !covered(code));
    expect(
      missing.join(", "),
      "each of these codes needs an entry in refusals.ts's COPY, or a line in SERVER_SENTENCE_ONLY saying the server's sentence is the whole copy",
    ).toBe("");
  });

  it("does not cover a code nobody emits", () => {
    // Without this the assertion above could be satisfied by a `covered` that
    // always answers true.
    expect(covered("made_up_code_nobody_emits")).toBe(false);
    expect(copyFor("made_up_code_nobody_emits")).toBeNull();
  });

  it("gives every code exactly one home", () => {
    // A code in both places is a code whose copy would be rendered in one
    // surface and withheld in another, depending on which list was consulted.
    for (const code of SERVER_SENTENCE_ONLY) {
      expect(knownCodes(), `${code} is listed as server-sentence-only AND has copy`).not.toContain(code);
    }
  });

  it("carries the compose epic's four new codes", () => {
    // Pinned by name because the engine half lands in its own task: the OS
    // copy must be ready before the first credential_not_found reaches a
    // browser, not after.
    expect(copyFor("credential_not_found")?.title).toBe("This source's credential is not one you can use");
    expect(copyFor("credential_not_found")?.next).toContain("Source stop");
    expect(copyFor("credential_revoked")?.title).toBe("This source's credential was revoked");
    expect(copyFor("credential_revoked")?.next).toContain("Source stop");
    expect(copyFor("source_host_unsupported")?.title).toBe("Only github.com today");
    expect(copyFor("source_host_unsupported")?.next).toContain("github.com");
    expect(copyFor("source_host_unsupported")?.next).toContain("zip");
    expect(copyFor("deployable_target_not_offered")?.title).toBe("That kind is not offered on this cluster yet");
    // The server's sentence names the kind ("iOS is not offered on this
    // cluster yet"), and nothing this build could add would help.
    expect(copyFor("deployable_target_not_offered")?.next).toBe("");
  });

  it("says the DEPLOY SUCCEEDED for the two placement halves", () => {
    // The pipeline applies the account and the domain AFTER the publish and
    // records a refusal on the outcome without failing the run
    // (component/packages/stages.go). Copy that read as a failed deploy would
    // send somebody looking for a site that is already serving, so both
    // headlines say it is live and both next steps name the stop that fixes
    // the half that was refused.
    for (const code of ["deployable_account_refused", "deployable_domain_refused"]) {
      const copy = copyFor(code);
      expect(copy, `${code} has no copy`).not.toBeNull();
      expect(copy?.title, `${code} does not say it is live`).toContain("live");
      expect(copy?.next, `${code} does not name the stop that repairs it`).toContain("Where it lives");
    }
  });
});
