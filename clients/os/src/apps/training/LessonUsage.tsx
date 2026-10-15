import { RecordListSkeleton } from "../../kit/RecordListSkeleton";
import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import {
  Caption,
  Notice,
  Panel,
  RecordList,
  RecordRow,
  Subhead,
} from "../../kit";
import { useOsConnection } from "../../live/connection";
import { readHistory } from "./evaluations";
type Call = {
  id: string;
  phase: string;
  provider: string;
  model: string;
  servedModel?: string;
  error?: string;
  contextReferences?: string[];
};
export function lessonUsage(rows: Row[], id: string) {
  const calls: Call[] = rows.flatMap((row) => {
    const d = row.data as { execution?: { call?: Call } } | undefined;
    return d?.execution?.call ? [d.execution.call] : [];
  });
  return rows.flatMap((row) => {
    const d = row.data as
      | {
          trainingSelection?: {
            selected?: { lessonId?: string; lessonVersion?: number }[];
          };
        }
      | undefined;
    const selected = d?.trainingSelection?.selected?.find(
      (p) => p.lessonId === id,
    );
    if (!selected) return [];
    const ref = String(row.id);
    const attempts = calls.filter((c) => c.contextReferences?.includes(ref));
    return [
      {
        id: ref,
        run: String(row.runId),
        version: selected.lessonVersion,
        attempts,
      },
    ];
  });
}
export function LessonUsage({ lessonId }: { lessonId: string }) {
  const connection = useOsConnection();
  const [rows, setRows] = useState<Row[] | null>(null),
    [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    setRows(null);
    setError("");
    if (connection)
      void readHistory((cursor) =>
        connection.query.workTrainingUsageForOwner({}, { cursor }),
      ).then(
        (r) => {
          if (active) setRows(r);
        },
        (e) => {
          if (active) setError(String(e));
        },
      );
    return () => {
      active = false;
    };
  }, [connection, lessonId]);
  const usage = rows ? lessonUsage(rows, lessonId) : [];
  return (
    <Panel>
      <Subhead>Use in work</Subhead>
      {error ? (
        <Notice
          tone="error"
          sentence="Usage evidence could not be read."
          detail={error}
        />
      ) : rows === null ? (
        <RecordListSkeleton label="Reading usage evidence" />
      ) : usage.length ? (
        <RecordList as="ul" label="Lesson usage">
          {usage.map((u) => (
            <RecordRow
              key={u.id}
              name={`Selected for work · Version ${u.version}`}
              secondary={u.run}
              state={
                u.attempts.some((c) => c.phase === "completed" && !c.error)
                  ? "Included in model call"
                  : u.attempts.length
                    ? "Delivery attempted"
                    : "Delivery not recorded"
              }
            >
              {u.attempts.map((c) => (
                <p key={c.id}>
                  {c.servedModel || c.model || c.provider} · {c.phase}
                </p>
              ))}
            </RecordRow>
          ))}
        </RecordList>
      ) : (
        <Caption>No selection recorded.</Caption>
      )}
      <Caption>
        Delivery does not establish that the advice was followed or improved the
        result.
      </Caption>
    </Panel>
  );
}
