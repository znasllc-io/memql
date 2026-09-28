import { ArrowRight } from "lucide-react";

import type { RuleRow } from "../../settings/rulesFacts";
import { routeTitle, ruleExtras, whenWords } from "./vocabulary";

/**
 * A rule as the sentence it is: [When] -> [Route], and what else it does in a
 * few quiet words. The arrow is an icon, never a typed character; a screen
 * reader hears "takes" in its place.
 */
export function RuleSentence({ rule }: { rule: Pick<RuleRow, "when" | "locked" | "policy" | "level" | "onUnavailable"> }) {
  const extras = ruleExtras(rule);
  return (
    <span className="fleet-rule-sentence">
      <span className="fleet-rule-when">{whenWords(rule)}</span>
      <ArrowRight size={14} aria-hidden className="fleet-rule-arrow" />
      <span className="os-sr-only"> takes </span>
      <span className="fleet-rule-route">{rule.policy ? routeTitle(rule.policy) : "No route"}</span>
      {extras ? <span className="fleet-rule-extra">{extras}</span> : null}
    </span>
  );
}
