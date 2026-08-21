import test from "node:test";
import assert from "node:assert/strict";

import { parseOpenRequest } from "../src/handoff/openRequest.js";

const ok = (query: string) => parseOpenRequest({ path: "/open", query });

test("the v=1 contract parses", () => {
  assert.deepEqual(ok("v=1&cluster=acme.example.com&kind=concept&name=v1%3Acognition%3Aspace"), {
    version: "1",
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
