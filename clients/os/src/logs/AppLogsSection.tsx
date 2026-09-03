import { useCallback, useEffect, useMemo, useState } from "react";
import { buildLogsTail } from "@znasllc-io/memql-sdk-core/client";

import { Head, Notice, Refine, useNow, type RefineChip } from "../kit";
import { useOsConnection } from "../live/connection";
import type { OsAppProps } from "../system/registry";
import { LevelFloorChoice, WindowChoice } from "./facets";
import {
  DEFAULT_FILTERS,
  TAIL_WINDOWS,
  constraintsOf,
  isNarrowed,
  lineCount,
  subjectIntentOf,
  toTailArgs,
  windowLabel,
  windowPhrase,
  withinWindow,
  type LogFilters,
  type LogScope,
} from "./filters";
import { TailView } from "./TailView";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "./useDebouncedValue";
import { useLogTail } from "./useLogTail";

// The Logs section every app carries (epic memql#4895, spec H "The
// convention" and "AppLogsSection").
//
// IMPORTED BY EVERY APP, THE WAY `apps/accounts/tie.tsx` IS. It lives in
// `src/logs/` rather than in the kit because it is one domain's surface --
// a reading of the log store -- that every app happens to mount, not
// vocabulary. It reads the app's SLICE: lines tagged with the app id OR
// whose subject concept is one the app owns, which the engine ORs into one
// scope predicate (spec D). Everything else -- the level floor, the window,
// the search text, a subject -- ANDs on top.
//
// It follows a stream. There is no arrival cue here on purpose: a log is
// nothing but arrivals, and the README's rule is that a heartbeat is not
// news. New lines land at the bottom and the list follows them; scrolling up
// pauses the following, the count of what arrived accumulates, and one pill
// offers the way back (all of that is `TailView`, shared with the Logs
// app's Stream).

export interface AppLogsSectionProps {
  /** The app's id, exactly as the manifest declares it. */
  app: string;
  /** The concepts this app owns -- generated constants, never composed. */
  subjectConcepts?: readonly string[];
  intent?: OsAppProps["intent"];
  consumeIntent?: OsAppProps["consumeIntent"];
}

/** The section's density. Fixed: an app's Logs section reads at the
 *  comfortable height; the Logs app's own setting is the Logs app's. */
const DENSITY = "comfortable";

export function AppLogsSection({ app, subjectConcepts = [], intent, consumeIntent }: AppLogsSectionProps) {
  const connection = useOsConnection();
  // One clock for every elapsed time in the section, and for the window
  // fold, so a line ages out of "the last hour" on the clock rather than on
  // the next arrival.
  const now = useNow(5_000);
  const [filters, setFilters] = useState<LogFilters>(DEFAULT_FILTERS);
  const [follow, setFollow] = useState(true);
  const [selectedId, setSelectedId] = useState("");

  const patch = useCallback((next: Partial<LogFilters>) => setFilters((held) => ({ ...held, ...next })), []);

  const conceptKey = subjectConcepts.join(",");
  const scope = useMemo<LogScope>(
    () => ({ apps: [app], subjectConcepts: conceptKey === "" ? [] : conceptKey.split(",") }),
    [app, conceptKey],
  );
  const text = useDebouncedValue(filters.text, TEXT_DEBOUNCE_MS);
  const args = useMemo(() => toTailArgs(scope, { ...filters, text }), [scope, filters, text]);
  // The rendered call IS the reading: when it changes, the tail re-baselines.
  const viewKey = buildLogsTail(args);
  const tail = useLogTail({ args, viewKey, paused: !follow });

  // A `{ subject, subjectConcept }` intent lands narrowed, consumed by id so
  // acting on a stale render can never eat a newer instruction. Any other
  // payload is the app's own and is left for it.
  useEffect(() => {
    if (!intent) return;
    const narrowed = subjectIntentOf(intent.payload);
    if (narrowed === null) return;
    setFilters((held) => ({ ...held, subject: narrowed.subject, subjectConcept: narrowed.subjectConcept }));
    setSelectedId("");
    consumeIntent?.(intent.id);
  }, [intent, consumeIntent]);

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
    <div className="os-app-stack os-logs" data-density={DENSITY}>
      {/* The Head names the window and the count of what is SHOWN (rule 7:
          the scope is said once), and the facets sit behind Refine (rule 2). */}
      <Head title="Logs" meta={`${windowLabel(filters.window)} · ${lineCount(visible.length)}`}>
        <Refine
          search={filters.text}
          onSearch={(next) => patch({ text: next })}
          chips={chips}
          label="Refine logs"
          placeholder="Search messages"
        >
          <LevelFloorChoice value={filters.levelFloor} onChange={(levelFloor) => patch({ levelFloor })} />
          <WindowChoice value={filters.window} onChange={(window) => patch({ window })} options={TAIL_WINDOWS} />
        </Refine>
      </Head>

      {connection === null ? (
        <Notice sentence="Not connected to the cluster." next="Lines follow the moment the connection is back." />
      ) : (
        <TailView
          tail={tail}
          rows={visible}
          follow={follow}
          onFollowChange={setFollow}
          selectedId={selectedId}
          onSelect={setSelectedId}
          onSubject={narrowTo}
          now={now}
          density={DENSITY}
          label={`Logs for ${app}`}
          id={`os-logs-${app}`}
          narrowed={isNarrowed(filters)}
          emptySentence={`Nothing recorded for this app ${windowPhrase(filters.window)}.`}
          emptyHint="Lines arrive as the app and the engine write them; this view follows."
        />
      )}
    </div>
  );
}
