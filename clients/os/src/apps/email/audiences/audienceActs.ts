import type { AudienceRow } from "../rows";
import type { ActSpec } from "../acts";

export function audienceActs(a: AudienceRow, asking: "" | "archive" | "confirmation"): ActSpec[] {
  if (asking) return [{ id: "keep", label: "Keep", text: true }, { id: "confirm", label: asking === "archive" ? "Archive" : a.confirmation === "required" ? "Stop asking to confirm" : "Ask people to confirm", tone: asking === "archive" ? "danger" : "primary" }];
  if (a.status === "archived") return [{ id: "restore", label: "Restore", tone: "primary" }];
  return [{ id: "archive", label: "Archive", text: true }, { id: "confirmation", label: a.confirmation === "required" ? "Stop asking to confirm" : "Ask people to confirm", text: true }];
}
