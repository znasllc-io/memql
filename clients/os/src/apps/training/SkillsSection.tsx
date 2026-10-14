import { useCallback, useEffect, useRef, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Caption, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, Subhead } from "../../kit";
import { readHistory } from "./evaluations";
import { useOsConnection } from "../../live/connection";

export interface TrainingSkill {
  id: string; name: string; description: string; category: string;
  domains: string[]; tools: string[]; version: number | null;
  predefined: boolean; status: string; summary: string; body: string;
  originGoal: string; originRun: string; reinforced: number | null;
}

const text = (v: unknown) => typeof v === "string" ? v : "";
const strings = (v: unknown) => Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : [];
export function trainingSkill(raw: Row): TrainingSkill {
  const row = raw as Record<string, unknown>;
  const body = row.instructions && typeof row.instructions === "object" ? row.instructions as Record<string, unknown> : {};
  return {
    id: text(row.id), name: text(row.name) || text(row.slug), description: text(row.description), category: text(row.category),
    domains: strings(row.domainIds), tools: strings(row.toolSlugs),
    version: typeof row.version === "number" && row.version > 0 ? row.version : null,
    predefined: row.reviewedBaseline === true, status: row.active === false ? "Inactive" : text(row.status) || "Not recorded",
    summary: text(body.summary), body: text(body.body), originGoal: text(row.originatingGoalId), originRun: text(row.mintedByRunId),
    reinforced: typeof row.reinforceCount === "number" ? row.reinforceCount : null,
  };
}

type Selection = { run: string; step: string; at: string; selected: boolean; version: string; content: string; reason: string; terms: string[]; digest: string; skill: string; score: number | null; source: string; reviewState: string; receipt: string; contextReceipt: string; restored: boolean };
function selections(raw: Row[]): Selection[] {
  const out: Selection[] = [];
  for (const row of raw) {
    const data = row.data as Record<string, unknown> | undefined;
    const selection = data?.skillSelection as Record<string, unknown> | undefined;
    if (!selection || selection.schemaVersion !== 1) continue;
    for (const state of ["selected", "rejected"] as const) {
      const entries = selection[state];
      if (!Array.isArray(entries)) continue;
      for (const entry of entries) {
        if (!entry || typeof entry !== "object") continue;
        out.push({ run: text(row.runId), step: text(row.stepKey), at: text(row.createdAt), selected: state === "selected", version: String(entry.version ?? "Not recorded"), content: text(entry.text), reason: text(entry.reason), terms: strings(entry.terms), digest: text(entry.digest), skill: text(entry.id), score: typeof entry.score === "number" ? entry.score : null, source: text(entry.source), reviewState: text(entry.reviewState), receipt: text(row.id), contextReceipt: text(data?.contextReceiptId) || text(row.id), restored: data?.stage === "restored-for-context" });
      }
    }
  }
  return out;
}

export function SkillsSection() {
  const connection = useOsConnection();
  const [rows, setRows] = useState<TrainingSkill[]>([]);
  const [history, setHistory] = useState<Selection[]>([]);
  const [selected, select] = useState("");
  const [error, setError] = useState("");
  const [ready, setReady] = useState(false);
  const [busy, setBusy] = useState(false);
  const current = useRef<AbortController | null>(null);
  const reload = useCallback(async () => {
    if (!connection || current.current) return;
    const controller = new AbortController();
    current.current = controller;
    setBusy(true);
    try {
      const [result, receipts] = await Promise.all([
        connection.query.workSkillCatalog({}, { signal: controller.signal }),
        readHistory((cursor) => connection.query.workSkillSelectionsForOwner({}, { signal: controller.signal, cursor })),
      ]);
      if (controller.signal.aborted) return;
      const catalog = result.single()?.skills;
      if (!Array.isArray(catalog)) throw new Error("The skill catalog response is incomplete.");
      setRows(catalog.map((row: Row) => trainingSkill(row)).sort((a, b) => a.name.localeCompare(b.name)));
      setHistory(selections(receipts));
      setError(""); setReady(true);
    } catch (err) {
      if (!controller.signal.aborted) setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (current.current === controller) { current.current = null; setBusy(false); }
    }
  }, [connection]);
  useEffect(() => {
    setRows([]); setHistory([]); select(""); setError(""); setReady(false);
    void reload();
    return () => { current.current?.abort(); current.current = null; };
  }, [reload]);
  const selectedHistory = history.filter((h) => h.skill === selected);
  const skill = rows.find((r) => r.id === selected);
  return <div className="os-app-stack">
    <AutoRefresh onRefresh={reload} busy={busy} enabled={connection !== null} />
    <Head title={skill?.name ?? "Skills"} meta={!skill && ready && !error ? rows.length : undefined}
      back={skill ? { label: "Skills", onSelect: () => select("") } : undefined} />
    {error ? <Notice tone="error" sentence="Skills could not be read." next={ready ? "Showing the last available reading; retrying automatically." : "No skills have been read; retrying automatically."} detail={error} /> : null}
    {!ready && !error ? <Caption>{connection ? "Loading skills…" : "Waiting for the cluster…"}</Caption> : null}
    {ready && !error && rows.length === 0 ? <Caption>No skills are available in this cluster.</Caption> : null}
    {skill ? <>
      <Panel><Facts>
        <Fact label="Source" value={skill.predefined ? "MemQL baseline" : "Shared catalog"} />
        <Fact label="Version" value={skill.version ?? "Not recorded"} />
        <Fact label="State" value={skill.status} />
        <Fact label="Domains" value={skill.domains.join(", ")} />
        <Fact label="Tools" value={skill.tools.join(", ") || "None granted by this skill"} />
      </Facts></Panel>
      <Panel><Subhead>Content</Subhead>
        {skill.summary ? <p>{skill.summary}</p> : null}
        <p style={{ whiteSpace: "pre-wrap", maxWidth: "80ch" }}>{skill.body || skill.description || "No instructions are recorded."}</p>
      </Panel>
      <Panel><Subhead>History</Subhead><Facts>
        <Fact label="Originating goal" value={skill.originGoal || (skill.predefined ? "Shipped with MemQL" : "Not recorded")} />
        <Fact label="Originating run" value={skill.originRun} />
        <Fact label="Reinforcement records" value={skill.reinforced ?? "Not measured"} />
      </Facts></Panel>
      <Panel><Subhead>Selection history</Subhead>
        <Caption>These records preserve the content selected for a step. Selection alone does not prove that a model followed the guidance or that it improved the outcome.</Caption>
        {selectedHistory.length ? selectedHistory.map((h, n) => <details key={`${h.run}/${h.step}/${n}`}><summary>{h.selected ? (h.restored ? "Restored for work" : "Selected for work") : "Not selected"} · Version {h.version} · {h.at}</summary><Facts>
          <Fact label="Run" value={h.run} /><Fact label="Step" value={h.step} /><Fact label="Reason" value={h.reason.replaceAll("-", " ")} /><Fact label="Matched terms" value={h.terms.join(", ")} /><Fact label="Matched term count" value={h.score ?? "Not recorded"} /><Fact label="Source at selection" value={h.source} /><Fact label="Review state at selection" value={h.reviewState} /><Fact label="Receipt" value={h.receipt} /><Fact label="Content digest" value={h.digest} />
          {h.restored ? <Fact label="Original context receipt" value={h.contextReceipt} /> : null}
        </Facts>{h.content ? <p style={{ whiteSpace: "pre-wrap", maxWidth: "80ch" }}>{h.content}</p> : null}</details>) : <Caption>No selection records for your work.</Caption>}
      </Panel>
    </> : <RecordList as="ul" label="Skills">{rows.map((row) => <RecordRow key={row.id} name={row.name}
      secondary={row.description} state={row.predefined ? "MemQL baseline" : "Shared catalog"} onOpen={() => select(row.id)}>
      {row.version === null ? "Version not recorded" : `Version ${row.version}`}
    </RecordRow>)}</RecordList>}
  </div>;
}
