// An undelivered invitation must not look like a delivered one (memql#4587).
//
// The row says `status: pending` whether the invitee got the link or the
// operator closed the panel and forgot. There was no error, no log line an
// operator would read, and no red status anywhere -- it looked exactly like
// success, which is how memql#4583 went unnoticed until the owner asked the
// colleague in person.

import { describe, expect, it } from "vitest";

import {
  EXPIRING_SOON_HOURS,
  deliveryBadge,
  invitationAdvice,
  invitationVerdict,
  readDeliveryState,
} from "../src/people/invitationState";

const NOW = Date.parse("2026-08-25T12:00:00Z");
const hoursOut = (h: number): string => new Date(NOW + h * 3_600_000).toISOString();

function verdict(over: Partial<{ deliveryState: string; deliveryError: string; expiresAt: string }> = {}) {
  return invitationVerdict(
    { deliveryState: "sent", deliveryError: "", expiresAt: hoursOut(7 * 24), ...over },
    NOW,
  );
}

describe("the delivery half", () => {
  it("distinguishes emailed from link-minted-never-sent", () => {
    // THE WHOLE POINT. These two were indistinguishable from every surface.
    const emailed = deliveryBadge(verdict({ deliveryState: "sent" }));
    const never = deliveryBadge(verdict({ deliveryState: "not_attempted" }));
    expect(emailed.label).not.toBe(never.label);
    expect(emailed.tone).toBe("ok");
    expect(never.tone).toBe("warn");
  });

  it("says a failed send is a failure, not an absence", () => {
    const v = verdict({ deliveryState: "failed", deliveryError: "550 mailbox unavailable" });
    expect(deliveryBadge(v).tone).toBe("danger");
    // The provider's own text, because a fixed token collapses "refused
    // credential", "refused sender" and "unreachable host" into one.
    expect(invitationAdvice(v)).toContain("550 mailbox unavailable");
    expect(invitationAdvice(v)).toContain("has not been told");
  });

  it("reads a row predating the record as unknown, never as not_attempted", () => {
    // Claiming "nobody tried to send this" about a row that carries nothing
    // would state, in the operator's own list, a fact nobody observed -- which
    // is the exact failure this surface exists to remove.
    expect(readDeliveryState("")).toBe("unknown");
    expect(readDeliveryState("   ")).toBe("unknown");
    expect(readDeliveryState("something-new")).toBe("unknown");

    const v = verdict({ deliveryState: "" });
    expect(v.delivery).toBe("unknown");
    expect(v.needsManualDelivery).toBe(false);
    expect(invitationAdvice(v)).toContain("not recorded");
  });

  it("only claims manual delivery is needed when it is", () => {
    expect(verdict({ deliveryState: "sent" }).needsManualDelivery).toBe(false);
    expect(verdict({ deliveryState: "failed" }).needsManualDelivery).toBe(true);
    expect(verdict({ deliveryState: "not_attempted" }).needsManualDelivery).toBe(true);
    expect(verdict({ deliveryState: "" }).needsManualDelivery).toBe(false);
  });
});

describe("the expiry half", () => {
  it("surfaces an invitation nearing expiry with no redemption", () => {
    const soon = verdict({ expiresAt: hoursOut(EXPIRING_SOON_HOURS - 1) });
    expect(soon.expiringSoon).toBe(true);
    expect(soon.expired).toBe(false);
    expect(invitationAdvice(soon)).toContain("has not been accepted");
  });

  it("stays quiet on a healthy invitation", () => {
    // A list that annotates every row teaches people to skim past the
    // annotations, and then the one that matters is skimmed past too.
    const healthy = verdict();
    expect(healthy.expiringSoon).toBe(false);
    expect(invitationAdvice(healthy)).toBe("");
  });

  it("names an expired invitation and points at the remedy", () => {
    const dead = verdict({ expiresAt: hoursOut(-1) });
    expect(dead.expired).toBe(true);
    expect(dead.expiringSoon).toBe(false);
    expect(invitationAdvice(dead)).toContain("Re-send");
  });

  it("does not invent a verdict from an unparseable expiry", () => {
    const v = verdict({ expiresAt: "not a date" });
    expect(v.expired).toBe(false);
    expect(v.expiringSoon).toBe(false);
  });

  it("reports both problems when both hold", () => {
    const v = verdict({ deliveryState: "not_attempted", expiresAt: hoursOut(3) });
    const advice = invitationAdvice(v);
    expect(advice).toContain("No email was sent");
    expect(advice).toContain("expires in");
  });
});
