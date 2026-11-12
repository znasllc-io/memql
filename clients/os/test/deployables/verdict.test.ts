import { describe, expect, it } from "vitest";

import { readStoreVerdict, verdictWord } from "../../src/apps/deployables/store/verdict";
import { builtinReply, storeVerdictRow, verdictReasonRow } from "./harness";

// THE VERDICT AS THE STORE PAGE READS IT (U7). The reply is read as it arrives
// on the wire -- a builtin's node map -- and nothing in it is reworded: the
// sentences are the engine's, so what is checked here is the reading alone.

function read(row: Record<string, unknown>) {
  return readStoreVerdict(builtinReply("shopifyStoreVerdict", [row as never]).rows());
}

describe("reading a store's verdict", () => {
  it("keeps an absent time absent", () => {
    const verdict = read(storeVerdictRow({
      storeId: "acme-widgets",
      // Go formats a zero time.Time as the year 1; that is no time at all.
      since: "0001-01-01T00:00:00Z",
      resumedAt: "",
      recorded: [
        { kind: "orders", lastChangeAt: "", lastCheckedAt: "2026-11-11T09:05:00Z" },
        { kind: "catalog", lastChangeAt: "2026-11-11T09:00:00Z" },
      ],
    }));
    expect(verdict).not.toBeNull();
    expect(verdict!.since).toBeNull();
    expect(verdict!.resumedAt).toBeNull();
    expect(verdict!.connectedAt).toBe("2026-11-01T08:00:00Z");
    expect(verdict!.recorded).toEqual([
      { kind: "orders", lastChangeAt: null, lastCheckedAt: "2026-11-11T09:05:00Z" },
      { kind: "catalog", lastChangeAt: "2026-11-11T09:00:00Z", lastCheckedAt: null },
    ]);
    expect(verdict!.ordersFrom).toBeNull();
  });

  it("answers null for a reply that names no state", () => {
    expect(read({ status: "unconfigured" })).toBeNull();
    expect(read(storeVerdictRow({ storeId: "acme-widgets", state: "" }))).toBeNull();
    expect(read(storeVerdictRow({ storeId: "acme-widgets", state: "humming" }))).toBeNull();
    expect(readStoreVerdict([])).toBeNull();
  });

  it("reads each reason, what is recorded, the alert and the caller's acts as they arrive", () => {
    const verdict = read(storeVerdictRow({
      storeId: "acme-widgets",
      state: "needs attention",
      reasons: [verdictReasonRow({ code: "changes_not_recorded", sentence: "At least 3 changes could not be recorded.", count: 3, atLeast: true, act: "retry", actLabel: "Try again" })],
      outside: ["abandoned checkouts"],
      withheld: ["product variants: delivery profile"],
      ordersFrom: "2026-08-13",
      alert: { state: "failed", reason: "the mail server refused the message" },
      may: { act: false },
    }));
    expect(verdict!.state).toBe("needs attention");
    expect(verdict!.reasons).toEqual([{ code: "changes_not_recorded", domain: "", sentence: "At least 3 changes could not be recorded.", detail: "", count: 3, atLeast: true, act: "retry", actLabel: "Try again", actUrl: "" }]);
    expect(verdict!.outside).toEqual(["abandoned checkouts"]);
    expect(verdict!.withheld).toEqual(["product variants: delivery profile"]);
    expect(verdict!.ordersFrom).toBe("2026-08-13");
    expect(verdict!.alert).toEqual({ state: "failed", reason: "the mail server refused the message" });
    expect(verdict!.may.act).toBe(false);
  });

  it("has no word for a store that is not connected", () => {
    expect(verdictWord("not connected")).toBe("");
    expect(verdictWord("recording")).toBe("Recording");
    expect(verdictWord("starting")).toBe("Starting");
    expect(verdictWord("paused")).toBe("Paused");
    expect(verdictWord("needs attention")).toBe("Needs attention");
  });
});
