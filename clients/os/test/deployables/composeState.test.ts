import { describe, expect, it } from "vitest";

import {
  EMPTY_ADDRESS,
  EMPTY_DRAFT,
  addressReady,
  appsToPlace,
  movingStopFor,
  pathOf,
  phaseOf,
  placementsComplete,
  placementsFrom,
  seedAddress,
  sourceReady,
  suggestName,
  type AddressDraft,
  type ComposeDraft,
} from "../../src/apps/deployables/page/compose";
import { placementsPayload } from "../../src/apps/deployables/packages/calls";
import {
  EMPTY_MANIFEST,
  PROBE_REASON_CODES,
  branchNamesFrom,
  manifestFrom,
  manifestIsEmpty,
  probeNote,
  probeParks,
  probeWantsCredential,
  zipUnusableNote,
  zipVerdict,
  type SourceProbeReply,
} from "../../src/apps/deployables/sources/probe";

// The compose flow's PURE half (epic memql#4885, task memql#4891).
//
// "Deploy is out of reach until every app has a valid address", "a definite
// answer about the repository parks the flow" and "a package's kinds come
// from its manifest" are statements about these two modules, not about a
// picture of them -- the same split rail.ts and list.ts keep, and for the
// same reason: a rule asserted through render() is asserted through three
// layers that can each fail for unrelated reasons.

const DOMAIN = "memql.example.com";

function draft(over: Partial<ComposeDraft> = {}): ComposeDraft {
  return { ...EMPTY_DRAFT, ...over };
}

function address(over: Partial<AddressDraft> = {}): AddressDraft {
  return { ...EMPTY_ADDRESS, ...over };
}

function reply(over: Partial<SourceProbeReply> = {}): SourceProbeReply {
  // `branches` and `manifest` are the two keys a GRANT makes answerable
  // (memql#4915). Both are always present on the wire and empty when there is
  // nothing to say, so the default here is what a pasted-token probe answers.
  return {
    host: "github.com",
    reachable: true,
    private: false,
    defaultBranch: "main",
    reason: "ok",
    branches: [],
    manifest: { ...EMPTY_MANIFEST },
    ...over,
  };
}

// ---------------------------------------------------------------------------
// The probe's answers
// ---------------------------------------------------------------------------

describe("what a probe reason is worth", () => {
  it("gives every reason the engine names a sentence", () => {
    // The reachable positive for the loop below: a reason this build has NO
    // name for renders nothing, so an all-non-empty result is evidence about
    // the table rather than about the function.
    expect(probeNote(reply({ reason: "something_new_the_engine_added" }))).toBe("");
    for (const reason of PROBE_REASON_CODES) {
      expect(probeNote(reply({ reason })), `${reason} has no sentence`).not.toBe("");
    }
  });

  it("says the design's fixed sentences, word for word", () => {
    expect(probeNote(reply())).toBe("public, default branch main");
    expect(probeNote(reply({ private: true }))).toBe(
      "private, and reachable under this credential -- default branch main",
    );
    expect(probeNote(reply({ reachable: false, reason: "not_found_or_private" }))).toBe("private, or not there");
    expect(probeNote(reply({ reachable: false, reason: "credential_cannot_see_it" }))).toBe(
      "this token cannot see it",
    );
    expect(probeNote(reply({ reachable: false, reason: "source_host_unsupported" }))).toBe(
      "only github.com today, or upload a zip",
    );
  });

  it("names the default branch as 'the default' when GitHub did not say", () => {
    expect(probeNote(reply({ defaultBranch: "" }))).toBe("public, default branch the default");
  });

  it("parks on an answer about the REPOSITORY and not on one about the probe", () => {
    // The split this module exists for (design H): the fetch is the
    // authority and the probe is a courtesy.
    for (const reason of [
      "not_found_or_private",
      "credential_cannot_see_it",
      "credential_not_found",
      "credential_revoked",
      "source_host_unsupported",
      // The two a GRANT adds (memql#4915). Each is a definite answer about
      // this repository whose repair is one click, so analyzing anyway would
      // fetch and refuse with the same information a round trip later.
      "reconnect_required",
      "repository_not_installed",
    ]) {
      expect(probeParks(reason), `${reason} must park`).toBe(true);
    }
    for (const reason of ["ok", "rate_limited", "", "something_new"]) {
      expect(probeParks(reason), `${reason} must not park`).toBe(false);
    }
    // `github_app_not_configured` is deliberately NOT a parking reason: it is
    // an operator's condition, the token path still works, and parking on it
    // would block a deploy this cluster can perform.
    expect(probeParks("github_app_not_configured")).toBe(false);
  });

  it("offers a credential only where one could change the answer", () => {
    expect(probeWantsCredential("not_found_or_private")).toBe(true);
    expect(probeWantsCredential("credential_cannot_see_it")).toBe(true);
    expect(probeWantsCredential("credential_revoked")).toBe(true);
    // No token makes github.com out of a gitlab URL, and a reachable
    // repository needs none.
    expect(probeWantsCredential("source_host_unsupported")).toBe(false);
    expect(probeWantsCredential("ok")).toBe(false);
    expect(probeWantsCredential("rate_limited")).toBe(false);
  });
});

describe("what a zip is", () => {
  it("reads BOTH at the root as a package, the way the engine does", () => {
    expect(zipVerdict({ isPackage: true, isBuiltSite: true, fileCount: 9, totalBytes: 10 })).toBe("package");
    expect(zipVerdict({ isPackage: false, isBuiltSite: true, fileCount: 9, totalBytes: 10 })).toBe("built_site");
    expect(zipVerdict({ isPackage: false, isBuiltSite: false, fileCount: 9, totalBytes: 10 })).toBe("neither");
  });

  it("reports what it counted rather than diagnosing", () => {
    const note = zipUnusableNote({ isPackage: false, isBuiltSite: false, fileCount: 1, totalBytes: 10 });
    expect(note).toContain("1 file");
    expect(note).toContain("memql-package.yaml");
    expect(note).toContain("index.html");
    // A possibility, named as one -- this build cannot tell a wrapped site
    // from an unbuilt source tree.
    expect(note).toContain("looks like this");
  });
});

// ---------------------------------------------------------------------------
// Which path, and whether it can move
// ---------------------------------------------------------------------------

describe("which path a draft is on", () => {
  it("is the package path for a repository and for a zip with a manifest", () => {
    expect(pathOf(draft({ choice: "repo" }), null)).toBe("package");
    expect(pathOf(draft({ choice: "zip" }), "package")).toBe("package");
  });

  it("is the hand-made path for a CI push and for a built-site zip", () => {
    expect(pathOf(draft({ choice: "ci" }), null)).toBe("handmade");
    expect(pathOf(draft({ choice: "zip" }), "built_site")).toBe("handmade");
  });

  it("is NO path until the zip has been probed, and none for a zip that is neither", () => {
    expect(pathOf(draft({ choice: "zip" }), null)).toBe("unknown");
    expect(pathOf(draft({ choice: "zip" }), "neither")).toBe("unknown");
    expect(pathOf(draft(), null)).toBe("unknown");
  });
});

describe("whether the source is answered", () => {
  it("wants a URL and a name for a repository, and refuses a parked probe", () => {
    expect(sourceReady(draft({ choice: "repo", repoUrl: "u", name: "n" }), null, false)).toBe(true);
    expect(sourceReady(draft({ choice: "repo", repoUrl: "", name: "n" }), null, false)).toBe(false);
    expect(sourceReady(draft({ choice: "repo", repoUrl: "u", name: "" }), null, false)).toBe(false);
    expect(sourceReady(draft({ choice: "repo", repoUrl: "u", name: "n" }), null, true)).toBe(false);
  });

  it("wants a KIND for a hand-made deployable and not for a package", () => {
    // A package declares each app's kind in its manifest and the report reads
    // it back; a built site and a CI push declare nothing.
    expect(sourceReady(draft({ choice: "zip", artifactId: "a", name: "n" }), "package", false)).toBe(true);
    expect(sourceReady(draft({ choice: "zip", artifactId: "a", name: "n" }), "built_site", false)).toBe(false);
    expect(sourceReady(draft({ choice: "zip", artifactId: "a", name: "n", kind: "spa" }), "built_site", false)).toBe(
      true,
    );
    expect(sourceReady(draft({ choice: "ci", name: "n" }), null, false)).toBe(false);
    expect(sourceReady(draft({ choice: "ci", name: "n", kind: "static" }), null, false)).toBe(true);
  });

  it("is never answered before a choice is made", () => {
    expect(sourceReady(draft(), null, false)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// The addresses
// ---------------------------------------------------------------------------

describe("whether an address is answered", () => {
  it("takes a claimable slug and refuses a reserved or malformed one", () => {
    expect(addressReady(address({ slug: "shop" }), DOMAIN)).toBe(true);
    expect(addressReady(address({ slug: "api" }), DOMAIN)).toBe(false);
    expect(addressReady(address({ slug: "no" }), DOMAIN)).toBe(false);
    expect(addressReady(address({ slug: "" }), DOMAIN)).toBe(false);
  });

  it("treats an own domain as optional, and one still being typed as not answered", () => {
    expect(addressReady(address({ slug: "shop", ownDomain: "" }), DOMAIN)).toBe(true);
    expect(addressReady(address({ slug: "shop", ownDomain: "acme.com" }), DOMAIN)).toBe(true);
    expect(addressReady(address({ slug: "shop", ownDomain: "  " }), DOMAIN)).toBe(true);
    expect(addressReady(address({ slug: "shop", ownDomain: "." }), DOMAIN)).toBe(false);
  });
});

describe("which apps need placing", () => {
  const report = {
    deployables: [
      { name: "storefront", kind: "spa", path: "a", buildPlan: "", output: "", prebuilt: true },
      { name: "reports", kind: "static", path: "b", buildPlan: "", output: "", prebuilt: true },
      {
        name: "mobile",
        kind: "ios",
        path: "c",
        buildPlan: "skipped -- not offered on this cluster yet",
        output: "",
        prebuilt: false,
        problem: { code: "deployable_target_not_offered", message: "iOS is not offered", scope: "mobile", fatal: false },
      },
    ],
  };

  it("is the one hand-made deployable, whatever the report says", () => {
    expect(appsToPlace("handmade", report)).toEqual([""]);
  });

  it("skips an app the cluster does not offer -- nothing answers at a bundle id", () => {
    expect(appsToPlace("package", report)).toEqual(["storefront", "reports"]);
  });

  it("skips an app that already has a site: a placement is read on a FIRST deploy only", () => {
    expect(appsToPlace("package", report, ["storefront"])).toEqual(["reports"]);
    expect(appsToPlace("package", report, ["storefront", "reports"])).toEqual([]);
  });

  it("is empty with no report at all", () => {
    expect(appsToPlace("package", null)).toEqual([]);
  });
});

describe("whether Deploy is reachable", () => {
  it("wants every app answered, and NO app is not an answer", () => {
    const held = { storefront: address({ slug: "shop" }), reports: address({ slug: "reports" }) };
    expect(placementsComplete(["storefront", "reports"], held, DOMAIN)).toBe(true);
    expect(placementsComplete(["storefront", "reports", "docs"], held, DOMAIN)).toBe(false);
    // A flow with nothing to place is not a flow that is ready to deploy.
    expect(placementsComplete([], held, DOMAIN)).toBe(false);
  });

  it("fails on ONE bad address out of several", () => {
    const held = { storefront: address({ slug: "shop" }), reports: address({ slug: "api" }) };
    expect(placementsComplete(["storefront", "reports"], held, DOMAIN)).toBe(false);
  });
});

describe("the wire form of a placement", () => {
  it("composes the hostname and normalizes the own domain", () => {
    const made = placementsFrom(
      ["storefront"],
      { storefront: address({ slug: "Shop".toLowerCase(), ownDomain: "Shop.Acme.COM." }) },
      DOMAIN,
    );
    expect(made["storefront"]).toEqual({
      hostname: "shop.memql.example.com",
      accountId: "",
      ownDomain: "shop.acme.com",
    });
  });

  it("OMITS a half nobody answered rather than sending an empty one", () => {
    // An explicit "" is a value the pipeline reads: an empty accountId asks
    // for a tie to nobody, an empty ownDomain for a binding to nothing.
    expect(placementsPayload({ storefront: { hostname: "shop.example.com", accountId: "", ownDomain: "" } })).toEqual({
      storefront: { hostname: "shop.example.com" },
    });
    expect(
      placementsPayload({ storefront: { hostname: "shop.example.com", accountId: "acct-1", ownDomain: "acme.com" } }),
    ).toEqual({ storefront: { hostname: "shop.example.com", accountId: "acct-1", ownDomain: "acme.com" } });
    // An entry with nothing in it is dropped whole, so a package whose apps
    // all already have addresses sends no `placements` argument at all.
    expect(placementsPayload({ storefront: { hostname: "", accountId: "", ownDomain: "" } })).toEqual({});
  });
});

// ---------------------------------------------------------------------------
// Where the flow is
// ---------------------------------------------------------------------------

describe("the phase, read off the rows", () => {
  it("follows a package run through its own statuses", () => {
    const at = (runStatus: string) => phaseOf({ path: "package", runStatus, siteId: "", published: false });
    expect(at("")).toBe("composing");
    expect(at("analyzing")).toBe("analyzing");
    expect(at("awaiting_confirm")).toBe("awaiting_confirm");
    expect(at("building")).toBe("deploying");
    expect(at("staging_dsl")).toBe("deploying");
    expect(at("rolling")).toBe("deploying");
    expect(at("publishing")).toBe("deploying");
    expect(at("succeeded")).toBe("published");
    expect(at("refused")).toBe("stopped");
    expect(at("failed")).toBe("stopped");
  });

  it("reads a status this build does not know as still composing rather than as done", () => {
    // The least-asserting reading: a phase this build cannot place must not
    // claim the flow finished.
    expect(phaseOf({ path: "package", runStatus: "some_new_stage", siteId: "", published: false })).toBe("composing");
  });

  it("follows the hand-made path through what it created", () => {
    expect(phaseOf({ path: "handmade", runStatus: "", siteId: "", published: false })).toBe("composing");
    expect(phaseOf({ path: "handmade", runStatus: "", siteId: "site-1", published: false })).toBe("awaiting_confirm");
    expect(phaseOf({ path: "handmade", runStatus: "", siteId: "site-1", published: true })).toBe("published");
  });

  it("names the stop a run is MOVING at, and nothing while nothing moves", () => {
    expect(movingStopFor("analyzing", "analyzing")).toBe("whatItIs");
    expect(movingStopFor("deploying", "building")).toBe("build");
    expect(movingStopFor("deploying", "staging_dsl")).toBe("live");
    expect(movingStopFor("deploying", "publishing")).toBe("live");
    expect(movingStopFor("awaiting_confirm", "awaiting_confirm")).toBeNull();
    expect(movingStopFor("published", "succeeded")).toBeNull();
    expect(movingStopFor("composing", "")).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// The suggestions
// ---------------------------------------------------------------------------

describe("the suggestions", () => {
  it("names a repository source after its repository", () => {
    expect(suggestName(draft({ choice: "repo", repoUrl: "https://github.com/acme/storefront" }), "")).toBe(
      "storefront",
    );
    expect(suggestName(draft({ choice: "repo", repoUrl: "https://github.com/acme/storefront.git" }), "")).toBe(
      "storefront",
    );
  });

  it("names a zip source after the zip, without the extension", () => {
    expect(suggestName(draft({ choice: "zip" }), "storefront-build.zip")).toBe("storefront-build");
  });

  it("suggests nothing for a CI push, which has no source to read a name off", () => {
    expect(suggestName(draft({ choice: "ci" }), "")).toBe("");
  });

  it("seeds an address from the app's own name", () => {
    expect(seedAddress("acme", "storefront")).toEqual({ slug: "storefront", accountId: "", ownDomain: "" });
    // ...and falls back to the source when the app has no usable name.
    expect(seedAddress("acme", "").slug).toBe("acme");
  });
});

// ---------------------------------------------------------------------------
// The two keys a grant adds to the probe's reply (epic memql#4915)
// ---------------------------------------------------------------------------

describe("reading the branches and the manifest a probe answered", () => {
  it("keeps the branch order the engine sent, because the default is first", () => {
    expect(branchNamesFrom(["main", "release", "spike"])).toEqual(["main", "release", "spike"]);
    // ...and the same list as the JSON text a builtin's reply row can carry.
    expect(branchNamesFrom('["trunk","dev"]')).toEqual(["trunk", "dev"]);
  });

  it("drops a member that is not a branch name rather than offering an empty option", () => {
    expect(branchNamesFrom(["main", "", "   ", 7, null])).toEqual(["main"]);
  });

  it("answers no branches for anything it cannot read, and never throws", () => {
    expect(branchNamesFrom(undefined)).toEqual([]);
    expect(branchNamesFrom("not json at all")).toEqual([]);
    expect(branchNamesFrom({ main: true })).toEqual([]);
  });

  it("projects the manifest summary field by field", () => {
    expect(
      manifestFrom({
        name: "acme-storefront",
        deployables: [{ name: "web", kind: "static", path: "clients/web" }],
        dslDomains: ["shop"],
      }),
    ).toEqual({
      name: "acme-storefront",
      deployables: [{ name: "web", kind: "static", path: "clients/web" }],
      dslDomains: ["shop"],
    });
  });

  it("drops a declared deployable with no name, which nothing could preview", () => {
    expect(manifestFrom({ deployables: [{ kind: "static", path: "clients/web" }] }).deployables).toEqual([]);
  });

  it("answers an empty summary for every shape it cannot read", () => {
    // EMPTY IS A VALID ANSWER AND NEVER A COMPLAINT: a repository with no
    // manifest, one that does not parse, and a reply this build cannot read
    // all render no preview and say nothing. The analysis is the authority.
    for (const value of [undefined, null, "", "not json", "[]", 7]) {
      expect(manifestIsEmpty(manifestFrom(value)), `${String(value)} is not empty`).toBe(true);
    }
    expect(manifestIsEmpty(EMPTY_MANIFEST)).toBe(true);
    // The reachable positive: a summary with anything in it is NOT empty.
    expect(manifestIsEmpty(manifestFrom({ name: "acme" }))).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// THE SKIP MUST SURVIVE THE WIRE
// ---------------------------------------------------------------------------
//
// `skip` was declared on Placement, documented at length, set correctly by
// `placementsFrom` and parsed correctly by the engine's `placementsArg` -- and
// dropped in between, because this function's return type was
// `Record<string, Record<string, string>>` and a boolean has nowhere to go in
// a string map. The engine then defaulted Skip to false and BUILT AND
// PUBLISHED the app the person had just chosen to leave out; because a fresh
// site starts at `draft`, the visible symptom was "skip creates a draft".
//
// Measured in production: a run whose `web` app was skipped recorded
// `"created": true` and a published bundleRef for it, with no skip refusal.
describe("a skipped placement on the wire", () => {
  it("carries skip through, so the engine's Skip guard can fire", () => {
    expect(
      placementsPayload({ web: { hostname: "web.example.com", accountId: "", ownDomain: "", skip: true } }),
    ).toEqual({ web: { hostname: "web.example.com", skip: true } });
  });

  it("survives even when the app has never been deployed and has no hostname", () => {
    // The memql#4930 case the engine's guard is written for: skipping an app
    // with no address must not send an entry with nothing in it, because an
    // entry dropped whole is an entry whose skip is dropped with it.
    expect(placementsPayload({ web: { hostname: "", accountId: "", ownDomain: "", skip: true } })).toEqual({
      web: { skip: true },
    });
  });

  it("omits skip entirely when it is not set, rather than sending false", () => {
    // The negative control, and the same rule the blank halves keep: an
    // explicit value is one the pipeline reads, so an unanswered half is
    // absent rather than defaulted.
    expect(placementsPayload({ web: { hostname: "web.example.com", accountId: "", ownDomain: "" } })).toEqual({
      web: { hostname: "web.example.com" },
    });
  });
});
