import type { ReactNode } from "react";
import { formatRelative } from "@znasllc-io/memql-view-kit";

import { Button } from "../ui";
import type { PageVersion } from "./overrides";

// The version strip: Original / v1 / v2 / ... (epic memql#4661, task
// memql#4668).
//
// ===========================================================================
// NOTHING IS EVER DESTROYED
// ===========================================================================
// A write in MemQL is an append, so every version a page has ever had is still
// there. Which means "revert" does not have to mean "undo": selecting an older
// version PREVIEWS it, and "Use this version" RE-WRITES that arrangement as
// the newest one. The history grows; it never shrinks. A person who reverts
// and then changes their mind reverts again.
//
// SELECTING DOES NOT WRITE. A strip that saved on click would make browsing
// your own history a way to lose it, and the two gestures -- look at, commit
// to -- are far enough apart in consequence to be far apart on screen.
//
// ORIGINAL IS ALWAYS THERE and always first. It is the page's seed, it has no
// row, and it costs nothing to keep -- which is what makes "you can always get
// back to the page as shipped" true rather than aspirational.

export interface VersionStripProps {
  versions: readonly PageVersion[];
  selected: number;
  onSelect: (version: number) => void;
  // Re-write the selected version as the newest. Absent while a write is in
  // flight or when the person cannot write.
  onUse?: () => void;
  busy?: boolean;
}

export function VersionStrip({
  versions,
  selected,
  onSelect,
  onUse,
  busy = false,
}: VersionStripProps): ReactNode {
  // A page nobody has regenerated has exactly one version -- Original -- and a
  // strip offering one choice is chrome that explains nothing. It appears when
  // there is something to choose BETWEEN.
  if (versions.length < 2) return null;

  const newest = versions[versions.length - 1]?.version ?? 0;
  const previewing = selected !== newest;

  return (
    <div
      className="flex flex-wrap items-center gap-x-2 gap-y-1"
      role="group"
      aria-label="Page versions"
    >
      {versions.map((version) => {
        const active = version.version === selected;
        return (
          <button
            key={version.version}
            type="button"
            onClick={() => onSelect(version.version)}
            aria-current={active ? "true" : undefined}
            title={
              version.createdAt === ""
                ? "The page as it ships"
                : `Regenerated ${formatRelative(new Date(version.createdAt))}`
            }
            className={
              active
                ? "rounded-full border border-accent bg-accent-subtle px-2.5 py-0.5 text-xs font-medium text-fg"
                : "rounded-full border border-line px-2.5 py-0.5 text-xs text-muted hover:text-fg"
            }
          >
            {version.label}
          </button>
        );
      })}

      {/* Offered only while PREVIEWING. Re-writing the newest version as the
          newest version is a write that changes nothing and adds a version to
          the history, so the control that would do it is absent rather than
          disabled. */}
      {previewing && onUse !== undefined ? (
        <Button size="xs" tone="primary" onClick={onUse} disabled={busy}>
          {busy ? "Saving…" : "Use this version"}
        </Button>
      ) : null}
    </div>
  );
}
