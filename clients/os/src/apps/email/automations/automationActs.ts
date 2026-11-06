import { emailState } from "../words";
import type { ActSpec } from "../acts";
import type { EmailRuleRow, JourneyRow } from "../rows";
import type { Issue } from "./issues";
import type { ActionBarTone } from "../../../kit/ActionBar";
export type Asking = "" | "turn-on" | "retire";
function confirmActs(asking: Asking): ActSpec[] { return [{ id: "keep", label: asking === "retire" ? "Keep" : "Not yet", text: true }, { id: "confirm", label: asking === "retire" ? "Retire" : "Turn on", tone: asking === "retire" ? "danger" : "primary" }]; }
const dirtyActs: ActSpec[] = [{ id: "discard", label: "Discard changes", text: true }, { id: "save", label: "Save", tone: "primary" }];
export function journeyActs(j: JourneyRow, dirty: boolean, issues: readonly Issue[], asking: Asking): ActSpec[] {
  if (j.status === "archived") return [];
  if (dirty) return dirtyActs;
  if (asking) return confirmActs(asking);
  return [{ id: "retire", label: "Retire", text: true }, ...(j.status === "active" ? [{ id: "pause", label: "Pause", tone: "primary" as const }] : !issues.length ? [{ id: "turn-on", label: "Turn on", tone: "primary" as const }] : [])];
}
/**
 * What stands in the way, in the bar's own words. An automation that is on is not
 * waiting to be turned on, it is holding emails (a template saved back to a
 * draft, a sender turned off), so it says what is held rather than what is missing.
 */
function blockedSentence(j: JourneyRow, issues: readonly Issue[]): string {
  if (!issues.length) return "";
  const more = issues.length > 1 ? ` ${issues.length - 1} more to fix.` : "";
  if (j.status === "active") return `Emails held: ${issues[0]!.text.replace(/ before turning this on\.$/, " to send its emails.")}${more}`;
  return `Can't turn on yet: ${issues[0]!.text}${more}`;
}
export function journeyReading(j: JourneyRow, dirty: boolean, issues: readonly Issue[]): { word: string; detail: string; tone: ActionBarTone } {
  return { word: dirty ? "Unsaved changes" : emailState("journey", j.status), detail: blockedSentence(j, issues), tone: j.status === "active" ? "live" : j.status === "paused" ? "paused" : "none" };
}
export function ruleActs(r: EmailRuleRow, dirty: boolean, asking: Asking): ActSpec[] {
  if (dirty) return dirtyActs;
  if (asking) return confirmActs(asking);
  return [...(r.status === "active" || r.status === "paused" && r.constructName ? [{ id: "retire", label: "Retire", text: true }] : []), r.status === "active" ? { id: "pause", label: "Pause", tone: "primary" } : { id: "turn-on", label: "Turn on", tone: "primary" }];
}
