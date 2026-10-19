// WHAT BECAME OF A MESSAGE AT ONE GOAL (design 2026-10-19, "Delivery by goal
// state"). Ask routes a person's message to goals already running in the
// conversation; the cluster records one status per goal it went to, and the
// person's message shows one line per goal: "To Weather in Phoenix · Received".
//
// A status this client does not know yet reads as Queued: the message was
// recorded for that goal and nothing has said it was refused.

/** The label for one route status ("received" -> "Received"). */
export function routeLabel(status: string): string {
  switch (status) {
    case "received":
      return "Received";
    case "answered":
      return "Answered";
    case "notApproved":
      return "Sent, not approved";
    case "saved":
      return "Saved";
    case "notDelivered":
      return "Not delivered";
    case "notReceived":
      return "Not received";
    default:
      return "Queued";
  }
}
