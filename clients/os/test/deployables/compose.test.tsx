import { describe, it } from "vitest";

// The compose flow (epic memql#4885, task memql#4891): the five stops as
// INPUTS -- the probes the Source stop runs, the credential picker, the
// placements the Where-it-lives stop asks for, and the two clicks a first
// deploy takes (design D5).
//
// ===========================================================================
// WHAT IS PARKED HERE, AND WHY IT IS NOT DELETED
// ===========================================================================
// The confirm gate (`packages/ConfirmGate.tsx`) retired with the section
// restructure (memql#4889): the gate's report is the What-it-is stop, and
// its hostname picker is the Where-it-lives stop. The gate's own assertions
// are still true statements about the compose flow -- they were only ever
// about the surface that asks for an address on a first deploy -- so they are
// parked here with their EXACT former titles rather than deleted, and
// memql#4891 turns them on where its stops answer them.
//
// One case is deliberately NOT parked. "never handles a token value -- the
// secret field takes a NAME" was about `NewPackage.tsx`'s secret-name field,
// and that field is gone: a package names a `v1:platform:sourceCredential`
// now (memql#4886), and what a browser holds is the CARD -- a label, a host
// and a fingerprint, with no type that could carry a value. The assertion
// that replaced it lives in `deployables.test.tsx` ("the credential card"),
// which pins the projection itself rather than one form's label.
//
// The retired file's own reasoning, kept because the compose stops inherit
// it: a hostname is chosen at a deployable's FIRST deploy and remembered on
// its site row, so the address is asked for only where it has never been
// answered -- which is the difference between a gate that protects somebody
// and a gate they learn to click past.

describe("the compose flow: where each app will live", () => {
  it.todo("asks for a hostname only on a deployable's first deploy, and previews it");

  it.todo("refuses a reserved name client-side, exactly as the server would");

  it.todo("blocks the deploy until it is confirmed, and shows the report first");
});
