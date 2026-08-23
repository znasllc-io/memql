import type { ReactNode } from "react";

import type { StatusTone } from "./Badge";

// A bordered note that says something the reader has to act on, or decide
// not to.
//
// The portal already had ErrorMessage (components/StatusMessage.tsx): one
// tone, `role="alert"`, for a request that died. What it did not have was the
// shape a WARNING needs -- a title, a sentence or two of consequence, and
// often a link to the remedy -- so the shared-mailbox note and the
// server-refusal messages on /me would each have hand-rolled a bordered div.
//
// TONE IS NOT THE ONLY CARRIER. The title says the thing; the family tints
// it. That is the same rule Badge follows, and it is why this takes a title
// rather than colouring a bare paragraph.
//
// `role="alert"` ONLY for danger. An alert interrupts a screen reader
// mid-sentence, which is right for "that write failed" and wrong for a
// standing observation about your mailbox that is true on every render --
// announcing the latter on every paint is how people learn to ignore the
// live region that matters.

const TONE: Record<StatusTone, string> = {
  ok: "border-ok bg-ok-subtle",
  warn: "border-warn bg-warn-subtle",
  danger: "border-danger bg-danger-subtle",
  neutral: "border-line bg-surface",
};

export function Callout({
  tone = "neutral",
  title,
  children,
}: {
  tone?: StatusTone;
  title: string;
  children?: ReactNode;
}): ReactNode {
  return (
    <div
      {...(tone === "danger" ? { role: "alert" } : {})}
      data-callout={tone}
      className={`rounded-lg border px-3 py-2 text-sm text-fg ${TONE[tone]}`}
    >
      <p className="font-medium">{title}</p>
      {children === undefined ? null : (
        <div className="mt-1 max-w-prose text-muted">{children}</div>
      )}
    </div>
  );
}
