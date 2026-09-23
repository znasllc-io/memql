import { useEffect, useRef } from "react";
import { Plus } from "lucide-react";

import { Button, Caption, Notice, RecordList, RecordRow, RefreshButton, Subhead, listCount } from "../../../../../kit";
import { shortRepo, sourceLabel, type PackageRow } from "../../../packages/rows";

/** The caller supplies the authorized, eligible sources for the chosen account.
 * Saved sources use their own credentials; GitHub connection belongs to Add source. */
export function SavedSourcePicker({
  sources,
  selectedId,
  onChoose,
  onAdd,
  canAdd,
  busy,
  state = "live",
  error,
  onRetry,
  focusId,
}: {
  sources: readonly PackageRow[];
  selectedId: string;
  onChoose: (source: PackageRow) => void;
  onAdd: () => void;
  canAdd: boolean;
  busy: boolean;
  state?: string;
  error?: string | null;
  onRetry?: () => void;
  /** Return keyboard focus to the source whose save just completed. */
  focusId?: string;
}) {
  const region = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!busy && focusId && focusId === selectedId) region.current?.querySelector<HTMLButtonElement>('button[aria-pressed="true"]')?.focus();
  }, [focusId, selectedId, busy]);
  const settled = state === "live" && !error;
  const loading = !error && state === "seeding";
  return <section ref={region} aria-label="Saved sources" aria-busy={loading || busy || undefined}>
    <div className="os-record-heading">
      <Subhead meta={listCount({ state, error, rows: sources })}>Sources</Subhead>
      {canAdd ? <Button onClick={onAdd} disabled={busy}><Plus size={14} aria-hidden /> Add source</Button> : null}
    </div>
    {error ? <Notice tone="error" sentence="Sources could not be read." detail={error} /> : null}
    {sources.length > 0 ? null : error ? null : loading ? <Caption>Reading sources…</Caption>
      : !settled ? <Caption>Sources are unavailable. Read them again to choose a source.</Caption>
      : sources.length === 0 ? <Caption>No saved sources are available for this account.</Caption>
      : null}
    {sources.length > 0 ? <RecordList as="ul" label="Sources">
        {sources.map((source) => {
          const chosen = source.id === selectedId;
          return <RecordRow
            key={source.id}
            name={source.name.trim() || shortRepo(source.repoUrl)}
            secondary={sourceLabel(source)}
            selected={chosen}
            current={chosen}
            state={chosen ? "chosen" : undefined}
            tone={chosen ? "accent" : "muted"}
            disabled={busy}
            onOpen={() => onChoose(source)}
          />;
        })}
      </RecordList> : null}
    {!settled && !loading && onRetry ? <RefreshButton label="Refresh sources" onClick={onRetry} busy={busy} /> : null}
  </section>;
}
