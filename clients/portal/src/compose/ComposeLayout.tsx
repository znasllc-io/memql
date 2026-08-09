import type { ReactNode } from "react";

// The chrome a composed view is drawn in.
//
// Deliberately the same grammar as the predefined views (see
// clients/portal/src/views/ViewLayout.tsx for the long-form reasoning): a
// header that names the population, then BANDS -- a reading, a shape, a roll
// -- each a small-caps caption on a hairline rule running to the pane's edge,
// not a boxed card. A composed view should feel like a sibling of the five
// designed ones, because it IS one: the same elements, the same bands, the
// same typography. The only difference is who chose the arrangement.
//
// Copied rather than imported for the reason ComposeElement gives: the
// predefined-view tree is a closed, guarded directory whose file list a repo-
// root test counts, and coupling a surface under parallel development to that
// list buys nothing. What must not diverge is the ELEMENT contract, and that
// lives in view-kit where both trees read it.

export function ComposeBand({
  title,
  meta,
  panel = false,
  children,
}: {
  // Omitted makes it the opening reading -- the numbers ARE the label, and
  // captioning a stat strip "Summary" adds a word and no information.
  title?: string;
  meta?: ReactNode;
  // Wrap the contents in a surface. On for an enumerable body (a table needs
  // an edge and its own horizontal scroll); off for a reading or a rail.
  panel?: boolean;
  children: ReactNode;
}): ReactNode {
  // The caption row appears when there is a caption OR chrome to hang on it.
  // A band with controls and no title is the composer's opening reading: the
  // numbers are their own label, and the move/remove buttons still have to be
  // reachable.
  return (
    <section className="min-w-0">
      {title === undefined && meta === undefined ? null : (
        <div className="mb-3 flex items-baseline gap-3">
          <h3 className="shrink-0 text-xs font-semibold tracking-wide text-muted uppercase">
            {title ?? ""}
          </h3>
          <span aria-hidden="true" className="h-px min-w-4 flex-1 bg-line" />
          {meta === undefined ? null : (
            <span className="shrink-0 text-xs text-subtle">{meta}</span>
          )}
        </div>
      )}
      {panel ? (
        <div className="overflow-x-auto rounded-lg border border-line bg-surface p-1">
          {children}
        </div>
      ) : (
        children
      )}
    </section>
  );
}

export function ComposeButton({
  onClick,
  disabled = false,
  tone = "quiet",
  title,
  children,
}: {
  onClick: () => void;
  disabled?: boolean;
  // "quiet" for the many small controls, "accent" for the one commitment on
  // the page (save), "danger" for the one that cannot be undone.
  tone?: "quiet" | "accent" | "danger";
  title?: string;
  children: ReactNode;
}): ReactNode {
  const toneClass =
    tone === "danger"
      ? "border-danger bg-danger-subtle text-fg hover:bg-danger hover:text-accent-fg"
      : tone === "accent"
        ? "border-accent bg-accent text-accent-fg hover:opacity-90"
        : "border-line bg-surface text-fg hover:bg-raised";
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      {...(title === undefined ? {} : { title })}
      className={
        "rounded border px-2.5 py-1 text-xs font-medium disabled:cursor-not-allowed disabled:opacity-40 " +
        toneClass
      }
    >
      {children}
    </button>
  );
}

// SectionHeader names one concept's section of a composed view.
//
// A composed view may stack several concepts, so unlike a predefined view the
// concept id belongs INSIDE the page rather than only in its header: it is the
// one thing that tells a reader which rows a section is about, and it is the
// address they would paste into a query. Monospace, because ids are read
// character by character.
export function SectionHeader({
  conceptId,
  entity,
  meta,
  actions,
}: {
  conceptId: string;
  entity: string;
  meta?: ReactNode;
  actions?: ReactNode;
}): ReactNode {
  return (
    <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 border-b border-line pb-3">
      <div className="min-w-0">
        <p className="font-mono text-xs break-all text-subtle">{conceptId}</p>
        <h2 className="mt-0.5 text-base font-semibold tracking-tight">{entity}</h2>
      </div>
      <div className="flex shrink-0 flex-col items-end gap-2">
        {actions}
        {meta}
      </div>
    </div>
  );
}

// PopulationMeta is the honest state of the keyset walk behind a section.
//
// Every reading in a composed view -- the counts, the proportions -- describes
// THE ROWS LOADED, not the whole concept. Saying so is the difference between
// a dashboard and a lie, and it matters more in a composed view than a
// designed one: the person who composed it chose a stat strip precisely
// because they wanted a number.
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
        <span className="text-xs text-fg">
          {count > 0 ? `Paging stopped after ${count}` : "Could not read rows"}: {error}
        </span>
        <ComposeButton onClick={onRetry}>Try again</ComposeButton>
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
        <ComposeButton onClick={onLoadMore}>Load more</ComposeButton>
      </div>
    );
  }
  return (
    <span className="text-xs text-subtle">
      {count === 0 ? "Nothing here yet" : `All ${count} loaded`}
    </span>
  );
}
