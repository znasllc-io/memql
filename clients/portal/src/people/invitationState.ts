// What an outstanding invitation actually says about itself (memql#4587).
//
// -----------------------------------------------------------------------------
// THE DEFECT THIS IS FOR
// -----------------------------------------------------------------------------
//
// An invitation that was never delivered was indistinguishable, from every
// surface, from one that was. The row says `status: pending` whether the
// invitee got the link or the operator closed the panel and forgot. That is how
// memql#4583 went unnoticed until the owner asked the colleague in person:
// there was no error, no log line an operator would read, and no red status
// anywhere -- it looked exactly like success.
//
// The delivery verdict now lives on the row (`deliveryState`, memql#4587), and
// this module turns that plus the clock into the two things a list has to say:
// WAS IT SENT, and IS IT ABOUT TO EXPIRE UNUSED.
//
// Pure, and separate from the component, so both questions are answerable under
// `node --test` without a cluster or a DOM.

/** What happened to the invitation email, as the issuing path observed it. */
export type DeliveryState = "sent" | "failed" | "not_attempted" | "unknown";

export interface InvitationVerdict {
  delivery: DeliveryState;
  /** The provider's own text when delivery failed. Never the link. */
  deliveryError: string;
  /** True when nobody can have received this link except by the operator's hand. */
  needsManualDelivery: boolean;
  /** Hours until expiry. Negative once expired. NaN when unparseable. */
  hoursToExpiry: number;
  expired: boolean;
  /** Expiring soon, and still unaccepted -- the operator's cue to re-issue. */
  expiringSoon: boolean;
}

/**
 * How close to expiry counts as "soon".
 *
 * 48 hours against a 7-day default, so it names an invitation with about a
 * quarter of its life left. Short enough that it is not permanently on, which
 * would make it noise nobody reads -- the failure mode of a status that is
 * always red.
 */
export const EXPIRING_SOON_HOURS = 48;

/**
 * readDeliveryState maps the row's field, tolerating what predates it.
 *
 * An EMPTY value is `unknown`, NOT `not_attempted`. Rows issued before
 * memql#4587 carry nothing, and reading those as "nobody tried to send this"
 * would state, in the operator's own list, a fact nobody observed -- which is
 * the exact failure this surface exists to remove. Unknown says what is true:
 * this row predates the record.
 */
export function readDeliveryState(raw: string): DeliveryState {
  switch (raw.trim()) {
    case "sent":
      return "sent";
    case "failed":
      return "failed";
    case "not_attempted":
      return "not_attempted";
    default:
      return "unknown";
  }
}

export function invitationVerdict(
  fields: { deliveryState: string; deliveryError: string; expiresAt: string },
  nowMs: number,
): InvitationVerdict {
  const delivery = readDeliveryState(fields.deliveryState);
  const expiresMs = Date.parse(fields.expiresAt);
  const hoursToExpiry = Number.isNaN(expiresMs) ? Number.NaN : (expiresMs - nowMs) / 3_600_000;
  const expired = !Number.isNaN(hoursToExpiry) && hoursToExpiry <= 0;

  return {
    delivery,
    deliveryError: fields.deliveryError.trim(),
    // `unknown` is NOT manual delivery. It is genuinely unknown, and claiming
    // the operator must act would be as wrong as claiming they need not.
    needsManualDelivery: delivery === "failed" || delivery === "not_attempted",
    hoursToExpiry,
    expired,
    expiringSoon: !expired && !Number.isNaN(hoursToExpiry) && hoursToExpiry <= EXPIRING_SOON_HOURS,
  };
}

/** The badge tone and label for the delivery half. */
export function deliveryBadge(v: InvitationVerdict): { tone: "ok" | "warn" | "danger" | "neutral"; label: string } {
  switch (v.delivery) {
    case "sent":
      return { tone: "ok", label: "emailed" };
    case "failed":
      return { tone: "danger", label: "email failed" };
    case "not_attempted":
      return { tone: "warn", label: "not emailed" };
    case "unknown":
      return { tone: "neutral", label: "delivery unknown" };
  }
}

/**
 * The sentence under a row, or "" when there is nothing an operator must do.
 *
 * Silence is deliberate for the healthy case: a list that annotates every row
 * teaches people to skim past the annotations, and then the one that matters is
 * skimmed past too.
 */
export function invitationAdvice(v: InvitationVerdict): string {
  if (v.expired) {
    return "This invitation has expired unused. Re-send to mint a fresh link.";
  }
  const parts: string[] = [];
  switch (v.delivery) {
    case "failed":
      parts.push(
        v.deliveryError === ""
          ? "The email could not be delivered, so the invitee has not been told."
          : `The email could not be delivered (${v.deliveryError}), so the invitee has not been told.`,
      );
      break;
    case "not_attempted":
      parts.push(
        "No email was sent for this invitation -- the link had to be delivered by hand. " +
          "If it was not, the invitee has not been told.",
      );
      break;
    case "unknown":
      parts.push("This invitation predates delivery tracking, so whether it was emailed is not recorded.");
      break;
    case "sent":
      break;
  }
  if (v.expiringSoon) {
    parts.push(
      `It expires in ${formatHours(v.hoursToExpiry)} and has not been accepted.` +
        (v.delivery === "sent" ? " Re-send if they never received it." : ""),
    );
  }
  return parts.join(" ");
}

function formatHours(hours: number): string {
  if (hours < 1) {
    const minutes = Math.max(1, Math.round(hours * 60));
    return `${minutes} minute${minutes === 1 ? "" : "s"}`;
  }
  if (hours < 48) {
    const whole = Math.round(hours);
    return `${whole} hour${whole === 1 ? "" : "s"}`;
  }
  const days = Math.round(hours / 24);
  return `${days} day${days === 1 ? "" : "s"}`;
}
