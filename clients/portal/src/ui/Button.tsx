import type { MouseEventHandler, ReactNode } from "react";

// The button. One component, three tones, two sizes -- replacing the six
// distinct padding recipes and ~40 ad-hoc class strings the survey found.
//
//   primary  the one commitment on a screen (save, sign in). Accent-filled;
//            most screens have zero or one.
//   quiet    everything else -- navigation-ish actions, load-more, refresh.
//            The data is supposed to out-rank the buttons on a console.
//   danger   the action that cannot be undone. Bordered in the danger family
//            at rest and fills on hover, so the screen is not shouting until
//            the pointer commits.
//
// `busy` disables and swaps the label for busyLabel (falling back to the
// children), so a double-submit is structurally impossible and the button
// itself says what is happening. type defaults to "button" -- a bare <button>
// inside a form submits it, which is never what a toolbar action means.

export type ButtonTone = "primary" | "quiet" | "danger";
export type ButtonSize = "sm" | "xs";

const TONE: Record<ButtonTone, string> = {
  primary: "border-accent bg-accent text-accent-fg hover:opacity-90",
  quiet: "border-line bg-surface text-fg hover:bg-raised",
  danger: "border-danger bg-danger-subtle text-fg hover:bg-danger hover:text-accent-fg",
};

const SIZE: Record<ButtonSize, string> = {
  sm: "px-3 py-1.5 text-sm",
  xs: "px-2.5 py-1 text-xs",
};

// Shared by Button and ButtonLink so the two cannot drift: a <button> and an
// <a> that are meant to look identical must be built from one class recipe,
// not two copies someone edits out of sync.
function classesFor(tone: ButtonTone, size: ButtonSize): string {
  return (
    "inline-flex items-center gap-1.5 rounded border font-medium " +
    `disabled:cursor-not-allowed disabled:opacity-40 ${SIZE[size]} ${TONE[tone]}`
  );
}

export function Button({
  tone = "quiet",
  size = "sm",
  type = "button",
  busy = false,
  busyLabel,
  disabled = false,
  onClick,
  title,
  children,
}: {
  tone?: ButtonTone;
  size?: ButtonSize;
  type?: "button" | "submit";
  busy?: boolean;
  busyLabel?: string;
  disabled?: boolean;
  onClick?: MouseEventHandler<HTMLButtonElement>;
  title?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <button
      type={type}
      disabled={busy || disabled}
      {...(onClick === undefined ? {} : { onClick })}
      {...(title === undefined ? {} : { title })}
      className={classesFor(tone, size)}
    >
      {busy && busyLabel !== undefined ? busyLabel : children}
    </button>
  );
}

// A link that looks like a Button. For navigations that are not clicks with
// side effects -- a deep link hands the browser a URL, and an anchor lets the
// browser own the "open this application?" gesture.
export function ButtonLink({
  tone = "quiet",
  size = "sm",
  href,
  title,
  target,
  rel,
  children,
}: {
  tone?: ButtonTone;
  size?: ButtonSize;
  href: string;
  title?: string;
  target?: string;
  rel?: string;
  children: ReactNode;
}): ReactNode {
  return (
    <a href={href} title={title} target={target} rel={rel} className={classesFor(tone, size)}>
      {children}
    </a>
  );
}
