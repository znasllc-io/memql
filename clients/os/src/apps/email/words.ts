// THE EMAIL APP'S VOCABULARY (memql#6033, Ruling 6): one word per thing.
//
// The app used to say a sender four ways (mailbox, sender, identity, address),
// a retired one three (retired, filed, disabled), and send readiness in five
// sentences. Each thing has one word here, and a sentence the app says in more
// than one place is written once. The state words are the ones a person reads;
// the engine's own statuses (`disabled`, `ready`, `sent`) are never shown.
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
} as const;
