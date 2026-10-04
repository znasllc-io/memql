// Where MemQL OS is for a cluster (clusters/consoleUrl.ts): the cluster's own
// site row when there is a connection to read it over, composed from the
// domain when there is not. Moved here from connectionView.test.ts when the
// cluster page was rebuilt; the page itself is tested there.

import test from "node:test";
import assert from "node:assert/strict";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

import type { ClusterConfig } from "../src/clusters/model.js";
import { composeConsoleUrl, consoleConceptUrl, consoleTarget } from "../src/clusters/consoleUrl.js";

function cluster(over: Partial<ClusterConfig> = {}): ClusterConfig {
  return { name: "staging", endpoint: "api.example.com:443", domain: "example.com", ...over };
}

// -----------------------------------------------------------------------------
// where the portal is
// -----------------------------------------------------------------------------

function siteRow(payload: Record<string, unknown>): Row {
  return { id: `v1:platform:site:${String(payload["hostname"])}`, concept: "v1:platform:site", payload };
}

test("the cluster's OWN site row outranks the composed host", () => {
  // Reading the row is what kept this correct across BOTH moves of the
  // console's origin -- memql#3711 and epic memql#4984. It is not sufficient
  // on its own, though: see the composition tests below, which are what a
  // first run actually reaches, and which needed an edit each time.
  const target = consoleTarget(cluster(), [
    siteRow({ hostname: "shop.example.com", systemOwned: false }),
    siteRow({ hostname: "console.example.com", systemOwned: true }),
  ]);
  assert.equal(target.url, "https://console.example.com/");
  assert.equal(target.fromSiteRow, true);
});

test("systemOwned is what identifies it, not a name", () => {
  // Matching on a name would pick a customer's site the day somebody calls one
  // "os". It is also what made the portal's retirement a no-op on this path:
  // the flag moved to the row that replaced it and nothing here had to know.
  const target = consoleTarget(cluster(), [siteRow({ hostname: "os.example.com", systemOwned: false })]);
  assert.equal(target.fromSiteRow, false);
  assert.equal(target.url, "https://os.example.com/");
});

test("the composed console is its OWN origin, not a path on the api front door", () => {
  // memql#3711 moved the console to its own hostname; this composition did not
  // follow until memql#3906. The old address does not 404 -- `/portal/` had no
  // Ingress rule of its own, so it fell through to the `/` h2c catch-all and
  // answered 415, an HTTP/1.1 request handed to a gRPC backend.
  //
  // The host then moved AGAIN: epic memql#4984 retired the portal, and this
  // composition follows the console rather than the label it used to carry.
  const target = consoleTarget(cluster(), []);
  assert.equal(target.url, "https://os.example.com/");
  assert.equal(target.fromSiteRow, false);
  assert.doesNotMatch(target.url, /\/portal\/$/, "the sub-path form is what 415s");
  assert.doesNotMatch(target.url, /portal\./, "the retired portal host answers nothing");
});

test("an api.<domain> endpoint implies its console sibling", () => {
  assert.equal(
    composeConsoleUrl({ name: "x", endpoint: "api.lab.example.com:50051" }),
    // Same derivation identityBaseUrlFor makes for identity.<domain>: the
    // endpoint IS the api front door, so stripping that label names the domain.
    // The port is dropped -- the console is served over 443 whatever the gRPC
    // endpoint names.
    "https://os.lab.example.com/",
  );
  // The shared endpoint parser, not a second `split(":")`: a scheme-prefixed
  // endpoint used to compose `https://https/portal/`.
  assert.equal(
    composeConsoleUrl({ name: "x", endpoint: "https://api.lab.example.com:443" }),
    "https://os.lab.example.com/",
  );
});

test("an endpoint that names no api front door composes nothing", () => {
  // The console is a DIFFERENT host from the gRPC endpoint, so there is no
  // host left to reuse. Opening the endpoint's own hostname would point a
  // browser at an address nobody nominated.
  assert.equal(composeConsoleUrl({ name: "x", endpoint: "grpc.lab.example.com:50051" }), "");
  assert.equal(composeConsoleUrl({ name: "x", endpoint: "[::1]:50051" }), "");
});

test("nothing to compose from yields nothing, not https:///", () => {
  assert.equal(composeConsoleUrl({ name: "x", endpoint: "" }), "");
  assert.equal(consoleTarget({ name: "x", endpoint: "" }, []).url, "");
});

// THE CONCEPT DEEP-LINK IS GONE (epic memql#4984), and so are the three cases
// that pinned its escaping. `portalConceptUrl` composed `<root>/concepts/<id>`
// for the "Browse rows" button; MemQL OS has no concept browser, the button was
// removed rather than pointed at a page that answers 404, and
// `encodePortalSegment` had that one caller. Nothing is left to escape.

// -----------------------------------------------------------------------------
// the concept deep-link (epic memql#5009, memql#5010)
// -----------------------------------------------------------------------------

// A QUERY PARAMETER, NOT A PATH SEGMENT, and the difference is the shell's
// shape rather than taste: MemQL OS has no router, so `/concepts/<id>` would
// be served the shell's own index and the marker would be lost on the desk.
test("composes the console's address for one concept's rows", () => {
  assert.equal(
    consoleConceptUrl("https://os.example.com/", "v1:library:artifact"),
    "https://os.example.com/?concept=v1%3Alibrary%3Aartifact",
  );
});

// The root arrives from `consoleTarget`, which may or may not end in a slash
// depending on whether a site row or the composition supplied it. Both have
// to produce the same address, or the same concept opens at two URLs.
test("does not double the slash, and adds one when the root has none", () => {
  assert.equal(
    consoleConceptUrl("https://os.example.com", "v1:work:goal"),
    consoleConceptUrl("https://os.example.com/", "v1:work:goal"),
  );
});

// EMPTY IN, EMPTY OUT. `consoleTarget` answers "" when no address can be
// worked out at all, and composing a link onto that would open `https:///?...`
// -- a browser tab at nothing, which reads as the extension being broken
// rather than as the cluster having no domain recorded.
test("composes nothing when there is no console address or no concept", () => {
  assert.equal(consoleConceptUrl("", "v1:library:artifact"), "");
  assert.equal(consoleConceptUrl("https://os.example.com/", ""), "");
  assert.equal(consoleConceptUrl("https://os.example.com/", "   "), "");
});
