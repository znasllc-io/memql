// Inviting somebody into the cluster, from the Users view (memql#4270,
// memql#4272).
//
// The behaviour that matters here is the COPY: "invite" means four different
// things depending on the registration mode, and an operator who does not know
// which one they are doing will misread the result. Under `open` the link is a
// convenience; under `domain_restricted` the server refuses an address outside
// the allowlist.
//
// The mode is read to WORD the dialog and is never the check -- the gate in
// component/identity/adminops applies the policy and refuses independently.

import { describe, expect, it } from "vitest";

import { modeStatement } from "../src/people/InvitePerson";

describe("what an invitation MEANS under each registration mode", () => {
  it("says the link is the only way in, under invite_only", () => {
    const s = modeStatement("invite_only", "");
    expect(s).toMatch(/only way in/i);
  });

  it("names the domains an address must be at, under domain_restricted", () => {
    const s = modeStatement("domain_restricted", "example.com, second.test");
    expect(s).toContain("example.com, second.test");
    // And says what happens to an address that is not -- the refusal is the
    // server's, and an operator should not discover it by clicking.
    expect(s).toMatch(/refused/i);
  });

  it("says an invitation skips the queue, under waitlist", () => {
    expect(modeStatement("waitlist", "")).toMatch(/without waiting|request access/i);
  });

  it("is honest that the link is a convenience, under open", () => {
    const s = modeStatement("open", "");
    expect(s).toMatch(/convenience|rather than a gate/i);
    // The one thing this must NOT say under open is that it controls access.
    expect(s).not.toMatch(/only way in/i);
  });

  it("says nothing rather than guessing, for an unknown mode", () => {
    // A cluster running a mode this build does not know about gets no
    // sentence, not an invented one. Silence is honest; a wrong claim about
    // who may register is not.
    expect(modeStatement("", "")).toBe("");
    expect(modeStatement("something_new", "")).toBe("");
  });
});
