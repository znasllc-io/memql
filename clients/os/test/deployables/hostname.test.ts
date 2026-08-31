import { describe, expect, it } from "vitest";

import {
  RESERVED_LABELS,
  SLUG_MAX_LENGTH,
  hostnameFor,
  validateSlug,
} from "../../src/apps/deployables/hostname";

// The browser's half of the hostname policy. The server is the authority; this
// is what makes "api is reserved" something somebody learns before the submit.

const DOMAIN = "memql.example.com";

describe("composing the hostname", () => {
  it("puts the name one label under the cluster's domain", () => {
    expect(hostnameFor("shop", DOMAIN)).toBe("shop.memql.example.com");
  });

  it("folds case and trims", () => {
    expect(hostnameFor("  Shop ", " MemQL.Example.COM ")).toBe("shop.memql.example.com");
  });

  it("previews nothing when either half is missing", () => {
    expect(hostnameFor("", DOMAIN)).toBe("");
    expect(hostnameFor("shop", "")).toBe("");
  });
});

describe("validating a name", () => {
  it("accepts an ordinary one", () => {
    expect(validateSlug("shop", DOMAIN)).toBe("");
    expect(validateSlug("my-shop-2", DOMAIN)).toBe("");
  });

  it("says nothing about an empty field", () => {
    // A form somebody has not typed into is not a form with an error in it.
    expect(validateSlug("", DOMAIN)).toBe("");
    expect(validateSlug("   ", DOMAIN)).toBe("");
  });

  it("REFUSES EVERY RESERVED LABEL, and names the set", () => {
    for (const label of RESERVED_LABELS) {
      const message = validateSlug(label, DOMAIN);
      expect(message, `${label} was accepted`).not.toBe("");
      expect(message).toContain("reserved");
      expect(message).toContain(`${label}.${DOMAIN}`);
    }
  });

  it("does not refuse a name that merely CONTAINS a reserved label", () => {
    // The reserved set is a set of whole labels: `apiary` is not `api`.
    expect(validateSlug("apiary", DOMAIN)).toBe("");
    expect(validateSlug("mailroom", DOMAIN)).toBe("");
  });

  it("explains the case rule rather than saying invalid", () => {
    expect(validateSlug("Shop", DOMAIN)).toContain("lowercase");
  });

  it("bounds the length in both directions and says which", () => {
    expect(validateSlug("ab", DOMAIN)).toContain("3 to 40");
    expect(validateSlug("a".repeat(SLUG_MAX_LENGTH + 1), DOMAIN)).toContain("3 to 40");
    expect(validateSlug("a".repeat(SLUG_MAX_LENGTH), DOMAIN)).toBe("");
  });

  it("refuses a dotted name, and says why a wildcard cannot route it", () => {
    expect(validateSlug("shop.eu", DOMAIN)).toContain("One label only");
  });

  it("refuses characters DNS would take but a handle should not", () => {
    expect(validateSlug("shop_eu", DOMAIN)).toContain("lowercase letters, digits and hyphens");
  });

  it("still names the rule when the cluster's domain is unknown", () => {
    const message = validateSlug("api", "");
    expect(message).toContain("reserved");
    expect(message).not.toContain("undefined");
  });
});
