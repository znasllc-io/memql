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

import { deliveryHeading, deliveryStatement, modeStatement } from "../src/people/InvitePerson";

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

// memql#4584 / memql#4585. Issuing an invitation used to mint a link and send
// nothing, while the button said "Send the invitation" -- so an operator
// believed a message had gone, never delivered the link, and the invitee
// waited for an email that did not exist. Now a send happens, and the panel
// has to say which of three things actually did.
describe("what the panel says happened to the invitation email", () => {
  it("names the recipient when the email went, so the operator can stop", () => {
    expect(deliveryHeading(true, "invitee@example.test")).toBe("Invitation sent to invitee@example.test");
    const s = deliveryStatement(true, "", "invitee@example.test");
    expect(s).toMatch(/went to them by email/i);
    expect(s).toMatch(/do not need to send it yourself/i);
  });

  it("still says sent when the address is unknown, rather than inventing one", () => {
    expect(deliveryHeading(true, "")).toBe("Invitation sent");
  });

  it("says nothing was emailed when no mail is wired, and asks for delivery", () => {
    const s = deliveryStatement(false, "", "invitee@example.test");
    expect(s).toMatch(/no mail configured/i);
    expect(s).toMatch(/send it to them yourself/i);
    // A configuration statement, NOT an incident: it must not read as a fault.
    expect(s).not.toMatch(/could not be delivered|failed/i);
  });

  it("distinguishes a FAILED send from an unconfigured one", () => {
    const failed = deliveryStatement(false, "graph: 503 unavailable", "invitee@example.test");
    expect(failed).toMatch(/could not be delivered/i);
    expect(failed).toMatch(/has not been told/i);
    // The two must not read alike -- one is "this cluster does not send mail",
    // the other is "mail is broken right now", and they send an operator to
    // different places.
    expect(failed).not.toBe(deliveryStatement(false, "", "invitee@example.test"));
  });

  it("heads both undelivered cases the same way: copy the link", () => {
    // Whatever the reason, the operator's next action is identical.
    expect(deliveryHeading(false, "invitee@example.test")).toMatch(/copy this link now/i);
    expect(deliveryHeading(false, "")).toMatch(/nothing was emailed/i);
  });

  it("never claims the invitation failed -- it did not", () => {
    // The row is written and the link admits somebody in every one of these
    // states. A panel that said "invitation failed" over a mail fault would
    // invite the operator to reissue and strand a live credential.
    for (const s of [
      deliveryStatement(true, "", "a@b.io"),
      deliveryStatement(false, "", "a@b.io"),
      deliveryStatement(false, "graph: 503", "a@b.io"),
    ]) {
      expect(s).not.toMatch(/invitation failed|not issued|could not be issued/i);
    }
  });
});
