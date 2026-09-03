import { useCallback, useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Fact, Facts, Head, Notice, Panel, Subhead, roleAdmits, useNow } from "../../kit";
import { formatBytes, formatFreshness, formatMoment } from "../../kit/format";
import { boolOr, flatten } from "../../kit/rows";
import { useOsConnection } from "../../live/connection";
import { errorSentence } from "../../logs/errors";
import { LEVEL_FLOORS, levelFloorLabel } from "../../logs/filters";
import { windowLabel } from "../../logs/filters";
import {
  DEFAULT_LOGS_SETTINGS,
  LOGS_DENSITIES,
  LOGS_SECTIONS,
  STREAM_WINDOWS,
  type LogsSettings,
} from "./settings";

// The Logs app's settings: its own preferences, what this cluster keeps, and
// the archived days (spec H "The Logs app", L7, L8).
//
// "This cluster" is `logsStatus`, an on-demand read that says when it was
// read. "Archived days" is `logsArchiveList`, with "Bring back" offered to an
// OWNER and one sentence for everyone else on why the action is absent -- an
// absent control with no account of itself reads as something nobody built.
// A restore's reply, or its refusal, renders in a Notice beside the day it
// was asked of, never as a toast.

interface LogsStatus {
  retentionDays: number;
  level: string;
  maxLinesPerSecond: number;
  archiveConfigured: boolean;
  archiveContainer: string;
  written: number;
  dropped: number;
  droppedByReason: Record<string, number>;
  oldestAt: string;
  newestAt: string;
  rowEstimate: number;
}

function num(row: Row, key: string): number {
  const v = row[key];
  if (typeof v === "number" && Number.isFinite(v)) return v;
  if (typeof v === "string" && v.trim() !== "" && Number.isFinite(Number(v))) return Number(v);
  return 0;
}

function str(row: Row, key: string): string {
  const v = row[key];
  return typeof v === "string" ? v : "";
}

/** The status row, read defensively. Exported for the harness's sake. */
export function statusFromRow(raw: Row): LogsStatus {
  const row = flatten(raw);
  const reasons: Record<string, number> = {};
  const byReason = row.droppedByReason;
  if (byReason !== null && typeof byReason === "object" && !Array.isArray(byReason)) {
    for (const [reason, count] of Object.entries(byReason as Record<string, unknown>)) {
      if (typeof count === "number" && Number.isFinite(count)) reasons[reason] = count;
    }
  }
  return {
    retentionDays: num(row, "retentionDays"),
    level: str(row, "level"),
    maxLinesPerSecond: num(row, "maxLinesPerSecond"),
    archiveConfigured: boolOr(row, "archiveConfigured", false),
    archiveContainer: str(row, "archiveContainer"),
    written: num(row, "written"),
    dropped: num(row, "dropped"),
    droppedByReason: reasons,
    oldestAt: str(row, "oldestAt"),
    newestAt: str(row, "newestAt"),
    rowEstimate: num(row, "rowEstimate"),
  };
}

export interface ArchivedDay {
  day: string;
  nodeTypes: string[];
  objects: number;
  bytes: number;
}

/** One entry per DAY, folded from one row per object. Newest day first. */
export function archivedDaysFromRows(rows: Row[]): ArchivedDay[] {
  const byDay = new Map<string, ArchivedDay>();
  for (const raw of rows) {
    const row = flatten(raw);
    const day = str(row, "day").trim();
    if (day === "") continue;
    const held = byDay.get(day) ?? { day, nodeTypes: [], objects: 0, bytes: 0 };
    const nodeType = str(row, "nodeType").trim();
    if (nodeType !== "" && !held.nodeTypes.includes(nodeType)) held.nodeTypes.push(nodeType);
    held.objects += 1;
    held.bytes += num(row, "bytes");
    byDay.set(day, held);
  }
  return [...byDay.values()].sort((a, b) => b.day.localeCompare(a.day));
}

/** The reply of a restore, as a sentence. */
export function restoreSentence(raw: Row | null): string {
  if (raw === null) return "The day was brought back.";
  const row = flatten(raw);
  const restored = num(row, "restored");
  const skipped = num(row, "skipped");
  const objects = num(row, "objects");
  return `Brought back ${restored.toLocaleString()} ${restored === 1 ? "line" : "lines"} from ${objects.toLocaleString()} ${objects === 1 ? "object" : "objects"}; ${skipped.toLocaleString()} already present. The day is older than retention and is swept again at the next nightly run -- read it now.`;
}

function droppedWord(status: LogsStatus): string {
  const reasons = Object.entries(status.droppedByReason)
    .filter(([, count]) => count > 0)
    .map(([reason, count]) => `${reason} ${count.toLocaleString()}`)
    .join(", ");
  return reasons === "" ? status.dropped.toLocaleString() : `${status.dropped.toLocaleString()} (${reasons})`;
}

export function LogsSettingsSection({
  settings,
  update,
  actorRole,
}: {
  settings: LogsSettings;
  update: (patch: Partial<LogsSettings>) => void;
  actorRole: string;
}) {
  const connection = useOsConnection();
  const now = useNow();
  const isOwner = roleAdmits(actorRole, { min: "owner" });
  // OFFER ONLY WHAT THIS SESSION CAN OPEN, and never Settings itself.
  const offered = LOGS_SECTIONS.filter((s) => s.id !== "settings" && roleAdmits(actorRole, s.roles));

  const [status, setStatus] = useState<LogsStatus | null>(null);
  const [statusError, setStatusError] = useState("");
  const [statusReadAt, setStatusReadAt] = useState<Date | null>(null);
  const [days, setDays] = useState<ArchivedDay[]>([]);
  const [daysError, setDaysError] = useState("");
  const [daysReadAt, setDaysReadAt] = useState<Date | null>(null);
  const [generation, setGeneration] = useState(0);
  const [restoring, setRestoring] = useState("");
  const [restoreNote, setRestoreNote] = useState<{ day: string; tone: "info" | "error"; sentence: string; detail?: string } | null>(null);

  useEffect(() => {
    if (connection === null) {
      setStatus(null);
      setDays([]);
      return undefined;
    }
    const controller = new AbortController();
    let live = true;
    // Each read settles on its own: the archive list can refuse (no container
    // configured is a row that says so, but a refusal is still possible) while
    // the status read succeeds, and one Promise.all would let either decide
    // the other's fate.
    void (async () => {
      try {
        const result = await connection.query.logsStatus({}, { signal: controller.signal });
        if (!live) return;
        const row = result.rows()[0];
        setStatus(row === undefined ? null : statusFromRow(row));
        setStatusError("");
        setStatusReadAt(new Date());
      } catch (err) {
        if (!live || controller.signal.aborted) return;
        setStatusError(errorSentence(err));
      }
    })();
    void (async () => {
      try {
        const result = await connection.query.logsArchiveList({}, { signal: controller.signal });
        if (!live) return;
        setDays(archivedDaysFromRows(result.rows()));
        setDaysError("");
        setDaysReadAt(new Date());
      } catch (err) {
        if (!live || controller.signal.aborted) return;
        setDaysError(errorSentence(err));
      }
    })();
    return () => {
      live = false;
      controller.abort();
    };
  }, [connection, generation]);

  const bringBack = useCallback(
    async (day: string) => {
      if (connection === null) return;
      setRestoring(day);
      setRestoreNote(null);
      try {
        const result = await connection.query.logsArchiveRestore({ day });
        setRestoreNote({ day, tone: "info", sentence: restoreSentence(result.rows()[0] ?? null) });
      } catch (err) {
        // The server's own sentence, verbatim: for a non-owner who reached
        // this somehow it is the refusal, and a paraphrase would drop the
        // one fact that helps.
        setRestoreNote({ day, tone: "error", sentence: "The day was not brought back.", detail: errorSentence(err) });
      } finally {
        setRestoring("");
      }
    },
    [connection],
  );

  return (
    <div className="os-settings">
      <Head title="Logs settings" />
      <Panel label="Logs settings">
        <fieldset className="os-field-group">
          <legend>Open Logs on</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Default section">
            {offered.map((section) => (
              <button
                key={section.id}
                type="button"
                role="radio"
                aria-checked={settings.defaultSection === section.id}
                className="os-choice"
                onClick={() => update({ defaultSection: section.id === "search" ? "search" : "stream" })}
              >
                {section.name}
              </button>
            ))}
          </div>
          <p className="os-caption">
            Applies the next time a Logs window opens; it does not move the window you are looking at.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Density</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Density">
            {LOGS_DENSITIES.map((density) => (
              <button
                key={density}
                type="button"
                role="radio"
                aria-checked={settings.density === density}
                className="os-choice"
                onClick={() => update({ density })}
              >
                {density}
              </button>
            ))}
          </div>
          <p className="os-caption">
            A view setting, not a filter: it changes how tightly the lines pack and nothing about which
            lines are read or shown.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Show by default</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Level floor">
            {LEVEL_FLOORS.map((floor) => (
              <button
                key={floor}
                type="button"
                role="radio"
                aria-checked={settings.levelFloor === floor}
                className="os-choice"
                onClick={() => update({ levelFloor: floor })}
              >
                {levelFloorLabel(floor)}
              </button>
            ))}
          </div>
          <p className="os-caption">
            The level floor a Stream or Search starts on. Refine steers the session afterwards.
          </p>
        </fieldset>

        <fieldset className="os-field-group">
          <legend>Stream window</legend>
          <div className="os-choice-row" role="radiogroup" aria-label="Stream window">
            {STREAM_WINDOWS.map((window) => (
              <button
                key={window}
                type="button"
                role="radio"
                aria-checked={settings.streamWindow === window}
                className="os-choice"
                onClick={() => update({ streamWindow: window })}
              >
                {windowLabel(window)}
              </button>
            ))}
          </div>
          <p className="os-caption">How far back the Stream shows, and which sources it offers as facets.</p>
        </fieldset>
      </Panel>

      <Panel label="This cluster">
        <Subhead>This cluster</Subhead>
        {connection === null ? (
          <Caption>Not connected to the cluster.</Caption>
        ) : statusError !== "" ? (
          <Notice tone="error" sentence="The store's status could not be read." detail={statusError} />
        ) : status === null ? (
          <Caption>Reading from the cluster.</Caption>
        ) : (
          <>
            <Facts>
              <Fact label="Retention" value={`${status.retentionDays} ${status.retentionDays === 1 ? "day" : "days"}`} />
              <Fact
                label="Archive"
                value={
                  status.archiveConfigured
                    ? status.archiveContainer
                    : "No archive configured -- lines are kept until one is."
                }
                mono={status.archiveConfigured}
              />
              <Fact label="Store level" value={status.level} mono />
              <Fact label="Rate cap" value={`${status.maxLinesPerSecond.toLocaleString()} lines a second, per node`} />
              <Fact label="Written" value={status.written.toLocaleString()} />
              <Fact label="Dropped" value={droppedWord(status)} />
              <Fact label="Oldest line" value={status.oldestAt === "" ? "" : formatMoment(status.oldestAt)} />
              <Fact label="Newest line" value={status.newestAt === "" ? "" : formatMoment(status.newestAt)} />
              <Fact label="Lines kept" value={status.rowEstimate === 0 ? "" : `about ${status.rowEstimate.toLocaleString()}`} />
            </Facts>
            <div className="os-refresh-row">
              <Button onClick={() => setGeneration((g) => g + 1)}>Read again</Button>
              <span className="os-caption">
                {statusReadAt === null ? "" : `Read ${formatFreshness(statusReadAt.toISOString(), now)}. The counters are the answering node's since it started.`}
              </span>
            </div>
          </>
        )}
      </Panel>

      <Panel label="Archived days">
        <Subhead>Archived days</Subhead>
        {connection === null ? (
          <Caption>Not connected to the cluster.</Caption>
        ) : daysError !== "" ? (
          <Notice tone="error" sentence="The archive could not be listed." detail={daysError} />
        ) : daysReadAt === null ? (
          <Caption>Reading from the cluster.</Caption>
        ) : days.length === 0 ? (
          <Caption>
            Nothing archived yet. The nightly sweep archives each day past retention before it deletes it, and
            keeps everything when no archive is configured.
          </Caption>
        ) : (
          <ul className="os-logs-days" aria-label="Archived days">
            {days.map((entry) => (
              <li key={entry.day} className="os-logs-day">
                <span className="os-logs-day-name">{entry.day}</span>
                <span className="os-logs-day-note">
                  {entry.nodeTypes.length === 0 ? `${entry.objects} objects` : entry.nodeTypes.join(", ")}
                  {entry.bytes > 0 ? ` · ${formatBytes(entry.bytes)}` : ""}
                </span>
                {isOwner ? (
                  <Button
                    onClick={() => void bringBack(entry.day)}
                    busy={restoring === entry.day}
                    busyLabel="Bringing back"
                    disabled={restoring !== "" && restoring !== entry.day}
                    ariaLabel={`Bring back ${entry.day}`}
                  >
                    Bring back
                  </Button>
                ) : null}
                {restoreNote !== null && restoreNote.day === entry.day ? (
                  <Notice tone={restoreNote.tone} sentence={restoreNote.sentence} detail={restoreNote.detail} />
                ) : null}
              </li>
            ))}
          </ul>
        )}
        {connection !== null && !isOwner ? (
          <Caption>
            Bringing a day back is an owner's action: a restore writes into the store every admin reads, so it is
            offered to an owner and to nobody else.
          </Caption>
        ) : null}
      </Panel>

      <p className="os-caption">
        These are kept in this browser, separately from your desktop, so an app learning a preference can never
        cost you your desks. The defaults are {DEFAULT_LOGS_SETTINGS.defaultSection} at{" "}
        {DEFAULT_LOGS_SETTINGS.density} density, showing {levelFloorLabel(DEFAULT_LOGS_SETTINGS.levelFloor).toLowerCase()},
        over the {windowLabel(DEFAULT_LOGS_SETTINGS.streamWindow).toLowerCase()}.
      </p>
    </div>
  );
}
