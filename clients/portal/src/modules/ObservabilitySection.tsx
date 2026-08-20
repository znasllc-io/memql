import { useState, type ReactNode } from "react";
import { LINE_CHART_ELEMENT, TABLE_ELEMENT } from "@znasllc-io/memql-view-kit";
import type { Module } from "@znasllc-io/memql-sdk-core/client";

import { ErrorMessage } from "../components/StatusMessage";
import { Band, DataText, EmptyState, Skeleton } from "../ui";
import { useConcepts } from "../cluster/useConcepts";
import { ViewElement } from "../views/ViewElement";
import { summarize, useModuleObservability, type MetricWindow } from "./useModuleObservability";

// The observability drill-in on a module's detail (memql#4192): the three
// headline numbers, a call-volume trendline through view-kit (its
// CVD-validated palette untouched), and the most recent raw invocations.
//
// The section is honest about its own reach. The data comes from a capped,
// newest-first walk over the codeMetric aggregates filtered by the module's
// join keys, and the footer states how much history that covered. A module
// with no join keys (components, v1) gets the statement of that fact -- a
// chart with nothing behind it would be the fake this issue forbids.

const CODE_METRIC_CONCEPT = "v1:observability:codeMetric";
const INVOCATION_CONCEPT = "v1:observability:invocation";

export function ObservabilitySection({ module }: { module: Module }): ReactNode {
  const [bucket, setBucket] = useState<"1m" | "1h">("1h");
  const joinable = module.fqnPrefixes.length > 0 || module.codeReference !== "";
  const obs = useModuleObservability(
    module.fqnPrefixes,
    module.codeReference,
    bucket,
    joinable,
  );
  const { concepts } = useConcepts();
  const metricConcept = concepts.find((c) => c.id === CODE_METRIC_CONCEPT);
  const invocationConcept = concepts.find((c) => c.id === INVOCATION_CONCEPT);

  if (!joinable) {
    return (
      <Band title="Observability">
        <p className="text-sm text-muted">
          No invocation mapping exists for this module yet: components carry no codeReference
          prefix the metrics could be joined on (module-registry design, section 7). Nothing is
          charted because nothing can honestly be attributed.
        </p>
      </Band>
    );
  }

  const summary = summarize(obs.windows);

  return (
    <Band
      title="Observability"
      meta={
        <span className="flex items-center gap-2">
          <BucketSwitch bucket={bucket} onChange={setBucket} />
        </span>
      }
    >
      {obs.error !== "" ? (
        <ErrorMessage>Could not read invocation metrics: {obs.error}</ErrorMessage>
      ) : obs.loading && obs.windows.length === 0 ? (
        <Skeleton variant="stat" />
      ) : obs.windows.length === 0 ? (
        <EmptyState
          statement={
            <>
              No invocations recorded for this module in the {bucket} aggregates the walk
              covered. Recording follows the observe level — raise MEMQL_OBSERVE_LEVEL (or a
              per-FQN codeProfile) and traffic will land here.
            </>
          }
        />
      ) : (
        <div className="flex flex-col gap-4">
          <div className="flex flex-wrap gap-6">
            <Reading label="calls" value={String(summary.calls)} />
            <Reading
              label="error rate"
              value={`${(summary.errorRate * 100).toFixed(summary.errorRate > 0 && summary.errorRate < 0.001 ? 2 : 1)}%`}
            />
            <Reading label="worst window p95" value={`${summary.worstP95Ms.toFixed(1)} ms`} />
          </div>

          {metricConcept && obs.windows.length >= 2 ? (
            <ViewElement
              element={LINE_CHART_ELEMENT}
              rows={windowRows(obs.windows)}
              concept={metricConcept}
              options={{ bindings: { x: ["windowStart"], y: ["callCount"] } }}
            />
          ) : null}

          {invocationConcept && obs.invocations.length > 0 ? (
            <div>
              <h3 className="mb-2 text-xs font-semibold tracking-wide text-muted uppercase">
                Recent invocations
              </h3>
              <ViewElement
                element={TABLE_ELEMENT}
                rows={obs.invocations}
                concept={invocationConcept}
                options={{ sort: { field: "occurredAt", direction: "desc" } }}
              />
            </div>
          ) : null}
        </div>
      )}
      <p className="mt-3 text-xs text-subtle">
        {obs.truncated
          ? `Read the newest ${obs.scannedRows} aggregate rows cluster-wide and matched this module inside them — older history exists beyond the cap.`
          : `Read all ${obs.scannedRows} aggregate rows the cluster holds.`}{" "}
        Join keys:{" "}
        {[...module.fqnPrefixes, module.codeReference]
          .filter((k) => k !== "")
          .map((k, i) => (
            <span key={k}>
              {i > 0 ? ", " : ""}
              <DataText kind="id">{k}</DataText>
            </span>
          ))}
        .
      </p>
    </Band>
  );
}

function BucketSwitch({
  bucket,
  onChange,
}: {
  bucket: "1m" | "1h";
  onChange: (next: "1m" | "1h") => void;
}): ReactNode {
  return (
    <span role="group" aria-label="Aggregate window" className="flex overflow-hidden rounded border border-line">
      {(["1m", "1h"] as const).map((option) => (
        <button
          key={option}
          type="button"
          aria-pressed={bucket === option}
          onClick={() => onChange(option)}
          className={
            "px-2 py-0.5 text-xs " +
            (bucket === option ? "bg-accent-subtle text-fg" : "bg-surface text-muted hover:bg-raised")
          }
        >
          {option}
        </button>
      ))}
    </span>
  );
}

function Reading({ label, value }: { label: string; value: string }): ReactNode {
  return (
    <div className="min-w-28">
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-0.5 text-base font-semibold">
        <DataText kind="number">{value}</DataText>
      </div>
    </div>
  );
}

// Re-wrap the filtered windows in the wire row shape ViewElement's flatten
// expects, so the chart reads the same nested form real rows carry.
function windowRows(windows: readonly MetricWindow[]) {
  return windows
    .slice()
    .reverse()
    .map((w, index) => ({
      id: `${w.codeReference}:${w.windowStart}:${index}`,
      concept: CODE_METRIC_CONCEPT,
      createdAt: w.windowStart,
      payload: {
        codeReference: w.codeReference,
        windowStart: w.windowStart,
        bucket: w.bucket,
        callCount: w.callCount,
        errorCount: w.errorCount,
        p95DurationNs: w.p95DurationNs,
      },
    })) as unknown as Parameters<typeof ViewElement>[0]["rows"];
}
