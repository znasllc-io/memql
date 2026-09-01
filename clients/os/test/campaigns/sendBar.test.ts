import { describe, expect, it } from "vitest";

import {
  campaignFromRow,
  sendBreakdown,
  sendBreakdownLabel,
} from "../../src/apps/campaigns/rows";
import { campaignRow } from "./harness";

// The send bar's model, tested as arithmetic.
//
// The BAND is what somebody reads, and what it says is entirely decided here:
// four proportions that must sum to the whole, a derived pending slice, and a
// label that states the same figures in words for a reader who cannot see it.

function breakdownOf(over: Parameters<typeof campaignRow>[0]) {
  return sendBreakdown(campaignFromRow(campaignRow(over)));
}

function shareOf(breakdown: ReturnType<typeof sendBreakdown>, key: string): number {
  return breakdown.segments.find((s) => s.key === key)?.share ?? -1;
}

function countOf(breakdown: ReturnType<typeof sendBreakdown>, key: string): number {
  return breakdown.segments.find((s) => s.key === key)?.count ?? -1;
}

describe("the send bar's partition", () => {
  it("divides the audience into the four outcomes, in reading order", () => {
    const breakdown = breakdownOf({
      id: "c1",
      recipientCount: 100,
      sentCount: 40,
      skippedCount: 10,
      failedCount: 5,
    });
    expect(breakdown.segments.map((s) => s.key)).toEqual([
      "sent",
      "skipped",
      "failed",
      "pending",
    ]);
    expect(breakdown.total).toBe(100);
    expect(countOf(breakdown, "pending")).toBe(45);
  });

  it("DERIVES pending rather than reading it, so the slices always sum to the whole", () => {
    // There is no `pendingCount` on the row and there should not be: pending is
    // the audience minus everything that has happened, which is the arithmetic
    // the bar is drawing. Deriving it means the band can never render a gap
    // that looks like a rendering fault.
    const breakdown = breakdownOf({
      id: "c1",
      recipientCount: 7,
      sentCount: 2,
      skippedCount: 1,
      failedCount: 1,
    });
    const sum = breakdown.segments.reduce((n, s) => n + s.count, 0);
    expect(sum).toBe(breakdown.total);
    const shares = breakdown.segments.reduce((n, s) => n + s.share, 0);
    expect(shares).toBeCloseTo(1, 10);
  });

  it("MAKES THE SKIPPED SLICE VISIBLE as its own proportion", () => {
    // The compliance-relevant figure nobody goes looking for. A row of four
    // stat cards makes a person add them up to find it; a band shows it.
    const breakdown = breakdownOf({
      id: "c1",
      recipientCount: 200,
      sentCount: 150,
      skippedCount: 50,
    });
    expect(shareOf(breakdown, "skipped")).toBeCloseTo(0.25, 10);
  });

  it("CLAMPS when the counters run past the frozen total", () => {
    // A resumed send whose roster shrank mid-run would otherwise produce a
    // negative pending slice and a band longer than its container. The
    // observations win over the frozen estimate.
    const breakdown = breakdownOf({
      id: "c1",
      recipientCount: 10,
      sentCount: 12,
      skippedCount: 1,
    });
    expect(breakdown.total).toBe(13);
    expect(countOf(breakdown, "pending")).toBe(0);
    expect(breakdown.segments.every((s) => s.count >= 0)).toBe(true);
  });

  it("is EMPTY on a draft rather than a band that is 100% pending", () => {
    // A bar drawn wholly as "not yet sent" claims a size the campaign does not
    // know: recipientCount is zero until the preflight freezes it.
    const breakdown = breakdownOf({ id: "c1", status: "draft" });
    expect(breakdown.empty).toBe(true);
    expect(breakdown.total).toBe(0);
    expect(breakdown.segments.every((s) => s.share === 0)).toBe(true);
  });

  it("is NOT empty for a send with no roster figure but real outcomes", () => {
    // A send whose recipientCount never landed still has work behind it, and
    // drawing nothing would lose it.
    const breakdown = breakdownOf({ id: "c1", recipientCount: 0, sentCount: 3 });
    expect(breakdown.empty).toBe(false);
    expect(breakdown.total).toBe(3);
    expect(shareOf(breakdown, "sent")).toBe(1);
  });

  it("fills as a send progresses without changing the total", () => {
    const early = breakdownOf({ id: "c1", recipientCount: 100, sentCount: 10 });
    const later = breakdownOf({ id: "c1", recipientCount: 100, sentCount: 90 });
    expect(early.total).toBe(later.total);
    expect(shareOf(early, "sent")).toBeCloseTo(0.1, 10);
    expect(shareOf(later, "sent")).toBeCloseTo(0.9, 10);
  });
});

describe("the bar in words", () => {
  it("states every figure, in the order the band draws them", () => {
    // A bar a screen reader cannot read is a bar that excluded somebody, and
    // the picture's whole content is proportion.
    const label = sendBreakdownLabel(
      breakdownOf({ id: "c1", recipientCount: 100, sentCount: 40, skippedCount: 10, failedCount: 5 }),
    );
    expect(label).toBe("100 recipients: 40 sent, 10 skipped, 5 failed, 45 not yet sent.");
  });

  it("NAMES a slice at zero rather than dropping it", () => {
    // "No failures" is the reading somebody wants; an omitted slice is silence
    // about it.
    const label = sendBreakdownLabel(breakdownOf({ id: "c1", recipientCount: 10, sentCount: 10 }));
    expect(label).toContain("0 failed");
    expect(label).toContain("0 skipped");
  });

  it("says nothing has been sent for an empty bar", () => {
    expect(sendBreakdownLabel(breakdownOf({ id: "c1" }))).toBe("Nothing has been sent yet.");
  });
});
