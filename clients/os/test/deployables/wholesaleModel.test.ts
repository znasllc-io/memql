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
    expect(accessSentence("customer_refused", "Email has already been taken")).toBe("Shopify refused: Email has already been taken");
    expect(accessSentence("shopify_error", "Throttled")).toBe("Shopify refused: Throttled");
  });

  it("has no label for an act it does not know", () => {
    expect(actLabel("reject")).toBe("Reject");
    expect(actLabel("approve")).toBe("Approve");
    expect(actLabel("remove")).toBe("Remove");
    expect(actLabel("retryAccess")).toBe("Retry access");
    expect(actLabel("resendTerms")).toBeNull();
    expect(actLabel("resendApproved")).toBeNull();
    expect(actLabel("toString")).toBeNull();
  });
});
