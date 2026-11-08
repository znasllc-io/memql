import { useEffect, useState } from "react";
import type { Row } from "@znasllc-io/memql-sdk-core/client";
import { AutoRefresh, Caption, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, Refine, Subhead } from "../../kit";
import { LoadMore } from "../../kit/LoadMore";
import { useLiveCollection, usePagedCollection } from "../../live/useLiveCollection";
import { TEXT_DEBOUNCE_MS, useDebouncedValue } from "../../logs/useDebouncedValue";
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
  const [selected, select] = useState("");
  const [search, setSearch] = useState("");
  const query = useDebouncedValue(search.trim(), TEXT_DEBOUNCE_MS);
  const catalog = usePagedCollection<Row>(`training:skills:${query}`, conn => ({
    concept: "v1:skills:skill",
    actions: [],
    seed: async (cursor, signal) => {
      const result = await conn.query.workSkillCatalogPage({ search: query, cursor }, { signal });
      const page = result.single();
      if (!Array.isArray(page?.skills)) throw new Error("The skill catalog response is incomplete.");
      return { rows: page.skills as Row[], nextCursor: text(page.nextCursor) };
    },
  }));
  const detail = useLiveCollection<Row>(selected ? `training:skill:${selected}` : null, conn => ({
    concept: "v1:skills:skill",
    actions: [], paged: false,
    seed: async (_cursor, signal) => {
      const result = await conn.query.workSkillCatalogEntry({ skillId: selected }, { signal });
      const row = result.single()?.skill;
      return { rows: row && typeof row === "object" ? [row as Row] : [], nextCursor: "" };
    },
  }));
  const receipts = usePagedCollection<Row>(selected ? `training:skill-history:${selected}` : null, conn => ({
    concept: "v1:work:observation", actions: [],
    seed: async (cursor, signal) => {
      const result = await conn.query.workSkillSelectionsForSkill({ skillId: selected }, { signal, cursor });
      return { rows: result.rows(), nextCursor: result.meta()?.cursor ?? "" };
    },
  }));
  useEffect(() => { select(""); }, [connection]);
  const rows = catalog.snapshot.rows.map(trainingSkill);
  const skill = detail.snapshot.rows[0] ? trainingSkill(detail.snapshot.rows[0]) : undefined;
  const selectedSummary = rows.find(row => row.id === selected);
  const selectedHistory = selections(receipts.snapshot.rows).filter(receipt => receipt.skill === selected);
  const ready = catalog.snapshot.state !== "seeding" && catalog.snapshot.state !== "disconnected";
  const error = catalog.snapshot.error;
  const reload = () => { catalog.reseed(); if (selected) { detail.reseed(); receipts.reseed(); } };
  const busy = catalog.snapshot.state === "seeding";
  return <div className="os-app-stack">
    <AutoRefresh resume={false} onRefresh={reload} busy={busy} enabled={connection !== null} />
    <Head navigationKey={`skill:${selected}`} title={skill?.name ?? selectedSummary?.name ?? "Skills"} meta={!skill && ready && !error ? rows.length : undefined}
      back={selected ? { label: "Skills", onSelect: () => select("") } : undefined}>
      {!selected ? <Refine label="Search skills" search={search} onSearch={setSearch} placeholder="Search skills" /> : null}
    </Head>
    {error ? <Notice tone="error" sentence="Skills could not be read." next={ready ? "Showing the last available reading; retrying automatically." : "No skills have been read; retrying automatically."} detail={error} /> : null}
    {!ready && !error ? <Caption>{connection ? "Loading skills…" : "Waiting for the cluster…"}</Caption> : null}
    {ready && !error && rows.length === 0 ? <Caption>{query ? "No skills match this search." : "No skills are available in this cluster."}</Caption> : null}
    {selected && detail.snapshot.error ? <Notice tone="error" sentence="This skill could not be read." detail={detail.snapshot.error} /> : null}
    {selected && detail.snapshot.state === "seeding" ? <Caption>Loading skill details…</Caption> : null}
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
        </Facts>{h.content ? <p style={{ whiteSpace: "pre-wrap", maxWidth: "80ch" }}>{h.content}</p> : null}</details>) : <Caption>{receipts.snapshot.state === "seeding" ? "Loading selection history…" : "No selection records for your work."}</Caption>}
        {receipts.snapshot.error ? <Notice tone="error" sentence="Selection history could not be read." detail={receipts.snapshot.error} /> : null}
        <LoadMore feed={receipts} />
      </Panel>
    </> : !selected ? <><RecordList as="ul" label="Skills">{rows.map((row) => <RecordRow key={row.id} name={row.name}
      secondary={row.description} state={row.predefined ? "MemQL baseline" : "Shared catalog"} onOpen={() => select(row.id)}>
      {row.version === null ? "Version not recorded" : `Version ${row.version}`}
    </RecordRow>)}</RecordList><LoadMore feed={catalog} /></> : detail.snapshot.state !== "seeding" && !detail.snapshot.error ? <Caption>This skill is no longer available.</Caption> : null}
  </div>;
}
