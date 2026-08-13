// One front door, named in five places (memql#3590).
//
// The domain the operator types reached `seedBootstrap` and `enrolmentLink`.
// `hostsBlock`, `localCA` and `frontDoor` used their own defaults, and underneath
// all of them the release's local overlay pins the same hostnames again in its
// Ingresses and identity config. Five independent statements of one fact, four of
// them invisible from the form.
//
// They agreed only because every one of them said `local.znas.io`. Nothing
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

import { SUPPORTED_LOCAL_DOMAIN, installDomainProblem } from "../src/install/stackPin.js";

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

/** The apex of a front-door name: `cockpit.local.znas.io` -> `local.znas.io`. */
function apexOf(host: string): string {
  return host.replace(/^(?:\*|cockpit|identity)\./i, "");
}

// The five statements of the one fact. Each is the file an operator or a
// reconciler actually reads.
const ARTIFACTS: readonly { rel: string; what: string }[] = [
  { rel: "scripts/install/hosts-entries.sh", what: "the hosts block the installer writes" },
  { rel: "scripts/install/verify-frontdoor.sh", what: "the hostnames the front-door check probes" },
  { rel: "scripts/lib/localtls.sh", what: "the names the local certificate covers" },
  { rel: "deploy/k8s/overlays/local/cockpit-front-door.yaml", what: "the cockpit Ingress host" },
  { rel: "deploy/k8s/overlays/local/front-door.yaml", what: "the identity Ingress host" },
];

test("every artifact that names the front door names the same one", () => {
  for (const { rel, what } of ARTIFACTS) {
    const names = frontDoorNames(read(rel));
    assert.ok(names.length > 0, `${rel} names no front-door host at all, so ${what} is not where this test thinks`);
    for (const host of names) {
      assert.equal(
        apexOf(host),
        SUPPORTED_LOCAL_DOMAIN,
        `${rel} names ${host} (${what}), but the installer offers and enforces ` +
          `${SUPPORTED_LOCAL_DOMAIN}. A cluster whose ingress, certificate, hosts block and probe ` +
          `do not agree resolves and then answers as the wrong site -- and each file looks correct ` +
          `on its own, which is why this is checked rather than reviewed.`,
      );
    }
  }
});

// The refusal has to name the domain that works. A refusal that only says "no"
// leaves the operator with a field they cannot fill.
test("the refusal names the domain that does work", () => {
  const problem = installDomainProblem("something.else.test") ?? "";
  assert.notEqual(problem, "", "an unservable domain must be refused");
  assert.ok(
    problem.includes(SUPPORTED_LOCAL_DOMAIN),
    `the refusal does not name the working domain: ${problem}`,
  );
  assert.equal(installDomainProblem(SUPPORTED_LOCAL_DOMAIN), undefined);
  assert.equal(
    installDomainProblem("  " + SUPPORTED_LOCAL_DOMAIN + "  "),
    undefined,
    "a pasted value with surrounding whitespace is the same domain",
  );
  assert.equal(installDomainProblem(""), undefined, "an empty field is unanswered, not wrong");
});
