// The plan's own status vocabulary, in the portal's tones. Not a general
// status mapper -- `awaitingFeedback` is a WARN here because it is waiting on
// the person reading the page, which is a different thing from a task that
// happens to be running.
//
// Lives in its own module because three surfaces read it now -- the goal
// header, the goals list, and the recent-goals strip -- and two of those sit
// on opposite sides of the GoalLayout -> GoalPicker import edge, where a
// shared export inside either would be a cycle.
export function toneForStatus(status: string): "ok" | "warn" | "danger" | "neutral" {
  switch (status) {
    case "succeeded":
      return "ok";
    case "failed":
      return "danger";
    case "running":
    case "routing":
    case "planning":
    case "awaitingFeedback":
    case "needsAgent":
      return "warn";
    default:
      return "neutral";
  }
}
