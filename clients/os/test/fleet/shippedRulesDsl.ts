import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import type { Row } from "@znasllc-io/memql-sdk-core/client";

/**
 * The shipped rules, READ FROM `dsl/rules/rules.memql` -- the way
 * test/seededAccess.ts reads the seeds -- in the shape `routingRules`
 * answers them.
 *
 * The routing fixtures (routingFixtures.ts) carry a literal copy because the
 * browser QA harness shares them and cannot read a file; a literal is a
 * second copy that drifts, and it did: it once held five of the nine rules,
 * and the four it lacked were exactly the ones whose words leaked a retired
 * routing word onto the Rules list. So the vocabulary sweep reads THIS, and a
 * parity test holds the literal to it.
 *
 * Node-only: never import this from anything the QA harness loads.
 */

const here = dirname(fileURLToPath(import.meta.url));
export const SHIPPED_RULES_PATH = join(here, "..", "..", "..", "..", "dsl", "rules", "rules.memql");

// A rule is its annotations, then `rule <name> { }`. Doc comments (`///`)
// sit above the annotations, never between them.
const RULE = /((?:@\w+(?:\([^)]*\))?\s*)+)rule\s+(\w+)\s*\{/g;
const ANNOTATION = /@(\w+)(?:\(([^)]*)\))?/g;

function quoted(raw: string): string {
  return /^"([^"]*)"$/.exec(raw.trim())?.[1] ?? raw.trim();
}

export function shippedRulesFromDsl(): Row[] {
  const src = readFileSync(SHIPPED_RULES_PATH, "utf8");
  const out: Row[] = [];
  for (const m of src.matchAll(RULE)) {
    const row: Row = { id: m[2]!, name: m[2]!, when: {}, level: "", policy: "", precedence: 0, onUnavailable: "", excludes: [], locked: false, described: "" };
    for (const a of m[1]!.matchAll(ANNOTATION)) {
      const [, key, args = ""] = a;
      if (key === "when") {
        const when: Record<string, string> = {};
        for (const kv of args.matchAll(/(\w+)\s*=\s*"([^"]*)"/g)) when[kv[1]!] = kv[2]!;
        row.when = when;
      } else if (key === "policy") row.policy = quoted(args);
      else if (key === "level") row.level = quoted(args);
      else if (key === "precedence") row.precedence = Number(args.trim());
      else if (key === "onUnavailable") row.onUnavailable = quoted(args);
      else if (key === "locked") row.locked = true;
    }
    out.push(row);
  }
  if (out.length === 0) throw new Error(`read no rules out of ${SHIPPED_RULES_PATH}; the file moved or changed shape`);
  return out;
}
