import test from "node:test";
import assert from "node:assert/strict";

import {
  describeOpenRequest,
  isArtifactId,
  openRequestUri,
  parseOpenRequest,
} from "../src/handoff/openRequest.js";

const ok = (query: string) => parseOpenRequest({ path: "/open", query });

test("the v=1 contract parses", () => {
  assert.deepEqual(ok("v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace"), {
    version: "1",
    target: "construct",
    domain: "acme.example.com",
    kind: "concept",
    name: "v1:cognition:space",
  });
});

test("the domain is normalised and case-folded", () => {
  const r = ok("v=1&cluster=.Acme.Example.COM.&kind=query&name=x");
  assert.equal("domain" in r ? r.domain : r.error, "acme.example.com");
});

test("every malformed input is refused by name", () => {
  const cases: Array<[string, RegExp]> = [
    ["v=2&cluster=a.test&kind=query&name=x", /v=2/],
    ["cluster=a.test&kind=query&name=x", /\bv\b/],
    ["v=1&kind=query&name=x", /cluster/],
    ["v=1&cluster=a.test&cluster=b.test&kind=query&name=x", /cluster/],
    ["v=1&cluster=a.test&name=x", /kind/],
    ["v=1&cluster=a.test&kind=Query&name=x", /kind/],
    ["v=1&cluster=a.test&kind=query", /name/],
    ["v=1&cluster=a.test&kind=query&name=..%2Fetc%2Fpasswd", /name/],
    ["v=1&cluster=a.test&kind=query&name=a%00b", /name/],
    ["v=1&cluster=not%20a%20host&kind=query&name=x", /cluster/],
    [`v=1&cluster=a.test&kind=query&name=${"x".repeat(600)}`, /name/],
    [`v=1&cluster=a.test&kind=query&name=x&pad=${"p".repeat(5000)}`, /query/],
  ];
  for (const [query, field] of cases) {
    const r = ok(query);
    assert.ok("error" in r, query);
    assert.match(r.error, field, query);
  }
  const wrongPath = parseOpenRequest({ path: "/run", query: "v=1&cluster=a.test&kind=query&name=x" });
  assert.ok("error" in wrongPath && /\/run/.test(wrongPath.error));
});

test("a hostile path is named in the refusal but not echoed whole", () => {
  // The refusal is rendered into a TOAST, and the path is attacker-supplied by
  // exactly the same route the query is -- but only the query was capped. A
  // megabyte of `/aaaa...` would be pasted into the notification verbatim.
  const long = parseOpenRequest({ path: `/${"a".repeat(5000)}`, query: "v=1&cluster=a.test&kind=query&name=x" });
  assert.ok("error" in long);
  assert.ok(long.error.length < 200, `the refusal echoed ${long.error.length} characters`);
  // Still says enough to debug the link: the start of the path, and where the
  // handler does listen.
  assert.match(long.error, /\/aaa/);
  assert.match(long.error, /\/open/);
});

// -----------------------------------------------------------------------------
// kind=artifact (memql#4748)
//
// MemQL OS fires `...&kind=artifact&id=<artifactId>` when somebody opens a file
// on the desktop, and every one of those links was refused as "missing name"
// before this target existed. These cases pin the two halves of the fix: the id
// is accepted under its OWN rule rather than the construct-name one, and the
// two addresses cannot both be present.
// -----------------------------------------------------------------------------

test("an artifact link parses to the artifact target, with the id untouched", () => {
  assert.deepEqual(ok("v=1&cluster=acme.example.com&kind=artifact&id=v1%3Alibrary%3Aartifact%3Aa9f3b7c2"), {
    version: "1",
    target: "artifact",
    domain: "acme.example.com",
    kind: "artifact",
    id: "v1:library:artifact:a9f3b7c2",
  });
});

test("a bare short id is an artifact id too", () => {
  // The engine bare-ifies ids on egress, so the OS forwards whichever spelling
  // the row it was reading carried. Refusing one of them would refuse roughly
  // half of all real links.
  const r = ok("v=1&cluster=a.test&kind=artifact&id=a9f3b7c2-4d1e");
  assert.deepEqual(r, { version: "1", target: "artifact", domain: "a.test", kind: "artifact", id: "a9f3b7c2-4d1e" });
});

test("the id is judged by the id rule, not the construct-name rule", () => {
  // The whole reason the union exists: a canonical id is nothing but colons,
  // and a name carrying `..` or a separator is refused where an id carrying a
  // colon is not.
  assert.ok(isArtifactId("v1:library:artifact:abc"));
  assert.ok(isArtifactId("A_1-2"));
  for (const bad of [
    "",
    "-leading-hyphen",
    ":leading-colon",
    "../etc/passwd",
    "a/b",
    "a\\b",
    "a.b",
    "a".repeat(256),
  ]) {
    assert.equal(isArtifactId(bad), false, bad);
  }
});

test("an artifact link is refused by name for every malformed id", () => {
  const cases: Array<[string, RegExp]> = [
    ["v=1&cluster=a.test&kind=artifact", /\bid\b/],
    ["v=1&cluster=a.test&kind=artifact&id=", /\bid\b/],
    ["v=1&cluster=a.test&kind=artifact&id=a&id=b", /\bid\b/],
    ["v=1&cluster=a.test&kind=artifact&id=..%2F..%2Fsecret", /artifact id/],
    ["v=1&cluster=a.test&kind=artifact&id=a%2Fb", /artifact id/],
    [`v=1&cluster=a.test&kind=artifact&id=${"x".repeat(300)}`, /artifact id/],
  ];
  for (const [query, field] of cases) {
    const r = ok(query);
    assert.ok("error" in r, query);
    assert.match(r.error, field, query);
  }
});

test("a link may not state two addresses for one open", () => {
  const both = ok("v=1&cluster=a.test&kind=artifact&id=abc&name=whatever");
  assert.ok("error" in both);
  assert.match(both.error, /name/);

  const other = ok("v=1&cluster=a.test&kind=query&name=q&id=abc");
  assert.ok("error" in other);
  assert.match(other.error, /\bid\b/);
});

test("a blank address key is silence, not a second address", () => {
  // `one()` already reads a blank value as missing, and `carries()` has to
  // agree with it -- otherwise `kind=artifact&name=` is refused for stating two
  // addresses when what it actually states is one.
  const r = ok("v=1&cluster=a.test&kind=artifact&id=abc&name=");
  assert.deepEqual(r, { version: "1", target: "artifact", domain: "a.test", kind: "artifact", id: "abc" });
});

test("the id is never echoed into the refusal", () => {
  // The sentence is rendered into a toast and the id is attacker-supplied by
  // the same route everything else in the link is.
  const hostile = "javascript:alert(1)".repeat(3);
  const r = parseOpenRequest({ path: "/open", query: `v=1&cluster=a.test&kind=artifact&id=${encodeURIComponent(hostile)}` });
  assert.ok("error" in r);
  assert.ok(!r.error.includes("alert"), r.error);
});

test("every request describes itself by its own address", () => {
  assert.equal(
    describeOpenRequest({ version: "1", target: "construct", domain: "d", kind: "query", name: "q" }),
    "query q",
  );
  assert.equal(
    describeOpenRequest({ version: "1", target: "artifact", domain: "d", kind: "artifact", id: "v1:library:artifact:x" }),
    "artifact v1:library:artifact:x",
  );
});

test("a request round-trips through the link it recomposes", () => {
  // The replay after a window reload goes back through the uri handler, so a
  // composer that fell out of step with the parser would surface as a replay
  // refusing its own parked request.
  for (const request of [
    { version: "1", target: "construct", domain: "acme.example.com", kind: "query", name: "v1:a:b" },
    { version: "1", target: "artifact", domain: "acme.example.com", kind: "artifact", id: "v1:library:artifact:x" },
  ] as const) {
    const url = new URL(openRequestUri(request));
    assert.deepEqual(parseOpenRequest({ path: url.pathname, query: url.search.replace(/^\?/, "") }), request);
  }
});
