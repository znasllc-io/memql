import { describe, expect, it } from "vitest";

import { accessSentence, actLabel, stageWords } from "../../src/apps/deployables/wholesale/model";

// THE WHOLESALE SECTION'S WORDS (apps/deployables/wholesale/model.ts): what a
// stage, an access failure and an act of the engine's are called on screen.

describe("the wholesale words", () => {
  it("calls a revoked application Removed", () => {
    expect(stageWords("revoked")).toBe("Removed");
    expect(stageWords("readyToReview")).toBe("Ready to review");
    expect(stageWords("approved")).toBe("Approved");
    expect(stageWords("rejected")).toBe("Rejected");
    expect(stageWords("waitingForTerms")).toBe("Waiting for terms");
    expect(stageWords("all")).toBe("All");
  });

  it("gives each error code its sentence", () => {
    expect(accessSentence("no_route", "")).toBe("No access route is chosen in Settings.");
    expect(accessSentence("no_settings", "")).toBe("This store has no wholesale settings.");
    expect(accessSentence("scope_missing", "")).toBe(
      "The store has not allowed MemQL to change customers. Its owner approves the new permissions from the store's admin.",
    );
    expect(accessSentence("plan_ceiling", "")).toBe("The store's plan allows no more catalogs.");
    expect(accessSentence("customer_refused", "Email has already been taken")).toBe("Email has already been taken");
    expect(accessSentence("shopify_error", "Throttled")).toBe("Throttled");
  });

  it("has no label for an act it does not know", () => {
    expect(actLabel("reject")).toBe("Reject");
    expect(actLabel("approve")).toBe("Approve");
    expect(actLabel("remove")).toBe("Remove");
    expect(actLabel("retryAccess")).toBe("Retry access");
    expect(actLabel("resendTerms")).toBe("Resend terms email");
    expect(actLabel("resendApproved")).toBe("Resend approval email");
    expect(actLabel("resendRejected")).toBe("Resend rejection email");
    expect(actLabel("resendRevoked")).toBe("Resend removal email");
    expect(actLabel("sendFlowers")).toBeNull();
    expect(actLabel("toString")).toBeNull();
  });
});

// FIX ROUND 1: what an email's line says, one line per attempt; a buyer's
// access in the engine's own sentence; and what the settings and the lists
// answer read as they arrive.

import { addBuyerWords, buyerAccessWords, emailLines, emailWords, wholesaleBuyersFrom, wholesalePageFrom, wholesaleSettingsFrom, dayWords, type WholesaleEmail } from "../../src/apps/deployables/wholesale/model";

const at = "2026-11-04T08:00:00Z";
const email = (over: Partial<WholesaleEmail>): WholesaleEmail => ({ kind: "terms", attempt: 1, state: "sent", reason: "", sentAt: at, delivery: null, ...over });
const delivered = (providerStatus: string) => ({ status: "sent", skipReason: "", providerStatus, sentAt: at });

describe("an email's line", () => {
  const day = dayWords(at);

  it("says on its way for a deferred attempt, with the lane's sentence when it gives one", () => {
    expect(emailWords(email({ state: "deferred", sentAt: "", reason: "" }))).toBe("On its way");
    expect(emailWords(email({ state: "deferred", sentAt: "", reason: "A lane sentence." }))).toBe("On its way: A lane sentence.");
  });

  it("says sent while the provider has not reported", () => {
    expect(emailWords(email({}))).toBe(`Sent on ${day}`);
    expect(emailWords(email({ delivery: delivered("") }))).toBe(`Sent on ${day}`);
  });

  it("names each of the lane's seven provider reports in the ruled words", () => {
    expect(emailWords(email({ delivery: delivered("delivered") }))).toBe(`Sent on ${day}; delivered`);
    expect(emailWords(email({ delivery: delivered("expanded") }))).toBe(`Sent on ${day}; passed on to a mailing list`);
    expect(emailWords(email({ delivery: delivered("bounced") }))).toBe("Bounced: the address does not accept mail");
    expect(emailWords(email({ delivery: delivered("suppressed") }))).toBe("Not delivered: the provider no longer sends to this address");
    expect(emailWords(email({ delivery: delivered("quarantined") }))).toBe(`Sent on ${day}; the recipient's mail system quarantined it`);
    expect(emailWords(email({ delivery: delivered("filtered-spam") }))).toBe(`Sent on ${day}; the recipient's mail system filtered it as spam`);
    expect(emailWords(email({ delivery: delivered("failed") }))).toBe(`Sent on ${day}; the provider could not deliver it`);
  });

  it("guesses nothing for a provider report it does not know", () => {
    for (const status of ["delayed", "complained", "opened"]) {
      expect(emailWords(email({ delivery: delivered(status) }))).toBe(`Sent on ${day}`);
    }
  });

  it("says not sent and the engine's sentence for a failed, held or skipped attempt", () => {
    for (const state of ["failed", "held", "skipped"]) {
      expect(emailWords(email({ state, sentAt: "", reason: "The engine's sentence." }))).toBe("Not sent: The engine's sentence.");
    }
  });

  it("says the engine's sentence alone for an uncertain attempt", () => {
    expect(emailWords(email({ state: "uncertain", sentAt: "", reason: "It may have been delivered." }))).toBe("It may have been delivered.");
  });

  it("says Not confirmed, and nothing else, for an uncertain attempt the engine gave no sentence", () => {
    expect(emailWords(email({ state: "uncertain", sentAt: "", reason: "" }))).toBe("Not confirmed");
    expect(emailWords(email({ state: "uncertain", sentAt: "", reason: "  " }))).toBe("Not confirmed");
  });

  it("draws one line per attempt, each kind's newest first", () => {
    const lines = emailLines([
      email({ kind: "approved", attempt: 1 }),
      email({ kind: "terms", attempt: 1 }),
      email({ kind: "terms", attempt: 2 }),
    ]);
    expect(lines.map((l) => [l.kind, l.email.attempt])).toEqual([["terms", 2], ["terms", 1], ["approved", 1]]);
  });
});

describe("what the engine answers, read as it arrives", () => {
  it("puts the engine's own sentence after Not on yet, and nothing of its own", () => {
    const [failed, refused, bare] = wholesaleBuyersFrom({ rows: [
      { applicationId: "a", companyName: "A", applicantEmail: "a@example.com", accessState: "failed", errorCode: "plan_ceiling", error: "The engine's reason.", grantedAt: "" },
      { applicationId: "b", companyName: "B", applicantEmail: "b@example.com", accessState: "failed", errorCode: "shopify_error", error: "Another reason.", grantedAt: "" },
      { applicationId: "c", companyName: "C", applicantEmail: "c@example.com", accessState: "failed", errorCode: "plan_ceiling", error: "", grantedAt: "" },
    ] } as never).rows;
    expect(buyerAccessWords(failed!)).toBe("Not on yet: the engine's reason.");
    expect(buyerAccessWords(refused!)).toBe("Not on yet: another reason.");
    expect(buyerAccessWords(bare!)).toBe("Not on yet");
  });

  it("reads operator from the list and the buyers", () => {
    expect(wholesalePageFrom({ rows: [], operator: true } as never).operator).toBe(true);
    expect(wholesalePageFrom({ rows: [] } as never).operator).toBe(false);
    expect(wholesaleBuyersFrom({ rows: [], operator: true } as never).operator).toBe(true);
  });

  it("reads the sender, the emails that would be sent, and who may publish and export", () => {
    const s = wholesaleSettingsFrom({
      exists: true,
      sender: { ready: true, fromName: "Shop", address: "shop@example.com", reason: "" },
      emailsWouldSend: { terms: { wouldSend: false, reason: "Why not." }, approved: { wouldSend: true, reason: "" } },
      may: { update: true, setUp: false, setSender: false, publish: true, export: false },
    } as never);
    expect(s.sender).toEqual({ ready: true, fromName: "Shop", address: "shop@example.com", reason: "" });
    expect(s.emailsWouldSend.terms).toEqual({ wouldSend: false, reason: "Why not." });
    expect(s.emailsWouldSend.approved).toEqual({ wouldSend: true, reason: "" });
    // A kind the engine did not answer is not sent.
    expect(s.emailsWouldSend.rejected.wouldSend).toBe(false);
    expect(s.may.publish).toBe(true);
    expect(s.may.export).toBe(false);
  });
});

describe("what adding a buyer says it will do", () => {
  const settings = (termsRequired: boolean, terms: { wouldSend: boolean; reason: string }) =>
    wholesaleSettingsFrom({ exists: true, termsRequired, emailsWouldSend: { terms } } as never);

  it("says the terms are emailed when they would be", () => {
    expect(addBuyerWords(settings(true, { wouldSend: true, reason: "" }))).toEqual({
      lead: "They will be emailed the terms. They can buy at trade prices once they accept and someone approves.",
      act: "Add and send the terms",
    });
  });

  it("gives the engine's reason when the terms email would not be sent", () => {
    expect(addBuyerWords(settings(true, { wouldSend: false, reason: "The engine's reason." }))).toEqual({
      lead: "The engine's reason. Nothing is sent to them yet. They can buy at trade prices once they accept the terms and someone approves.",
      act: "Add buyer",
    });
  });

  it("says only the approval when the store requires no terms", () => {
    expect(addBuyerWords(settings(false, { wouldSend: false, reason: "terms are not required on this store" }))).toEqual({
      lead: "They can buy at trade prices once someone approves.",
      act: "Add buyer",
    });
  });
});
