import { useCallback, useMemo, useState } from "react";
import { buildLogsTail } from "@znasllc-io/memql-sdk-core/client";

import { Head, Notice, Refine, useNow, type RefineChip } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { LevelFloorChoice, WindowChoice } from "../../logs/facets";
import {
  DEFAULT_FILTERS,
  constraintsOf,
  isNarrowed,
  lineCount,
  toTailArgs,
  windowBounds,
  windowLabel,
  windowPhrase,
  withinWindow,
  type LogFilters,
  type LogScope,
} from "../../logs/filters";
import { TailView } from "../../logs/TailView";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../logs/useDebouncedValue";
import { useLogSources } from "../../logs/useLogSearch";
import { useLogTail } from "../../logs/useLogTail";
import { STREAM_WINDOWS, type LogsSettings } from "./settings";
import { SourceFacets } from "./SourceFacets";

// The Stream: the whole store, following (spec H "The Logs app").
//
// The same reading `AppLogsSection` makes, with no scope and with the
// cluster's own sources as facets; everything below the Head is the shared
// `TailView`. The sources come from `logsSources` over the stream window,
// keyed on the PRESET rather than on the window's instant bounds -- the
// clock ticks every five seconds here, and a catalogue re-read on every tick
// would be a second stream nobody asked for.

/** The whole store: no app, no concept. */
const WHOLE_STORE: LogScope = {};

export function StreamSection({ settings }: { settings: LogsSettings }) {
  const connection = useOsConnection();
  const now = useNow(5_000);
  const [filters, setFilters] = useState<LogFilters>(() => ({
    ...DEFAULT_FILTERS,
    levelFloor: settings.levelFloor,
    window: settings.streamWindow,
  }));
  const [follow, setFollow] = useState(true);
  const [selectedId, setSelectedId] = useState("");
  const patch = useCallback((next: Partial<LogFilters>) => setFilters((held) => ({ ...held, ...next })), []);

  const text = useDebouncedValue(filters.text, TEXT_DEBOUNCE_MS);
  const args = useMemo(() => toTailArgs(WHOLE_STORE, { ...filters, text }), [filters, text]);
  const viewKey = buildLogsTail(args);
  const tail = useLogTail({ args, viewKey, paused: !follow });

  // Read once per window choice. `windowBounds` against a fresh clock at
  // that moment; the memo's only dependency is the preset.
  const windowPreset = filters.window;
  const bounds = useMemo(() => windowBounds({ window: windowPreset, from: "", to: "" }, new Date()), [windowPreset]);
  const sources = useLogSources(bounds, `stream:${windowPreset}`);

  const visible = useMemo(() => withinWindow(tail.rows, filters.window, now), [tail.rows, filters.window, now]);
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
      <Head title="Stream" meta={`${windowLabel(filters.window)} · ${lineCount(visible.length)}`}>
        <Refine
          search={filters.text}
          onSearch={(next) => patch({ text: next })}
          chips={chips}
          label="Refine stream"
          placeholder="Search messages"
        >
          <LevelFloorChoice value={filters.levelFloor} onChange={(levelFloor) => patch({ levelFloor })} />
          <WindowChoice value={filters.window} onChange={(window) => patch({ window })} options={STREAM_WINDOWS} />
          <SourceFacets sources={sources} filters={filters} patch={patch} idPrefix="logs-stream" />
        </Refine>
      </Head>

      {connection === null ? (
        <Notice sentence="Not connected to the cluster." next="Lines follow the moment the connection is back." />
      ) : (
        <>
          {sources.state === "error" ? (
            <Notice tone="warn" sentence="The source list could not be read." detail={sources.error} />
          ) : null}
          <TailView
            tail={tail}
            rows={visible}
            follow={follow}
            onFollowChange={setFollow}
            selectedId={selectedId}
            onSelect={setSelectedId}
            onSubject={narrowTo}
            now={now}
            density={settings.density}
            label="Log stream"
            id="os-logs-stream"
            narrowed={isNarrowed(filters)}
            emptySentence={`Nothing recorded ${windowPhrase(filters.window)}.`}
            emptyHint="Every node writes here as it runs; this view follows."
          />
        </>
      )}
    </div>
  );
}
