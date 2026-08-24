// The two rules the deployables surface holds in the browser: the hostname
// policy it mirrors, and the publish refusals it translates.
//
// WHAT THIS FILE IS AND IS NOT ABOUT. Neither of these is a gate. The hostname
// policy is enforced in Go on every write
// (component/memql/platform_site_hostname_policy.go, with its own db-backed
// tests) and `sitePublishFromArtifact` refuses server-side by name
// (integrations/library/site_publish.go). What is asserted here is the half
// that only exists in the browser: an answer while somebody is still typing,
// and a sentence instead of a token.
//
// EVERY CASE IS WRITTEN TO FAIL ON THE VACUOUS PASS. "validateSlug rejects
// something" is trivially true of a function that rejects everything, so each
// refusal case is paired with an acceptance that a blunter rule would also
// have refused -- and the refusal-text cases assert the token is ABSENT from
// what a person reads, which is the actual requirement.

import { describe, expect, it } from "vitest";

import {
  RESERVED_LABELS,
  SLUG_MAX_LENGTH,
  SLUG_MIN_LENGTH,
  hostnameFor,
  validateSlug,
} from "../src/deployables/hostname";
import {
  PUBLISH_REFUSALS,
  describePublishFailure,
  publishRefusalReason,
} from "../src/deployables/publishRefusal";

const DOMAIN = "memql.localhost";

describe("hostnameFor", () => {
  it("composes ONE label under the cluster's domain", () => {
    expect(hostnameFor("shop", DOMAIN)).toBe("shop.memql.localhost");
  });

  it("lowercases both halves, because a hostname is case-folded", () => {
    expect(hostnameFor("Shop", "MemQL.Localhost")).toBe("shop.memql.localhost");
  });

  it("returns empty rather than a half-built hostname when the domain is unknown", () => {
    // The serving node publishes `domain` in runtime-config.json; an older one
    // does not. "shop." is a hostname nothing serves, and the form must say so
    // rather than compose it.
    expect(hostnameFor("shop", "")).toBe("");
    expect(hostnameFor("", DOMAIN)).toBe("");
  });
});

describe("validateSlug", () => {
  it("accepts an ordinary name", () => {
    expect(validateSlug("shop", DOMAIN)).toBe("");
    expect(validateSlug("my-shop-2", DOMAIN)).toBe("");
  });

  it("says nothing about an empty field -- that is untouched, not wrong", () => {
    expect(validateSlug("", DOMAIN)).toBe("");
    expect(validateSlug("   ", DOMAIN)).toBe("");
  });

  it("refuses uppercase, and says why rather than just refusing", () => {
    const problem = validateSlug("Shop", DOMAIN);
    expect(problem).not.toBe("");
    expect(problem).toMatch(/lowercase/i);
  });

  it("refuses a name shorter than the minimum and longer than the maximum, and accepts both bounds", () => {
    expect(validateSlug("a".repeat(SLUG_MIN_LENGTH - 1), DOMAIN)).not.toBe("");
    expect(validateSlug("a".repeat(SLUG_MAX_LENGTH + 1), DOMAIN)).not.toBe("");
    // The boundaries themselves are legal. A rule tested only from outside its
    // range would pass just as well if it were off by one.
    expect(validateSlug("a".repeat(SLUG_MIN_LENGTH), DOMAIN)).toBe("");
    expect(validateSlug("a".repeat(SLUG_MAX_LENGTH), DOMAIN)).toBe("");
  });

  it("refuses a name with a dot in it, because an Ingress wildcard matches exactly one label", () => {
    const problem = validateSlug("shop.eu", DOMAIN);
    expect(problem).not.toBe("");
    expect(problem).toMatch(/one label/i);
  });

  it("refuses characters DNS would allow in a label but this policy does not", () => {
    expect(validateSlug("shop_eu", DOMAIN)).not.toBe("");
    expect(validateSlug("shop eu", DOMAIN)).not.toBe("");
  });

  // The property that matters: EVERY reserved label is refused, not merely the
  // one somebody thought to write a case for. A list-driven assertion cannot
  // drift out of step with the list the form actually uses.
  it("refuses every reserved label, and names the whole set so the person can see it", () => {
    for (const label of RESERVED_LABELS) {
      const problem = validateSlug(label, DOMAIN);
      expect(problem, `expected ${label} to be refused`).not.toBe("");
      expect(problem).toContain(label);
    }
    // The front-door roles are the reason the set exists at all.
    expect(RESERVED_LABELS).toContain("api");
    expect(RESERVED_LABELS).toContain("identity");
    expect(RESERVED_LABELS).toContain("mcp");
    expect(RESERVED_LABELS).toContain("portal");
  });

  it("does not refuse a name that merely CONTAINS a reserved label", () => {
    // "api" is reserved; "api-docs" is not. A substring test would take both,
    // which is a rule nobody wrote and one that would quietly deny valid names.
    expect(validateSlug("api-docs", DOMAIN)).toBe("");
    expect(validateSlug("my-portal", DOMAIN)).toBe("");
  });
});

describe("publishRefusalReason", () => {
  it("finds the reason inside the message the SDK actually delivers", () => {
    // The real shape: the capability's Error(), wrapped by executeNamed's
    // "<call name>: " prefix.
    const wire =
      "sitePublishFromArtifact: sitePublishFromArtifact refused: bundle_missing_index -- " +
      "bundle for site-shop has no index.html at its root";
    expect(publishRefusalReason(wire)).toBe("bundle_missing_index");
  });

  it("tells apart two reasons that share a prefix", () => {
    expect(publishRefusalReason("refused: artifact_not_a_zip -- x")).toBe("artifact_not_a_zip");
    expect(publishRefusalReason("refused: artifact_not_a_file -- x")).toBe("artifact_not_a_file");
    expect(publishRefusalReason("refused: file_not_found -- x")).toBe("file_not_found");
    expect(publishRefusalReason("refused: artifact_not_found -- x")).toBe("artifact_not_found");
  });

  it("reports no reason for a failure that is not a refusal", () => {
    expect(publishRefusalReason("stream closed")).toBe("");
    expect(publishRefusalReason("")).toBe("");
  });
});

describe("describePublishFailure", () => {
  // THE requirement, stated as a test: the person never reads the token. A
  // mapping that returned the reason verbatim would satisfy "shows something"
  // and fail this.
  it("renders a sentence and never the raw reason token", () => {
    for (const reason of Object.keys(PUBLISH_REFUSALS)) {
      const text = describePublishFailure(
        new Error(`sitePublishFromArtifact: sitePublishFromArtifact refused: ${reason} -- detail`),
      );
      expect(text, `${reason} produced no text`).not.toBe("");
      expect(text, `${reason} leaked its token`).not.toContain(reason);
      // Prose, not an identifier: every sentence ends in a full stop.
      expect(text.trim().endsWith(".")).toBe(true);
    }
  });

  it("says something actionable for the two refusals a person is most likely to hit", () => {
    expect(
      describePublishFailure(new Error("refused: bundle_missing_index -- no index.html")),
    ).toMatch(/index\.html/);
    expect(describePublishFailure(new Error("refused: artifact_not_a_zip -- application/pdf"))).toMatch(
      /zip/i,
    );
  });

  // A transport failure is NOT a refusal, and dressing one up as user error is
  // how a real fault gets ignored.
  it("passes an unrecognised failure through with its own message", () => {
    expect(describePublishFailure(new Error("the stream closed"))).toBe("the stream closed");
    expect(describePublishFailure("plain string failure")).toBe("plain string failure");
  });
});
