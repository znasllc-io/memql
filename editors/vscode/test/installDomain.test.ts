// One front door, named in five places (memql#3590).
//
// The domain the operator types reached `seedBootstrap` and `enrolmentLink`.
// `hostsBlock`, `localCA` and `frontDoor` used their own defaults, and underneath
// all of them the release's local overlay pins the same hostnames again in its
// Ingresses and identity config. Five independent statements of one fact, four of
// them invisible from the form.
//
// They agreed only because every one of them said `memql.localhost`. Nothing
// checked that they did, and the first person to change any single one of them
// would have produced a cluster that resolves and then answers as the wrong site.
//
// So this asserts the agreement directly, against the SHIPPED files. It is the
// test that would have caught the whole class, and it fails the moment one
// artifact learns a different domain from the others.

import test from "node:test";
import assert from "node:assert/strict";
import * as fs from "node:fs";
import * as path from "node:path";

import { DEFAULT_LOCAL_DOMAIN, installDomainProblem } from "../src/install/stackPin.js";

const REPO_ROOT = path.resolve(__dirname, "..", "..", "..", "..");

function read(rel: string): string {
  const p = path.join(REPO_ROOT, rel);
  assert.ok(fs.existsSync(p), `${rel} does not exist -- this test is asserting about the wrong path`);
  return fs.readFileSync(p, "utf8");
}

/**
 * Every front-door hostname a file mentions.
 *
 * Deliberately broad: any dotted name with a `cockpit.` / `identity.` prefix or a
 * `*.` wildcard. A narrower pattern would silently stop covering the artifact it
 * is supposed to be watching.
 */
function frontDoorNames(body: string): string[] {
  const found = new Set<string>();
  // COMMENT LINES ARE SKIPPED. Both shell and YAML use `#`, and prose legitimately
  // contains hostnames that are not statements about this cluster --
  // hosts-entries.sh explains why wildcards are rejected by naming `*.foo`, which
  // is an example, not a front door. A stale hostname in a comment is a
  // documentation bug; failing a build over one would teach people to delete the
  // comment rather than fix it.
  const code = body
    .split("\n")
    .filter((line) => !/^\s*#/.test(line))
    .join("\n");
  for (const m of code.matchAll(/(?:\*|cockpit|identity)\.[a-z0-9.-]*[a-z]{2,}/gi)) {
    const name = m[0];
    // In-cluster service names are not front-door hostnames: `identity:8085` has
    // no dot, but `identity.memql.svc` does and is internal.
    if (name.endsWith(".svc") || name.includes(".svc.")) continue;
    found.add(name.toLowerCase());
  }
  return [...found];
}

/** The apex of a front-door name: `cockpit.memql.localhost` -> `memql.localhost`. */
function apexOf(host: string): string {
  return host.replace(/^(?:\*|cockpit|identity)\./i, "");
}

// The artifacts that state the front door, and HOW each one states it.
//
// THREE kinds, because memql#3593 changed the shape of most of them.
//
//   hosts    -- spells whole hostnames. An Ingress host is a literal, so the
//               two manifests still do this, and every name they spell must
//               have the default apex.
//   apex     -- pins the apex once and derives its hostnames from it. Any
//               literal it pins must be the default.
//   derives  -- pins NOTHING and reads the apex from scripts/lib/localtls.sh.
//               Asserted to contain no literal at all, because the moment it
//               grows one it has started disagreeing with its own source.
//
// A test that only knew the first kind would have gone quiet the moment the
// scripts stopped repeating themselves, which is exactly when it stops being
// able to catch anything.
const ARTIFACTS: readonly { rel: string; what: string; kind: "hosts" | "apex" | "derives" }[] = [
  { rel: "scripts/install/hosts-entries.sh", what: "the hosts block the installer writes", kind: "apex" },
  { rel: "scripts/lib/localtls.sh", what: "the names the local certificate covers", kind: "apex" },
  { rel: "scripts/k3d/up.sh", what: "the default the Ingress patches are compared against", kind: "apex" },
  { rel: "scripts/install/verify-frontdoor.sh", what: "the hostnames the front-door check probes", kind: "derives" },
  { rel: "deploy/k8s/overlays/local/cockpit-front-door.yaml", what: "the cockpit Ingress host", kind: "hosts" },
  { rel: "deploy/k8s/overlays/local/front-door.yaml", what: "the identity Ingress host", kind: "hosts" },
];

/**
 * The apexes a shell file pins as its default domain.
 *
 * Matches a domain on a line that assigns something domain-shaped --
 * `readonly DEFAULT_DOMAIN="memql.localhost"`, `MEMQL_LOCAL_DOMAIN:-memql.localhost`,
 * `OVERLAY_DEFAULT_DOMAIN="memql.localhost"`. Deliberately narrow: a hostname in
 * prose is not a statement about this cluster.
 */
function apexConstants(body: string): string[] {
  const found = new Set<string>();
  for (const line of body.split("\n")) {
    if (/^\s*#/.test(line)) continue;
    if (!/DOMAIN/i.test(line)) continue;
    // LOWERCASE ONLY, and not preceded by a dot. Both exclusions are load-
    // bearing: a kubectl jsonpath like `{.data.MEMQL_DOMAIN}` sits on a line
    // that mentions DOMAIN and offers `data.MEMQL` to a case-insensitive
    // pattern, which is a field path rather than a hostname.
    for (const m of line.matchAll(/(^|[^.\w])([a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)+)/g)) {
      const candidate = m[2];
      if (/\.(sh|go|yaml|yml|json|ts|md)$/.test(candidate)) continue;
      found.add(candidate);
    }
  }
  return [...found];
}

test("every artifact that names the front door names the same one", () => {
  for (const { rel, what, kind } of ARTIFACTS) {
    const body = read(rel);
    if (kind === "hosts") {
      const names = frontDoorNames(body);
      assert.ok(
        names.length > 0,
        `${rel} names no front-door host at all, so ${what} is not where this test thinks`,
      );
      for (const host of names) {
        assert.equal(
          apexOf(host),
          DEFAULT_LOCAL_DOMAIN,
          `${rel} names ${host} (${what}), but the installer offers ` +
            `${DEFAULT_LOCAL_DOMAIN}. A cluster whose ingress, certificate, hosts block and probe ` +
            `do not agree resolves and then answers as the wrong site -- and each file looks correct ` +
            `on its own, which is why this is checked rather than reviewed.`,
        );
      }
      continue;
    }

    const apexes = apexConstants(body);
    if (kind === "derives") {
      assert.deepEqual(
        apexes,
        [],
        `${rel} pins ${apexes.join(", ")} (${what}), but it is supposed to read the apex ` +
          `from scripts/lib/localtls.sh. A literal here is a second source of truth for the ` +
          `same fact, which is how ${rel} and the certificate come to name different front doors.`,
      );
      continue;
    }
    assert.ok(
      apexes.length > 0,
      `${rel} pins no default domain at all, so ${what} is not where this test thinks`,
    );
    for (const apex of apexes) {
      assert.equal(
        apex,
        DEFAULT_LOCAL_DOMAIN,
        `${rel} pins ${apex} (${what}), but the installer offers ${DEFAULT_LOCAL_DOMAIN}.`,
      );
    }
  }
});

// The validator's job changed with memql#3593: it no longer enforces one
// domain, it rejects answers that cannot be hostnames. Each refusal has to say
// which mistake was made -- "invalid" alone leaves the operator with a field
// they cannot fill.
test("a well-formed domain is accepted, whoever owns it", () => {
  for (const domain of [DEFAULT_LOCAL_DOMAIN, "lab.example.com", "memql.localhost", "a.b.c.test"]) {
    assert.equal(installDomainProblem(domain), undefined, `${domain} should be accepted`);
  }
});

test("what cannot be a hostname is refused, and the refusal says why", () => {
  const cases: [string, string][] = [
    ["https://memql.localhost", "URL"],
    ["memql.localhost:443", "port"],
    ["*.memql.localhost", "wildcard"],
    ["localhost", "two labels"],
    ["MEMQL.localhost", "lowercase"],
    ["memql.localhost.", "lowercase"],
  ];
  for (const [domain, expect] of cases) {
    const problem = installDomainProblem(domain) ?? "";
    assert.notEqual(problem, "", `${domain} should be refused`);
    assert.ok(
      problem.includes(expect),
      `the refusal for ${domain} should mention ${expect}: ${problem}`,
    );
  }
});

test("whitespace and an empty field are handled", () => {
  assert.equal(
    installDomainProblem("  " + DEFAULT_LOCAL_DOMAIN + "  "),
    undefined,
    "a pasted value with surrounding whitespace is the same domain",
  );
  assert.equal(installDomainProblem(""), undefined, "an empty field is unanswered, not wrong");
  assert.equal(installDomainProblem("   "), undefined, "whitespace alone is unanswered too");
});
