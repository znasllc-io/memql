import type { ReactNode } from "react";

import { Button } from "./Button";

// PopulationMeta is the honest state of the keyset walk behind a surface.
//
// Every reading on a page -- the counts, the proportions -- describes THE
// ROWS LOADED, not the whole concept. Saying so is not a caveat, it is the
// difference between a dashboard and a lie: an operator who reads "3 owners"
// off a page that has loaded one of nine pages has been misinformed. So the
// count is always present and always says "loaded".
//
// The single implementation of what used to exist twice (views and composer,
// differing only in which button component they called). The strings are
// load-bearing -- tests and operators alike read them -- so they are exactly
// the ones both copies carried.

// THE `error` PROP IS GONE (memql#4653). It rendered the engine's own string
// into a 12px header line beside a button, where it was unreadable at that
// size and unusable to whoever read it. The same read's error still renders
// in full behind ErrorNotice's disclosure, in the page body, where there is
// room -- so nothing was lost, and callers no longer thread a string here to
// have it truncated.
export function PopulationMeta({
  count,
  status,
  onLoadMore,
  onRetry,
}: {
  count: number;
  status: "idle" | "loading" | "ready" | "exhausted" | "failed";
  onLoadMore: () => void;
  onRetry: () => void;
}): ReactNode {
  if (status === "failed") {
    return (
      <div className="flex items-center gap-2">
        {/* Said in words at full contrast, not in a hue: this sits in a
            header at 12px, below the size where the danger tint alone can
            carry the message. */}
        {/* THE RAW STRING IS NOT HERE (memql#4653). This sits in a header at
            12px beside a button; an engine error rendered into it is
            unreadable at that size and unusable to the person reading it.
            What survives is the fact and the remedy -- and the same read's
            error renders in full, behind ErrorNotice's disclosure, in the
            page body where there is room for it. */}
        <span className="text-xs text-fg">
          {count > 0 ? `Paging stopped after ${count} rows` : "Could not read rows"}.
        </span>
        <Button size="xs" onClick={onRetry}>
          Try again
        </Button>
      </div>
    );
  }
  if (status === "loading" || status === "idle") {
    return <span className="text-xs text-subtle">Loading… ({count} so far)</span>;
  }
  if (status === "ready") {
    return (
      <div className="flex items-center gap-2">
        <span className="text-xs text-subtle">{count} loaded, more available</span>
        <Button size="xs" onClick={onLoadMore}>
          Load more
        </Button>
      </div>
    );
  }
  return (
    <span className="text-xs text-subtle">
      {count === 0 ? "Nothing here yet" : `All ${count} loaded`}
    </span>
  );
}
