import { useState } from "react";
import type { QueryClient, Row } from "@znasllc-io/memql-sdk-core/client";
import { Button, Caption, Notice, RecordList, RecordRow, Subhead } from "../../../kit";
import type { RecordTone } from "../../../kit/RecordRow";
import { formatMoment } from "../../../kit/format";
import { flatten } from "../../../kit/rows";
import { useReleaseRead } from "./useReleaseRead";

// The newest release train of each repository, and the one control a person needs
// when the nightly release is not enough: start one now (design record
// 2026-11-02-nightly-release-from-main-design.md, section 4). The engine refuses a
// start while one is building or running, so the button is only a courtesy there.

export type TrainQueries = Pick<QueryClient, "pipelineTrainsForOwner" | "pipelinesTrainExpress">;

interface TrainRow {
  id: string; repository: string; branch: string; state: string; version: string; heldReason: string; pullRequestUrl: string; departedAt: string;
}

const words: Record<string, { word: string; tone: RecordTone }> = {
  building: { word: "Building", tone: "accent" },
  running: { word: "Testing", tone: "accent" },
  landing: { word: "Landing", tone: "accent" },
  awaiting_approval: { word: "Waiting", tone: "warn" },
  held: { word: "Held", tone: "warn" },
  landed: { word: "Landed", tone: "muted" },
  cancelled: { word: "Cancelled", tone: "muted" },
};
const moving = new Set(["building", "running", "landing"]);

function text(row: Record<string, unknown>, name: string): string {
  const value = row[name];
  return typeof value === "string" ? value : "";
}

/** The newest row of each repository. The query answers newest first. */
export function newestTrains(rows: readonly Row[]): TrainRow[] {
  const seen = new Set<string>();
  const out: TrainRow[] = [];
  for (const raw of rows) {
    const row = flatten(raw) as Record<string, unknown>;
    const repository = text(row, "repository");
    if (!repository || seen.has(repository)) continue;
    seen.add(repository);
    out.push({ id: text(row, "id"), repository, branch: text(row, "branch"), state: text(row, "state"), version: text(row, "version"), heldReason: text(row, "heldReason"), pullRequestUrl: text(row, "pullRequestUrl"), departedAt: text(row, "departedAt") });
  }
  return out;
}

export function ReleaseTrainPanel({ query, connected, visible = true }: { query: TrainQueries | null; connected: boolean; visible?: boolean }) {
  const available = connected && query !== null;
  const trains = useReleaseRead("trains", available ? async signal => newestTrains((await query.pipelineTrainsForOwner({}, { signal })).rows()) : null, visible, query);
  const [starting, setStarting] = useState("");
  const [refusal, setRefusal] = useState("");
  async function start(repository: string) {
    if (!query || starting) return;
    setStarting(repository);
    setRefusal("");
    try {
      await query.pipelinesTrainExpress({ repository, pullRequests: [] });
      trains.retry();
    } catch (error) {
      setRefusal(error instanceof Error ? error.message : String(error));
    } finally {
      setStarting("");
    }
  }
  if (!trains.value?.length) return null;
  return <>
    <Subhead>Latest release</Subhead>
    {refusal ? <Notice tone="error" sentence={refusal} /> : null}
    <RecordList as="ul" label="Latest release of each repository">
      {trains.value.map(train => {
        const state = words[train.state] ?? { word: train.state, tone: "muted" as const };
        const name = train.pullRequestUrl ? <a href={train.pullRequestUrl} target="_blank" rel="noreferrer">{train.repository}</a> : train.repository;
        const secondary = [train.version, train.branch, train.departedAt ? formatMoment(train.departedAt) : ""].filter(Boolean).join(" · ");
        return <li key={train.id}>
          <RecordRow name={name} secondary={secondary} state={state.word} tone={state.tone} />
          {train.heldReason && train.state === "held" ? <Caption>{train.heldReason}</Caption> : null}
          {moving.has(train.state) ? null : <div className="os-panel-actions">
            <Button disabled={starting !== ""} onClick={() => void start(train.repository)}>
              {train.state === "held" || train.state === "awaiting_approval" ? "Replace with a new release" : "Start release now"}
            </Button>
          </div>}
        </li>;
      })}
    </RecordList>
  </>;
}
