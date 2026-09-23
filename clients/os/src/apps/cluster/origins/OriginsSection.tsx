import { useCallback, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Head, Notice, Panel, Subhead, RecordList, RecordRow } from "../../../kit";
import { Measure } from "../../../kit/MeasureView";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { DeadLetterBand } from "./DeadLetters";
import {
  connectorCoverage,
  coverageSentence,
  coverageTone,
  dataStateSentence,
  joinOrigins,
  originActs,
  type ConnectorCoverage,
  type OriginRow,
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

  const readInventory = useCallback(
    async (signal: AbortSignal): Promise<Row[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.dataOrigins({}, { signal });
      return result.rows();
    },
    [connection],
  );

  const readHealth = useCallback(
    async (signal: AbortSignal): Promise<Row[]> => {
      if (connection === null) throw new Error("not connected");
      const result = await connection.query.syncStatesAll({}, { signal });
      return result.rows();
    },
    [connection],
  );

  const inventory = useReading<Row[]>(
    "cluster:origins:inventory",
    connection === null ? null : readInventory,
  );
  const health = useReading<Row[]>(
    "cluster:origins:health",
    connection === null ? null : readHealth,
  );

  const join = useMemo(
    () => joinOrigins(inventory.value ?? [], health.value ?? []),
    [inventory.value, health.value],
  );

  const coverage = useMemo(() => connectorCoverage(join.rows), [join.rows]);

  const [openConnector, setOpenConnector] = useState("");
  const [busyKey, setBusyKey] = useState("");
  const [refusal, setRefusal] = useState("");

  const act = useCallback(
    async (row: OriginRow, which: "backfill" | "pause" | "resume") => {
      if (connection === null) return;
      setBusyKey(`${row.key}:${which}`);
      setRefusal("");
      try {
        if (which === "backfill") {
          await connection.query.datasyncStartBackfill({
            connector: row.connector,
            conceptId: row.conceptId,
          });
        } else {
          await connection.query.datasyncSetSyncPaused({
            connector: row.connector,
            conceptId: row.conceptId,
            paused: which === "pause",
          });
        }
        // Re-read the HEALTH half only. The declaration cannot have moved --
        // it comes from the concept registry, which no act on this page
        // touches -- and re-reading it would restart the whole table.
        health.reread();
      } catch (err: unknown) {
        setRefusal(err instanceof Error ? err.message : String(err));
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
      <Head title="Data origins" meta={inventory.state === "read" && !inventory.error ? join.rows.length : undefined}>
        <Button
          tone="quiet"
          busy={inventory.state === "reading" || health.state === "reading"}
          busyLabel="Reading"
          onClick={() => {
            inventory.reread();
            health.reread();
          }}
        >
          Read again
        </Button>
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
      {refusal === "" ? null : (
        <Notice tone="error" sentence="The cluster refused, and nothing changed." detail={refusal} />
      )}

      {inventory.state === "reading" && inventory.value === null ? (
        <Caption>Reading the declared inventory.</Caption>
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
            Reported means something has written a health row for that concept.
            {coverage.some((c) => coverageTone(c) === "silent")
              ? " Nothing here says a silent connector is broken, because this page cannot tell a connector that has not run from one that is running and reading nothing."
              : ""}
          </Caption>
        </Panel>
      )}

      {join.rows.length === 0 ? null : (
        <Panel label="Concepts">
          <Subhead meta={inventory.state === "read" && !inventory.error ? join.rows.length : undefined}>Concepts with a connector</Subhead>
          <RecordList as="ul" label="Data origins">
            {join.rows.map((row) => (
              <OriginLine
                key={row.key}
                row={row}
                busyKey={busyKey}
                onAct={(which) => void act(row, which)}
              />
            ))}
          </RecordList>
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
          {openConnector === "" ? null : <DeadLetterBand connector={openConnector} />}
        </Panel>
      ) : null}

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

function OriginLine({
  row,
  busyKey,
  onAct,
}: {
  row: OriginRow;
  busyKey: string;
  onAct: (which: "backfill" | "pause" | "resume") => void;
}) {
  const acts = originActs(row);
  return (
    <RecordRow name={row.conceptId} secondary={`${row.connector}${row.direction ? ` · ${row.direction}` : ""}`}
      state={row.paused ? "paused" : row.dataState || "undeclared"}
      stateTitle={dataStateSentence(row.dataState, row.origin, row.mirroredTo)}
      tone={row.paused || row.lastError ? "warn" : row.dataState === "mirror" ? "accent" : "muted"}
      actions={acts.map(which => <Button key={which} tone="quiet" busy={busyKey === `${row.key}:${which}`} busyLabel="Working"
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
