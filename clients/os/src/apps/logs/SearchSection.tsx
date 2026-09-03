import { useCallback, useEffect, useMemo, useState } from "react";
import { buildLogsSearch, type LogsSearchArgs } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, CopyValue, Field, Head, Notice, Refine, useNow, type RefineChip } from "../../kit";
import { formatFreshness } from "../../kit/format";
import { useOsConnection } from "../../live/connection";
import { LevelFloorChoice, WindowChoice } from "../../logs/facets";
import {
  DEFAULT_FILTERS,
  WINDOW_PRESETS,
  constraintsOf,
  isNarrowed,
  lineCount,
  subjectIntentOf,
  toTailArgs,
  windowBounds,
  windowLabel,
  windowPhrase,
  type LogFilters,
  type LogScope,
} from "../../logs/filters";
import { LogDetail } from "../../logs/LogDetail";
import { LogLine, ROW_HEIGHT } from "../../logs/LogLine";
import { conceptWord } from "../../logs/rows";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../logs/useDebouncedValue";
import { useLogSearch, useLogSources } from "../../logs/useLogSearch";
import { WindowedList } from "../../logs/WindowedList";
import type { OsAppProps } from "../../system/registry";
import type { LogsSettings } from "./settings";
import { SourceFacets } from "./SourceFacets";

// Search: a window and every facet, newest first, paged older by keyset
// (spec H "The Logs app").
//
// AN ON-DEMAND READ THAT SAYS WHEN IT WAS READ. A preset window is anchored
// to the moment the question was asked -- "the last 24 hours" as of the read,
// not a window that slides under the rows while somebody reads them -- and
// "Read again" re-anchors it. The sources catalogue is keyed on the same
// anchor, so the two never describe different windows.
//
// A `{ subject, subjectConcept }` intent lands here narrowed, with the
// subject shown as a copyable chip: it is one unbreakable id the reader is
// about to paste into a cockpit command or a search elsewhere.

const WHOLE_STORE: LogScope = {};

export function SearchSection({
  settings,
  intent,
  consumeIntent,
}: {
  settings: LogsSettings;
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
}) {
  const connection = useOsConnection();
  const now = useNow(15_000);
  const [filters, setFilters] = useState<LogFilters>(() => ({
    ...DEFAULT_FILTERS,
    levelFloor: settings.levelFloor,
    window: "24h",
  }));
  const [selectedId, setSelectedId] = useState("");
  const [generation, setGeneration] = useState(0);
  const patch = useCallback((next: Partial<LogFilters>) => setFilters((held) => ({ ...held, ...next })), []);

  useEffect(() => {
    if (!intent) return;
    const narrowed = subjectIntentOf(intent.payload);
    if (narrowed === null) return;
    setFilters((held) => ({ ...held, subject: narrowed.subject, subjectConcept: narrowed.subjectConcept }));
    setSelectedId("");
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);

  const text = useDebouncedValue(filters.text, TEXT_DEBOUNCE_MS);

  // The window is anchored when it is CHOSEN (or re-read), never on the clock.
  const windowKey = `${filters.window}|${filters.from}|${filters.to}|${generation}`;
  const bounds = useMemo(
    () => windowBounds(filters, new Date()),
    // The key is the dependency: the three fields it names are what change
    // the window, and `generation` is the re-read.
    [windowKey],
  );
  const args = useMemo<LogsSearchArgs | null>(
    () =>
      bounds === null
        ? null
        : {
            windowStart: bounds.start.toISOString(),
            windowEnd: bounds.end.toISOString(),
            ...toTailArgs(WHOLE_STORE, { ...filters, text }),
          },
    [bounds, filters, text],
  );
  const viewKey = args === null ? "" : buildLogsSearch(args);
  const search = useLogSearch(args, viewKey);
  const sources = useLogSources(bounds, bounds === null ? "" : `search:${windowKey}`);

  const selected = selectedId === "" ? undefined : search.rows.find((row) => row.id === selectedId);
  const narrowed = isNarrowed(filters);
  const chips: RefineChip[] = constraintsOf(filters).map((c) => ({
    id: c.id,
    label: c.label,
    onRemove: () => patch(c.clear),
  }));

  function narrowTo(subject: string, subjectConcept: string): void {
    patch({ subject, subjectConcept });
    setSelectedId("");
  }

  return (
    <div className="os-app-stack os-logs" data-density={settings.density}>
      <Head title="Search" meta={`${windowLabel(filters.window)} · ${lineCount(search.rows.length)}`}>
        <Refine
          search={filters.text}
          onSearch={(next) => patch({ text: next })}
          chips={chips}
          label="Refine search"
        >
          <WindowChoice value={filters.window} onChange={(window) => patch({ window })} options={WINDOW_PRESETS} />
          {filters.window === "custom" ? (
            <div className="os-logs-range">
              <Field label="From">
                <label className="os-sr-only" htmlFor="logs-search-from">
                  From
                </label>
                <input
                  id="logs-search-from"
                  className="os-input"
                  type="datetime-local"
                  value={filters.from}
                  onChange={(event) => patch({ from: event.target.value })}
                />
              </Field>
              <Field label="To">
                <label className="os-sr-only" htmlFor="logs-search-to">
                  To
                </label>
                <input
                  id="logs-search-to"
                  className="os-input"
                  type="datetime-local"
                  value={filters.to}
                  onChange={(event) => patch({ to: event.target.value })}
                />
              </Field>
            </div>
          ) : null}
          <LevelFloorChoice value={filters.levelFloor} onChange={(levelFloor) => patch({ levelFloor })} />
          <SourceFacets sources={sources} filters={filters} patch={patch} idPrefix="logs-search" />
        </Refine>
      </Head>

      {filters.subject.trim() === "" ? null : (
        <div className="os-logs-subject-line" role="group" aria-label="Subject">
          <Chip tone="accent">{conceptWord(filters.subjectConcept)}</Chip>
          <CopyValue value={filters.subject.trim()} label="Subject" />
        </div>
      )}

      {connection === null ? (
        <Notice sentence="Not connected to the cluster." next="The search runs the moment the connection is back." />
      ) : bounds === null ? (
        <Caption>Pick a From and a To, with To after From, to search a custom window.</Caption>
      ) : (
        <>
          <div className="os-logs-scope">
            <span className="os-caption">
              Newest first
              {search.readAt === null
                ? " · not read yet"
                : ` · read ${formatFreshness(search.readAt.toISOString(), now)}`}
            </span>
            <Button onClick={() => setGeneration((g) => g + 1)} busy={search.state === "reading"} busyLabel="Reading">
              Read again
            </Button>
          </div>

          {search.state === "error" ? (
            <Notice tone="error" sentence="The search could not be read." detail={search.error} />
          ) : null}
          {sources.state === "error" ? (
            <Notice tone="warn" sentence="The source list could not be read." detail={sources.error} />
          ) : null}

          {search.rows.length === 0 ? (
            search.state === "reading" ? (
              <Caption>Reading from the cluster.</Caption>
            ) : search.state === "ready" ? (
              <div className="os-logs-empty">
                <p className="os-logs-empty-line">
                  {narrowed ? "No lines match." : `Nothing recorded ${windowPhrase(filters.window)}.`}
                </p>
                <Caption>
                  {narrowed ? "Clear a facet or widen the window to see more." : "Widen the window to look further back."}
                </Caption>
              </div>
            ) : null
          ) : (
            <>
              <div className="os-logs-list">
                <WindowedList
                  rows={search.rows}
                  rowHeight={ROW_HEIGHT[settings.density]}
                  renderRow={(row) => <LogLine row={row} now={now} onSubject={narrowTo} />}
                  rowId={(row) => row.id}
                  selectedId={selectedId}
                  onSelect={setSelectedId}
                  follow={false}
                  onFollowChange={() => {}}
                  label="Search results"
                  id="os-logs-search"
                />
              </div>
              <div className="os-logs-older">
                {search.exhausted ? (
                  <Caption>That is every line in the window.</Caption>
                ) : (
                  <Button onClick={search.loadOlder} busy={search.loadingOlder} busyLabel="Reading">
                    Older lines
                  </Button>
                )}
              </div>
            </>
          )}

          {selected ? <LogDetail row={selected} onClose={() => setSelectedId("")} /> : null}
        </>
      )}
    </div>
  );
}
