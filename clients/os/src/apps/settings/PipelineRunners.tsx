import { useEffect, useState } from "react";

import { AutoRefresh, Caption, RecordListSkeleton, RecordList, RecordRow, Notice } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { readRunnerReport, runnerWords, type RunnerReport } from "./runnerReadiness";

/** Read-only observations, never a probe or an implicit build on page open. */
export function PipelineRunners() {
  const connection = useOsConnection();
  const [epoch, setEpoch] = useState(0);
  const [reading, setReading] = useState(true);
  const [error, setError] = useState("");
  const [savedReport, setReport] = useState<RunnerReport | null>(null);
  const [reportedBy, setReportedBy] = useState<typeof connection>(null);
  const [receivedAt, setReceivedAt] = useState(0);
  const [age, setAge] = useState(0);
  const report = reportedBy === connection ? savedReport : null;

  useEffect(() => {
    if (connection === null) {
      setReading(false);
      setError("Not connected to the cluster.");
      return;
    }
    const controller = new AbortController();
    setReading(true);
    setError("");
    void connection.query.pipelinesStatus({}, { signal: controller.signal }).then((result) => {
      if (controller.signal.aborted) return;
      setReport(readRunnerReport(result.rows()));
      setReportedBy(connection);
      setReceivedAt(performance.now());
      setAge(0);
    }).catch((err: unknown) => {
      if (!controller.signal.aborted) setError(err instanceof Error ? err.message : String(err));
    }).finally(() => {
      if (!controller.signal.aborted) setReading(false);
    });
    return () => controller.abort();
  }, [connection, epoch]);

  useEffect(() => {
    const timer = window.setInterval(() => setAge(performance.now() - receivedAt), 1_000);
    return () => window.clearInterval(timer);
  }, [receivedAt]);

  return <div className="os-pipelines-stop" aria-label="Runner readiness">
    {reading && report === null ? <RecordListSkeleton label="Reading runner readiness" rows={2} /> : null}
    {error !== "" ? <Notice tone="error" sentence="Runner readiness could not be read." detail={error} /> : null}
    {!reading && error === "" && report === null ? <Caption>Runner readiness has not been reported.</Caption> : null}
    {error === "" && report !== null && report.runners.length === 0 ? <Caption>No workbench runner has reported readiness.</Caption> : null}
    {report !== null && report.runners.length > 0 ? <RecordList as="ul" label="Workbench runners" density="compact">
      {report.runners.map((runner, index) => {
        const state = error !== "" ? "Readiness unknown" : runnerWords(runner, report, age);
        return <RecordRow key={`${runner.nodeId}:${index}`} name={runner.nodeId || "Unnamed workbench"}
          secondary={state} />;
      })}
    </RecordList> : null}
    {connection === null ? null : <div className="os-pipelines-act"><AutoRefresh onRefresh={() => setEpoch((value) => value + 1)} busy={reading} /></div>}
    <Caption>Reads the last isolation checks. A run checks again when needed.</Caption>
  </div>;
}
