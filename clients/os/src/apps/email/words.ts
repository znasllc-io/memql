// THE EMAIL APP'S VOCABULARY (memql#6033, Ruling 6): one word per thing.
//
// The app used to have four words for a sender, three for one that was put
// away (retired, filed, disabled), and five sentences for send readiness.
// Each thing has one word here, and a sentence the app says in more than one
// place is written once. The state words are the ones a person reads; the
// engine's own statuses (`disabled`, `ready`, `sent`) are never shown.
//
// THIS FILE GROWS WITH THE APP. It holds what the screens that exist say; each
// later screen that names a state adds its words here rather than spelling them.

export const WORDS = {
  /** A thing that is in use. */
  active: "Active",
  /** A thing put away: never deleted, and brought back with Restore. */
  archived: "Archived",

  /**
   * The one sentence for sending that is not set up for an organization. The
   * engine's reason for it is the detail beneath, verbatim; this is only the
   * statement of what is wrong and for whom.
   */
  sendingNotSetUp: (organization: string): string => `Sending isn't set up for ${organization}.`,

  /**
   * The check itself failed, which is not the same as sending not being set up:
   * nothing is known about it either way. The read's own error is the detail.
   */
  sendingNotChecked: (organization: string): string => `Couldn't check sending for ${organization}.`,

  /** A campaign's state, by the engine's status. A status this build does not know has no word. */
  campaign: {
    draft: "Draft",
    scheduled: "Scheduled",
    sending: "Sending",
    paused: "Paused",
    sent: "Sent",
    cancelled: "Cancelled",
    failed: "Failed",
  } as Readonly<Record<string, string>>,

  /** A draft that a repeating series sends from reads as the series does, by its status. */
  repeating: {
    active: "Repeating",
    paused: "Repeating paused",
    blocked: "Repeating stopped",
  } as Readonly<Record<string, string>>,

  /**
   * A change on its way, from the press that confirmed it until the campaign's
   * row says it happened: the bar reads this while the act stays busy.
   */
  changing: {
    send: "Starting",
    schedule: "Scheduling",
    cancel: "Cancelling",
    pause: "Pausing",
    resume: "Resuming",
    repeat: "Starting",
  } as const,

  /** What happened to one recipient of a send, by the delivery's status. */
  delivery: {
    sent: "Sent",
    skipped: "Skipped",
    failed: "Failed",
    pending: "Not yet sent",
  } as Readonly<Record<string, string>>,

  /** How often a campaign repeats: "Every week", "Every 2 weeks". */
  every: (weeks: number): string => (weeks === 1 ? "Every week" : `Every ${weeks} weeks`),

  /** A template that is not published yet. */
  templateDraft: "Draft",

  /**
   * The cluster's own sender, which only the operator organization may send as.
   * Spelled as it reads inside a sentence ("as this cluster's sender"); a line
   * that starts with it capitalizes it.
   */
  clusterSender: "this cluster's sender",

  /** How many people an audience reaches now: "1,204 subscribers", "1 subscriber". */
  subscribers: (count: number): string => `${count.toLocaleString("en-US")} ${count === 1 ? "subscriber" : "subscribers"}`,
} as const;
