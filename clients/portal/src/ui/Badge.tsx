import type { ReactNode } from "react";

// Status vocabulary: a bordered pill and a dot, riding the same three
// families src/styles/status.css keys lifecycle values onto. Colour appears
// on the BORDER and FILL, never as the only carrier of meaning -- the label
// says the word, the family tints it.

export type StatusTone = "ok" | "warn" | "danger" | "neutral";

const BADGE: Record<StatusTone, string> = {
  ok: "border-ok bg-ok-subtle",
  warn: "border-warn bg-warn-subtle",
  danger: "border-danger bg-danger-subtle",
  neutral: "border-line bg-surface",
};

const DOT: Record<StatusTone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  danger: "bg-danger",
  neutral: "bg-subtle",
};

export function Badge({
  tone = "neutral",
  children,
}: {
  tone?: StatusTone;
  children: ReactNode;
}): ReactNode {
  return (
    <span
      className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs text-fg ${BADGE[tone]}`}
    >
      {children}
    </span>
  );
}

export function StatusDot({
  tone = "neutral",
  pulse = false,
  label,
}: {
  tone?: StatusTone;
  // Pulses for in-between states (connecting); static otherwise, and always
  // static under prefers-reduced-motion.
  pulse?: boolean;
  // Screen-reader text when the dot stands alone; omit when a visible label
  // sits beside it.
  label?: string;
}): ReactNode {
  return (
    <span
      {...(label === undefined ? { "aria-hidden": true } : { role: "img", "aria-label": label })}
      className={
        `inline-block h-2 w-2 rounded-full ${DOT[tone]}` +
        (pulse ? " animate-pulse motion-reduce:animate-none" : "")
      }
    />
  );
}
