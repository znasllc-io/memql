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
export function journeyReading(j: JourneyRow, dirty: boolean, issues: readonly Issue[]): { word: string; detail: string; tone: ActionBarTone } {
  return { word: dirty ? "Unsaved changes" : ({ draft: "Draft", active: "On", paused: "Paused", archived: "Retired" }[j.status] || ""), detail: issues.length ? `${issues.length} ${issues.length === 1 ? "thing" : "things"} to fix before turning on` : "", tone: j.status === "active" ? "live" : j.status === "paused" ? "paused" : "none" };
}
export function ruleActs(r: EmailRuleRow, dirty: boolean, asking: Asking): ActSpec[] {
  if (dirty) return dirtyActs;
  if (asking) return confirmActs(asking);
  return [...(r.status === "active" || r.status === "paused" && r.constructName ? [{ id: "retire", label: "Retire", text: true }] : []), r.status === "active" ? { id: "pause", label: "Pause", tone: "primary" } : { id: "turn-on", label: "Turn on", tone: "primary" }];
}
