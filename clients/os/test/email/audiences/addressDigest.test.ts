import { describe, expect, it } from "vitest";
import { addressDigest, normalizeAddress } from "../../../src/apps/email/audiences/addressDigest";

// The expected values are what the engine's campaigns.EmailDigest returns for
// the same input (component/campaigns/digest.go). If one of these changes, the
// stored digest format changed and the duplicate check no longer finds people.
describe("the address digest matches the engine's", () => {
  it.each([
    ["person@example.test", "143b53b0840f6e1fcf8537a38513b59a9f326c72f577f17d1da67393eff1d413"],
    ["  Alice.Smith@Example.COM ", "7dcd3a39ad3a8d2145645ec612ed4f6fa3f297b47bdcf7e0aeb76040f5e24e89"],
    ["a+news@x.org", "e1a9017af1cf95cdd976d2886139171f3a1cbbf1cfaf1bda9cbaf33b8a298620"],
    ["\u00e9@Ex.com", "11518873f354634bb410ea3bc9e60a42be5b3a9b042aeaa0dd11c68c61591526"],
  ])("digests %j", async (address, expected) => {
    expect(await addressDigest(address)).toBe(expected);
  });
  it.each(["bad address@x.com", "@x.com", "a@", "plain", "", "a@b\tc"])("refuses %j as the engine does", async address => {
    expect(normalizeAddress(address)).toBe("");
    expect(await addressDigest(address)).toBe("");
  });
});
