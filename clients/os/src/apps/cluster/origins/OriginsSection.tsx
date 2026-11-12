import { RecordListSkeleton } from "../../../kit/RecordListSkeleton";
import { useCallback, useMemo, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { AutoRefresh, Button, Caption, Head, Notice, Panel, Subhead, RecordList, RecordRow } from "../../../kit";
import { Measure } from "../../../kit/MeasureView";
import { useSession } from "../../../chrome/access";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { DeadLetterBand } from "./DeadLetters";
import {
  connectorCoverage,
  coverageSentence,
  coverageTone,
  dataStateSentence,
  givenUpBySource,
  groupByPartition,
  joinOrigins,
  originActs,
  type ConnectorCoverage,
  type GivenUpSource,
  type OriginRow,
  type PartitionGroup,
} from "./rows";

// Data origins: what this cluster owns, what it mirrors, and how the
// connectors carrying either are doing.
//
// ===========================================================================
// THE DECLARATION AND THE HEALTH ARE TWO READS, AND THEY SETTLE SEPARATELY
// ===========================================================================
// `dataOrigins` is a virtual projection of the concept registry -- never
// persisted -- and `syncStatesAll` is a row read gated on
// `actor.isClusterOwner`. They fail for different reasons, and a single
// combined await would let the one that fails decide the state of the one
// that did not. So each has its own reading and its own sentence.
//
// ===========================================================================
// EVERY HEALTH NUMBER GOES THROUGH Figure
// ===========================================================================
// A connector that has never run has no lag, no drift, no outbox depth and no
// dead letters. Rendering those as `0` says "we looked, and the answer is
// none" -- which is exactly what a clean sweep also produces, and the two
// lead to opposite actions. The em dash is the difference, and it is the one
// thing about this table that must not regress.

export function OriginsSection() {
  const connection = useOsConnection();
  // EVERY ACT ON THIS PAGE IS A CLUSTER OWNER'S. Backfill now, Pause, Resume,
  // Try again and a dead letter's Retry and Discard are refused by the engine
  // for anybody else (requireOperator), while the page's part can be granted
  // to anybody. A person who is not one sees the state and no act: absent,
  // never drawn to be refused (DESIGN.md rule 12).
  const { access } = useSession();
  const operator = access?.role === "owner";

  const readInventory = useCallback(
    async (signal: AbortSignal): Promise<Row[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.dataOrigins({}, { signal });
      return result.rows();
    },
    [connection],
  );

  const readHealth = useCallback(
    async (signal: AbortSignal): Promise<HealthRead> => {
      if (connection === null) throw new Error("not connected");
      // To the last page, with the cursor each page hands back: a connector
      // that serves several stores writes a row per domain per store, and a
      // page that stopped at the first answer would draw the rest as never
      // reported.
      // Bounded all the same, and a read that stopped short -- at the bound,
      // or on a cursor handed back twice -- says so rather than passing for
      // the whole list.
      const read = await readPages((cursor) => connection.query.syncStatesAll({}, { signal, ...(cursor ? { cursor } : {}) }), HEALTH_PAGES);
      return { rows: read.rows, incomplete: read.more };
    },
    [connection],
  );

  const inventory = useReading<Row[]>(
    "cluster:origins:inventory",
    connection === null ? null : readInventory,
  );
  const health = useReading<HealthRead>(
    "cluster:origins:health",
    connection === null ? null : readHealth,
  );

  const join = useMemo(
    () => joinOrigins(inventory.value ?? [], health.value?.rows ?? []),
    [inventory.value, health.value],
  );

  const coverage = useMemo(() => connectorCoverage(join.rows), [join.rows]);
  const groups = useMemo(() => groupByPartition(join.rows), [join.rows]);
  // DISTINCT concepts: one concept on two stores is one concept, as the
  // caption's "N of M declared concepts" counts it.
  const conceptCount = useMemo(() => new Set(join.rows.map((row) => row.conceptId)).size, [join.rows]);
  const keepsStores = coverage.some((c) => c.stores > 0);
  // NO ACT WITHOUT A WHOLE HEALTH READ (ruling 45). Without it the page cannot
  // tell a store's row from a shared one, and a pause that names no store
  // reaches every store. Absent, not disabled.
  const mayAct = operator && health.value !== null && !health.value.incomplete;
  const [givenUpOpen, setGivenUpOpen] = useState(false);

  const [openConnector, setOpenConnector] = useState("");
  const [busyKey, setBusyKey] = useState("");
  const [refusal, setRefusal] = useState("");

  const act = useCallback(
    async (row: OriginRow, which: "backfill" | "pause" | "resume") => {
      if (connection === null) return;
      setBusyKey(`${row.key}:${which}`);
      setRefusal("");
      const store = row.partition === "" ? {} : { partitionKey: row.partition };
      try {
        if (which === "backfill") {
          // A failed backfill starts again from the start, because whatever
          // failed it is in the cursor it stopped at; any other continues
          // from where it stopped.
          await connection.query.datasyncStartBackfill({
            connector: row.connector,
            conceptId: row.conceptId,
            ...store,
            ...(row.backfillFailed ? { restart: true } : {}),
          });
        } else {
          // The row's own store: a pause with no partition is fanned out to
          // every store the connector serves.
          await connection.query.datasyncSetSyncPaused({
            connector: row.connector,
            conceptId: row.conceptId,
            ...store,
            paused: which === "pause",
          });
        }
        // Re-read the HEALTH half only. The declaration cannot have moved --
        // it comes from the concept registry, which no act on this page
        // touches -- and re-reading it would restart the whole table.
        health.reread();
      } catch (err: unknown) {
        setRefusal(err instanceof Error ? err.message : String(err));
        // A backfill can fail partway, with pages applied and progress
        // recorded: the rows are read again so they show it.
        health.reread();
      } finally {
        setBusyKey("");
      }
    },
    [connection, health],
  );


  return (
    <div className="os-cluster">
      {/* Quiet, not primary (DESIGN.md rule 1). Both readings are re-asked
          together: they describe one subject, and a page where half the
          numbers were refreshed and half were not is worse than one where
          none were. */}
      <Head title="Data origins" meta={inventory.state === "read" && !inventory.error ? conceptCount : undefined}>
        <AutoRefresh
          busy={inventory.state === "reading" || health.state === "reading"}

          onRefresh={() => {
            inventory.reread();
            health.reread();
          }}
         />
      </Head>

      {inventory.state === "read" ? <Caption>{join.withConnector} of {join.declared} declared concepts have a connector</Caption> : null}
      {inventory.state === "failed" ? (
        <Notice
          tone="error"
          sentence="The cluster did not answer the declared inventory."
          detail={inventory.error}
        />
      ) : null}
      {health.state === "failed" ? (
        <Notice
          tone="error"
          sentence="The cluster did not answer the connector health. The declarations below are still what every concept says about itself."
          detail={health.error}
        />
      ) : null}
      {health.value?.incomplete ? (
        <Caption>The connector health was not read to its end, so this list is incomplete.</Caption>
      ) : null}
      {/* NOT "NOTHING CHANGED": a backfill that fails partway keeps what it
          applied. True of a refusal and of a failure alike. */}
      {refusal === "" ? null : (
        <Notice tone="error" sentence="The cluster did not finish this." next="The list is read again, so each row shows where it stands now." detail={refusal} />
      )}

      {inventory.state === "reading" && inventory.value === null ? (
        <RecordListSkeleton label="Loading the declared inventory" />
      ) : null}

      {join.rows.length === 0 && inventory.state === "read" ? (
        <Caption>
          No concept in this cluster names a connector, so nothing here mirrors or syncs anything.
          Every concept is native: MemQL owns its data and nobody else holds a copy.
        </Caption>
      ) : null}

      {/* THE CONNECTOR, BEFORE THE PAIRINGS (issue memql#5574).
          The table below is honest per row and silent about the connector:
          65 concepts each correctly drawing an em dash say nothing about the
          one thing an operator came to ask. This band answers it first, and
          answers it as a COUNT -- what reported, out of what is declared --
          rather than as a verdict the page is in no position to give. */}
      {coverage.length === 0 ? null : (
        <Panel label="Connector coverage">
          <Subhead meta={inventory.state === "read" && !inventory.error ? coverage.length : undefined}>Connectors</Subhead>
          <RecordList as="ul" label="Connectors">
            {coverage.map((c) => (
              <CoverageLine key={c.connector} coverage={c} />
            ))}
          </RecordList>
          {/* TWO SENTENCES, AND THE SECOND ONLY WHEN IT IS ABOUT SOMETHING.
              The definition is always worth having. The refusal to give a
              verdict is about a silent connector, so on a cluster with none
              it explains a reading nobody is looking at -- and a caption that
              is true but irrelevant is the kind a reader learns to skip,
              taking the useful half with it. */}
          <Caption>
            Reported means something has written a health row for that concept{keepsStores ? ", on each store." : "."}
            {coverage.some((c) => coverageTone(c) === "silent")
              ? " Nothing here says a silent connector is broken, because this page cannot tell a connector that has not run from one that is running and reading nothing."
              : ""}
          </Caption>
        </Panel>
      )}

      {join.rows.length === 0 ? null : (
        <Panel label="Concepts">
          <Subhead meta={inventory.state === "read" && !inventory.error ? conceptCount : undefined}>Concepts with a connector</Subhead>
          {groups.map((group) => (
            <PartitionLines key={`${group.connector}|${group.partition}`} group={group} busyKey={busyKey} mayAct={mayAct} onAct={(row, which) => void act(row, which)} />
          ))}
          <Caption>
            An em dash is not a zero. It means nothing has reported that figure yet -- a connector
            that has never run has no lag and no drift, which is a different answer from a sweep
            that ran and found none.
          </Caption>
          {join.unmatchedHealth === 0 ? null : (
            <Caption>
              {join.unmatchedHealth} health {join.unmatchedHealth === 1 ? "row names" : "rows name"} a
              concept and connector this cluster no longer declares. That is a connector still
              running against a declaration that has moved.
            </Caption>
          )}
        </Panel>
      )}

      {/* THE BAND IS ABSENT UNTIL THE INVENTORY LANDS, not disabled
          (DESIGN.md rule 12). Its fan-out is over the connectors the
          inventory names, so before that read there is nothing it could ask
          about -- and a control that cannot act is one somebody has to read
          past to learn it is not for them yet. The reading state above says
          why the page is still filling in. */}
      {inventory.state === "read" && join.connectors.length > 0 ? (
        <Panel label="Dead letters">
          <Subhead>Dead letters</Subhead>
          <Caption>
            Read one connector at a time, on demand. These are never checked automatically: the
            question is one read per connector, and on a healthy cluster every answer is empty.
          </Caption>
          <div className="os-cluster-connectors">
            {join.connectors.map((connector) => (
              <Button
                key={connector}
                tone={openConnector === connector ? "primary" : "quiet"}
                onClick={() => setOpenConnector((held) => (held === connector ? "" : connector))}
              >
                {openConnector === connector ? `Close ${connector}` : `Check ${connector}`}
              </Button>
            ))}
          </div>
          {openConnector === "" ? null : <DeadLetterBand connector={openConnector} mayAct={operator} />}
        </Panel>
      ) : null}

      {/* A DELIVERY IS A STORE'S, NOT A DOMAIN'S, so it is not on a row above.
          Read on demand, as the dead letters are: on a healthy cluster the
          answer is empty. */}
      <Panel label="Changes that could not be recorded">
        <Subhead>Changes that could not be recorded</Subhead>
        <div className="os-cluster-connectors">
          <Button tone={givenUpOpen ? "primary" : "quiet"} ariaExpanded={givenUpOpen} onClick={() => setGivenUpOpen((held) => !held)}>
            {givenUpOpen ? "Close" : "Check"}
          </Button>
        </div>
        {givenUpOpen ? <GivenUpBand mayAct={operator} /> : null}
      </Panel>

      {inventory.at === null ? null : (
        <Caption>
          Read {inventory.at.toLocaleTimeString()}. Nothing here is live: neither the registry
          projection nor the connector health broadcasts a change.
        </Caption>
      )}
    </div>
  );
}

/**
 * One connector's line: its name, how much of what it carries has reported,
 * and the consequence when there is one.
 *
 * THE RATIO IS THE READING and it carries the eye, so it is the only thing
 * on the line besides the name. The sentence sits beneath in the quiet voice,
 * where a reader who wants it will find it and a reader scanning the ratios
 * is not made to read past it.
 *
 * `silent` takes warn INK ON WORDS and nothing else -- no chip, no card, no
 * border. `.os-cluster-row-attention`'s own comment already describes this
 * exact state ("configured to run and unable to"), which is why it is reused
 * rather than given a tone of its own.
 */
function CoverageLine({ coverage }: { coverage: ConnectorCoverage }) {
  const tone = coverageTone(coverage);
  const sentence = coverageSentence(coverage);
  return (
    <RecordRow name={coverage.connector} state={`${coverage.reported} of ${coverage.concepts} reported`} tone={tone === "silent" ? "warn" : "muted"}>
      {sentence === "" ? null : (
        <p className={tone === "silent" ? "os-cluster-row-attention" : "os-cluster-coverage-note"}>
          {sentence}
        </p>
      )}
      {coverage.withError === 0 ? null : (
        /* THE COUNT, IN THE QUIET VOICE, and the error itself stays in its own
           row. This was error ink and it was the wrong call: red outshouted
           the warn on a connector that had reported NOTHING, so the eye landed
           on the lesser reading first. The band's one loud thing is a silent
           connector; a per-concept failure is a detail the table already
           carries, in the place where the sentence that explains it lives. */
        <p className="os-cluster-coverage-note">
          {coverage.withError} of them {coverage.withError === 1 ? "carries" : "carry"} an error
          from its last attempt.
        </p>
      )}
    </RecordRow>
  );
}

/**
 * One store's domains, headed by the store in plain text -- or, for the
 * connectors that keep no stores apart, the list as it always was. What the
 * connection never asked for sits behind one closed line: it is not a fault,
 * so it is neither a row with an error nor counted as one.
 */
function PartitionLines({
  group,
  busyKey,
  mayAct,
  onAct,
}: {
  group: PartitionGroup;
  busyKey: string;
  mayAct: boolean;
  onAct: (row: OriginRow, which: "backfill" | "pause" | "resume") => void;
}) {
  const list = (
    <>
      <RecordList as="ul" label={group.partition === "" ? "Data origins" : group.partition}>
        {group.rows.map((row) => (
          <OriginLine key={row.key} row={row} busyKey={busyKey} mayAct={mayAct} onAct={(which) => onAct(row, which)} />
        ))}
      </RecordList>
      {group.outside.length === 0 ? null : (
        <details className="os-caption">
          <summary>Not part of this connection</summary>
          <ul>{group.outside.map((row) => <li key={row.key}>{row.conceptId}</li>)}</ul>
        </details>
      )}
    </>
  );
  if (group.partition === "") return <div className="os-cluster-partition">{list}</div>;
  return (
    <div className="os-cluster-partition" role="group" aria-label={group.partition}>
      <Subhead>{group.partition}</Subhead>
      {list}
    </div>
  );
}

/** At most this many pages of connector health are read. */
const HEALTH_PAGES = 200;

interface HealthRead {
  rows: Row[];
  /** The read stopped short: at HEALTH_PAGES, or on a cursor handed back twice. */
  incomplete: boolean;
}

/** At most this many pages of given-up deliveries are read (ruling 42). */
const GIVEN_UP_PAGES = 20;

/**
 * The deliveries that used their four tries, one line per source, LOADED ONLY
 * WHEN ASKED: the band mounts on demand and the read is its own mount, as the
 * dead-letter band's is. When pages remain after the last one read, each count
 * is a floor and says so.
 */
function GivenUpBand({ mayAct }: { mayAct: boolean }) {
  const connection = useOsConnection();
  const read = useCallback(
    async (signal: AbortSignal) => {
      if (connection === null) throw new Error("not connected");
      const pages = await readPages(
        (cursor) => connection.query.inboundRequestsGivenUp({}, { signal, ...(cursor ? { cursor } : {}) }),
        GIVEN_UP_PAGES,
      );
      return { sources: givenUpBySource(pages.rows), capped: pages.more };
    },
    [connection],
  );
  const reading = useReading<{ sources: GivenUpSource[]; capped: boolean }>(
    "cluster:origins:givenup",
    connection === null ? null : read,
  );
  const [busy, setBusy] = useState("");
  const [refusal, setRefusal] = useState("");
  // One requeue at a time: while one runs, a press on any source does
  // nothing. A ref, because two presses can land before the busy state renders.
  const running = useRef(false);

  const tryAgain = useCallback(
    async (source: string) => {
      if (connection === null || running.current) return;
      running.current = true;
      setBusy(source);
      setRefusal("");
      try {
        await connection.query.datasyncRequeueInbound({ source });
        reading.reread();
      } catch (err: unknown) {
        setRefusal(err instanceof Error ? err.message : String(err));
        // The requeue works in pages and can fail after some deliveries went
        // back in line: the list is read again so it shows them.
        reading.reread();
      } finally {
        running.current = false;
        setBusy("");
      }
    },
    [connection, reading],
  );

  const sources = reading.value?.sources ?? [];
  const capped = reading.value?.capped ?? false;
  return (
    <div className="os-cluster-band">
      {reading.state === "failed" ? (
        <Notice tone="error" sentence="The list could not be read." detail={reading.error} />
      ) : null}
      {/* NOT "NOTHING CHANGED": a requeue that fails part-way has put some
          deliveries back in line. */}
      {refusal === "" ? null : (
        <Notice tone="error" sentence="The cluster did not finish this." next="The list is read again, so each line shows where it stands now." detail={refusal} />
      )}
      {reading.state === "reading" && reading.value === null ? (
        <RecordListSkeleton label="Loading the changes that could not be recorded" />
      ) : null}
      {reading.state === "read" && sources.length === 0 ? (
        <Caption>No change has used all four tries.</Caption>
      ) : null}
      <RecordList as="ul" label="Changes that could not be recorded">
        {sources.map((s) => (
          <RecordRow key={s.source} name={s.source} tone="warn"
            actions={mayAct ? <Button tone="quiet" busy={busy === s.source} busyLabel="Working" onClick={() => void tryAgain(s.source)}>Try again</Button> : undefined}>
            <span>{capped ? "At least " : ""}{s.count} {s.count === 1 ? "change" : "changes"}</span>
            {s.lastError ? <span className="os-cluster-dl-error os-mono">{s.lastError}</span> : null}
          </RecordRow>
        ))}
      </RecordList>
    </div>
  );
}

/**
 * Read a paginated query page by page, with the cursor each page hands back,
 * up to `limit` pages. `more` is true when the walk stopped short of the end:
 * the last page read still handed a cursor back, or a cursor came back twice
 * (which stops the walk rather than looping on it).
 */
async function readPages(
  page: (cursor: string) => Promise<{ rows: () => Row[]; meta: () => { cursor?: string } | null | undefined }>,
  limit: number,
): Promise<{ rows: Row[]; more: boolean }> {
  const rows: Row[] = [];
  const seen = new Set<string>();
  let cursor = "";
  for (let read = 0; read < limit; read++) {
    const result = await page(cursor);
    rows.push(...result.rows());
    cursor = result.meta()?.cursor ?? "";
    if (cursor === "") return { rows, more: false };
    if (seen.has(cursor)) return { rows, more: true };
    seen.add(cursor);
  }
  return { rows, more: true };
}

function OriginLine({
  row,
  busyKey,
  mayAct,
  onAct,
}: {
  row: OriginRow;
  busyKey: string;
  mayAct: boolean;
  onAct: (which: "backfill" | "pause" | "resume") => void;
}) {
  const acts = mayAct ? originActs(row) : [];
  return (
    <RecordRow name={row.conceptId} secondary={`${row.connector}${row.direction ? ` · ${row.direction}` : ""}`}
      state={row.paused ? "paused" : row.dataState || "undeclared"}
      stateTitle={dataStateSentence(row.dataState, row.origin, row.mirroredTo)}
      tone={row.paused || row.lastError ? "warn" : row.dataState === "mirror" ? "accent" : "muted"}
      actions={acts.length === 0 ? undefined : acts.map(which => <Button key={which} tone="quiet" busy={busyKey === `${row.key}:${which}`} busyLabel="Working"
        ariaLabel={`${actLabel(which)} ${row.conceptId} on ${row.connector}`} onClick={() => onAct(which)}>{actLabel(which)}</Button>)}>
      <span className="os-record-facts">
      <span title="Seconds between the origin's write and MemQL applying it.">Lag <Measure figure={row.lagSeconds} suffix="s" /></span>
      <span title="Rows the last sweep found disagreeing with the origin.">Drift <Measure figure={row.driftCount} /></span>
      <span title="Pending and failed entries waiting to be drained.">Outbox <Measure figure={row.outboxDepth} /></span>
      <span title="Entries that exhausted their attempts and are waiting for a person.">Dead <Measure figure={row.deadLetterCount} /></span>
      </span>
      {row.hasHealth ? null : <span>nothing has reported on this pairing</span>}
      {row.dataState === "mirror" ? <span>Read-only here -- the engine refuses every write that does not come from {row.origin}.</span> : null}
      {row.lastError ? <span className="os-cluster-row-error os-mono">{row.lastError}</span> : null}
    </RecordRow>
  );
}

function actLabel(which: "backfill" | "pause" | "resume"): string {
  if (which === "backfill") return "Backfill now";
  if (which === "pause") return "Pause";
  return "Resume";
}
