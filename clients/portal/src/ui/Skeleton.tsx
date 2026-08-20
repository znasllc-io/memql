import type { ReactNode } from "react";

// Skeletons, not spinners. A loading surface renders bars SHAPED LIKE THE
// CONTENT so the layout never jumps when data lands -- a spinner says "wait"
// while a skeleton says "this is what is coming, here". Variants match the
// portal's three content shapes: a row list, a key/value detail pane, and a
// text line.
//
// The shimmer respects prefers-reduced-motion via Tailwind's motion-reduce
// variant: static blocks, same shapes, no pulse.

const BLOCK = "animate-pulse rounded bg-raised motion-reduce:animate-none";

export function Skeleton({
  variant = "text",
  // rows for the row-list / kv variants; ignored by text.
  rows = 5,
  // width utility for the text variant, e.g. "w-40".
  width = "w-32",
}: {
  variant?: "text" | "rows" | "kv" | "stat";
  rows?: number;
  width?: string;
}): ReactNode {
  if (variant === "rows") {
    return (
      <div aria-hidden="true" data-skeleton="rows" className="flex flex-col gap-2 p-3">
        {Array.from({ length: rows }, (_, i) => (
          <div key={i} className="flex items-center gap-3">
            <div className={`${BLOCK} h-4 w-4 shrink-0 rounded-full`} />
            <div className={`${BLOCK} h-4 flex-1`} />
            <div className={`${BLOCK} h-4 w-16 shrink-0`} />
          </div>
        ))}
      </div>
    );
  }
  if (variant === "kv") {
    return (
      <div aria-hidden="true" data-skeleton="kv" className="flex flex-col gap-2.5">
        {Array.from({ length: rows }, (_, i) => (
          <div key={i} className="flex items-center gap-4">
            <div className={`${BLOCK} h-3.5 w-24 shrink-0`} />
            <div className={`${BLOCK} h-3.5 flex-1`} />
          </div>
        ))}
      </div>
    );
  }
  if (variant === "stat") {
    return (
      <div aria-hidden="true" data-skeleton="stat" className="flex flex-col gap-2">
        <div className={`${BLOCK} h-3 w-16`} />
        <div className={`${BLOCK} h-7 w-24`} />
      </div>
    );
  }
  return <div aria-hidden="true" data-skeleton="text" className={`${BLOCK} h-4 ${width}`} />;
}
