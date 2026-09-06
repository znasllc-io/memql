import { useCallback, useMemo, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";

import { Button, Caption, Chip, Head, Notice, Panel, Subhead } from "../../../kit";
import { FigureValue } from "../../../cluster/FigureValue";
import { useOsConnection } from "../../../live/connection";
import { useReading } from "../../../cluster/reading";
import { DeadLetterBand } from "./DeadLetters";
import { dataStateSentence, joinOrigins, originActs, type OriginRow } from "./rows";

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

  const meta =
    inventory.value === null
      ? null
      : `${join.withConnector} of ${join.declared} declared concepts have a connector`;

  return (
    <div className="os-cluster">
      {/* Quiet, not primary (DESIGN.md rule 1). Both readings are re-asked
          together: they describe one subject, and a page where half the
          numbers were refreshed and half were not is worse than one where
          none were. */}
      <Head title="Data origins" meta={meta}>
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

      {join.rows.length === 0 ? null : (
        <Panel label="Connectors">
          <Subhead>Concepts with a connector</Subhead>
          <div className="os-cluster-table" role="table" aria-label="Data origins">
            <div className="os-cluster-tr os-cluster-th" role="row">
              <span role="columnheader">Concept</span>
              <span role="columnheader">Connector</span>
              <span role="columnheader" title="Seconds between the origin's write and MemQL applying it.">
                Lag
              </span>
              <span role="columnheader" title="Rows the last sweep found disagreeing with the origin.">
                Drift
              </span>
              <span role="columnheader" title="Pending and failed entries waiting to be drained.">
                Outbox
              </span>
              <span role="columnheader" title="Entries that exhausted their attempts and are waiting for a person.">
                Dead
              </span>
              <span role="columnheader">What you can do</span>
            </div>
            {join.rows.map((row) => (
              <OriginLine
                key={row.key}
                row={row}
                busyKey={busyKey}
                onAct={(which) => void act(row, which)}
              />
            ))}
          </div>
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
    <div className="os-cluster-tr" role="row" data-paused={row.paused || undefined}>
      <span role="cell" className="os-cluster-td-concept">
        <span className="os-mono">{row.conceptId}</span>
        <span className="os-cluster-state-marks">
          <Chip
            tone={row.dataState === "mirror" ? "accent" : "muted"}
            title={dataStateSentence(row.dataState, row.origin, row.mirroredTo)}
          >
            {row.dataState || "undeclared"}
          </Chip>
          {row.paused ? <Chip tone="accent">paused</Chip> : null}
        </span>
        {row.dataState === "mirror" ? (
          <span className="os-cluster-row-note">
            Read-only here -- the engine refuses every write that does not come from {row.origin}.
          </span>
        ) : null}
        {row.lastError === "" ? null : (
          <span className="os-cluster-row-error os-mono">{row.lastError}</span>
        )}
      </span>
      <span role="cell" className="os-cluster-td-connector">
        <span className="os-mono">{row.connector}</span>
        {row.direction === "" ? null : <span className="os-cluster-dir">{row.direction}</span>}
        {row.hasHealth ? null : (
          <span className="os-cluster-row-note">nothing has reported on this pairing</span>
        )}
      </span>
      <span role="cell">
        <FigureValue figure={row.lagSeconds} suffix="s" />
      </span>
      <span role="cell">
        <FigureValue figure={row.driftCount} />
      </span>
      <span role="cell">
        <FigureValue figure={row.outboxDepth} />
      </span>
      <span role="cell">
        <FigureValue figure={row.deadLetterCount} />
      </span>
      <span role="cell" className="os-cluster-td-acts">
        {acts.map((which) => (
          <Button
            key={which}
            tone="quiet"
            busy={busyKey === `${row.key}:${which}`}
            busyLabel="Working"
            ariaLabel={`${actLabel(which)} ${row.conceptId} on ${row.connector}`}
            onClick={() => onAct(which)}
          >
            {actLabel(which)}
          </Button>
        ))}
      </span>
    </div>
  );
}

function actLabel(which: "backfill" | "pause" | "resume"): string {
  if (which === "backfill") return "Backfill now";
  if (which === "pause") return "Pause";
  return "Resume";
}
