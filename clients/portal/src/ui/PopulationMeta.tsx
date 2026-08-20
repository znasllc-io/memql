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

export function PopulationMeta({
  count,
  status,
  error,
  onLoadMore,
  onRetry,
}: {
  count: number;
  status: "idle" | "loading" | "ready" | "exhausted" | "failed";
  error: string;
  onLoadMore: () => void;
  onRetry: () => void;
}): ReactNode {
  if (status === "failed") {
    return (
      <div className="flex items-center gap-2">
        {/* Said in words at full contrast, not in a hue: this sits in a
            header at 12px, below the size where the danger tint alone can
            carry the message. */}
        <span className="text-xs text-fg">
          {count > 0 ? `Paging stopped after ${count}` : "Could not read rows"}: {error}
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
