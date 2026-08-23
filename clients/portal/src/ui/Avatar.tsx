import type { ReactNode } from "react";

// The person, as two letters.
//
// INITIALS ONLY, and that is a data decision rather than an aesthetic one:
// v1:identity:user carries no avatar image field, so there is no picture to
// render and inventing a gravatar-style lookup would put every operator's
// email hash on a third-party wire to draw a circle.
//
// ARIA-HIDDEN, ALWAYS. The avatar never carries the name -- the link or the
// heading beside it does. A screen reader that announced "J S" and then "Jose
// Sanz" would read the same person twice, and the first reading is noise. That
// makes this component decorative by construction, which is also why it takes
// no `label`: there is no correct value for one.
//
// The ground is `accent-subtle` rather than a per-person hue. Colour-coding
// people would need a stable hash to a palette that stays legible in both
// themes and readable next to the accent bar the nav rows already use, and it
// would encode identity in a channel somebody cannot see. One ground, one
// meaning: this is you.

export type AvatarSize = "sm" | "md" | "lg";

const SIZE: Record<AvatarSize, string> = {
  sm: "h-6 w-6 text-[0.625rem]",
  md: "h-8 w-8 text-xs",
  lg: "h-12 w-12 text-base",
};

// initialsFrom takes at most two letters, and never returns nothing.
//
// The name first, its first and last word (so "Jose Sanz" is JS and "Ada
// Lovelace King" is AK -- the first and family name, which is how a person
// abbreviates themselves). Falling back to the email's first character when
// there is no name, because there is always an email; falling back again to
// "?" only when there is neither, which is the unresolved state the caller
// should be rendering a Skeleton for anyway.
export function initialsFrom(displayName: string, email: string): string {
  const words = displayName.trim().split(/\s+/).filter(Boolean);
  if (words.length > 0) {
    const first = words[0]?.[0] ?? "";
    const last = words.length > 1 ? (words[words.length - 1]?.[0] ?? "") : "";
    const initials = (first + last).toUpperCase();
    if (initials !== "") return initials;
  }
  const letter = email.trim()[0];
  return letter === undefined ? "?" : letter.toUpperCase();
}

export function Avatar({
  displayName = "",
  email = "",
  size = "md",
}: {
  displayName?: string;
  email?: string;
  size?: AvatarSize;
}): ReactNode {
  return (
    <span
      aria-hidden="true"
      data-avatar=""
      className={
        "inline-flex shrink-0 select-none items-center justify-center rounded-full " +
        `bg-accent-subtle font-medium text-fg ${SIZE[size]}`
      }
    >
      {initialsFrom(displayName, email)}
    </span>
  );
}
