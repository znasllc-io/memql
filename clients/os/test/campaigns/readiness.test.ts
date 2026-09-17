import { describe, expect, it } from "vitest";
import { campaignSendingConfigured } from "../../src/apps/campaigns/readiness";
import { readiness, verdict } from "../setup/harness";

describe("campaign sending readiness", () => {
  it("requires fresh affirmative transport and unsubscribe setup", () => {
    expect(campaignSendingConfigured(undefined)).toBe(false);
    expect(campaignSendingConfigured(readiness(false, []))).toBe(false);
    expect(campaignSendingConfigured(readiness(true, [verdict("email", "configured")]))).toBe(false);
    expect(campaignSendingConfigured(readiness(true, [verdict("email", "partial"), verdict("campaigns", "configured")]))).toBe(false);
    const configured = readiness(true, [verdict("email", "configured"), verdict("campaigns", "configured")]);
    expect(campaignSendingConfigured(configured)).toBe(true);
    expect(campaignSendingConfigured({ ...configured, state: "disconnected" })).toBe(false);
  });
});
