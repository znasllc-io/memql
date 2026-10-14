import { useCallback, useEffect, useRef, useState } from "react";
import { AutoRefresh, Caption, Fact, Facts, Head, Notice, Panel, RecordList, RecordRow, Subhead } from "../../kit";
import { useOsConnection } from "../../live/connection";
import { catalog, evaluation, evaluationOutcome, fingerprint, measurement, readHistory, type Evaluation, type Scenario } from "./evaluations";
import matrix from "../../../../../test/proving/goals/matrix.json";
import "./inspection.css";

function Evidence({ items }: { items: string[] }) {
  return <ul>{items.map((ref, i) => <li key={`${ref}-${i}`}>{/^https:\/\//.test(ref) ? <a href={ref} target="_blank" rel="noreferrer">{ref}</a> : ref}</li>)}</ul>;
}
const scenarioById = new Map(catalog.scenarios.map((s) => [s.id, s]));
const setupKey = (r: Evaluation) => `${r.engineCommit}/${r.harnessCommit}/${r.setup.routingDigest}`;

export function EvaluationsSection() {
  const connection = useOsConnection();
  const [results, setResults] = useState<Evaluation[]>([]);
  const [selected, select] = useState<Scenario | null>(null);
  const [runId, selectRun] = useState("");
  const [fingerprints, setFingerprints] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [ready, setReady] = useState(false);
  const [busy, setBusy] = useState(false);
  const current = useRef<AbortController | null>(null);
  const reload = useCallback(async () => {
    if (!connection || current.current) return;
    const controller = new AbortController(); current.current = controller; setBusy(true);
    try {
      const rows = await readHistory((cursor) => connection.query.workGoalEvaluationsForOwner({}, { signal: controller.signal, cursor }));
      const hashes = Object.fromEntries(await Promise.all(catalog.scenarios.map(async (s) => [s.id, await fingerprint(s)])));
      if (controller.signal.aborted) return;
      const next: Evaluation[] = [];
      for (const row of rows) {
        const data = row.data as Record<string, unknown> | undefined;
        const value = data?.goalEvaluation;
        const parsed = await evaluation(value);
        if (!parsed) throw new Error("An evaluation record is incomplete or its evidence does not match its outcome.");
        next.push(parsed);
      }
      if (controller.signal.aborted) return;
      setFingerprints(hashes); setResults(next.sort((a,b) => Date.parse(b.finishedAt) - Date.parse(a.finishedAt))); setReady(true); setError("");
    } catch (e) { if (!controller.signal.aborted) setError(e instanceof Error ? e.message : String(e)); }
    finally { if (current.current === controller) { current.current = null; setBusy(false); } }
  }, [connection]);
  useEffect(() => {
    setResults([]); setReady(false); setError("");
    void reload();
    return () => { current.current?.abort(); current.current = null; };
  }, [reload]);
  const compatible = results.filter((r) => { const s = scenarioById.get(r.scenarioId); return s && r.scenarioVersion === s.version && r.fixtureVersion === s.fixture.version && r.scenarioFingerprint === fingerprints[s.id]; });
  const matching = (s: Scenario) => compatible.filter((r) => r.scenarioId === s.id);
  const history = selected ? matching(selected) : [];
  const result = history.find((r) => r.id === runId) ?? history[0];
  const groups = new Map<string, Evaluation[]>();
  for (const r of compatible) { const key = setupKey(r); const group = groups.get(key); if (group) group.push(r); else groups.set(key, [r]); }
  const setups = Array.from(groups.values(), (group) => group[0]!);
  const roles = setups.length ? Array.from(new Set(setups.flatMap((r) => r.setup.routes.map((route) => route.role)))) : ["reasoning"];
  return <div className="os-app-stack">
    <AutoRefresh onRefresh={reload} busy={busy} enabled={connection !== null} />
    <Head title={selected?.title ?? "Evaluations"} meta={selected ? `Version ${selected.version}` : `Catalog ${catalog.version}`}
      back={selected ? { label: "Evaluations", onSelect: () => select(null) } : undefined} />
    {error ? <Notice tone="error" sentence="Evaluation results could not be read." next={ready ? "Showing the last available reading; retrying automatically." : "No results have been read; retrying automatically."} detail={error} /> : null}
    {!ready && !error ? <Caption>{connection ? "Loading results…" : "Waiting for the cluster…"}</Caption> : null}
    {selected ? <>
      {history.length > 1 ? <label>Run <select aria-label="Evaluation run" value={result?.id} onChange={(e) => selectRun(e.target.value)}>{history.map((r) => <option key={r.id} value={r.id}>{r.setup.name} · {r.finishedAt}</option>)}</select></label> : null}
      <Panel><Subhead>Goal</Subhead><p style={{ maxWidth: "80ch" }}>{selected.goal}</p><Facts>
        <Fact label="Fixture" value={`${selected.fixture.id} ${selected.fixture.version}`} />
        <Fact label="Outcome" value={!ready ? "Not read" : evaluationOutcome(result)} />
        <Fact label="Routing setup" value={result?.setup?.name ?? "Not run"} />
        <Fact label="Engine" value={result?.engineCommit} /><Fact label="Harness" value={result?.harnessCommit} />
        <Fact label="Goal" value={result?.goalId} /><Fact label="Run" value={result?.runId} />
        <Fact label="Human interventions" value={result ? result.interventions?.length ?? "Not recorded" : "Not measured"} />
      </Facts></Panel>
      {result ? <>
        <Panel><Subhead>Configured routes</Subhead><RecordList as="ul" label="Configured routes">{result.setup?.routes?.map((route, i) =>
          <RecordRow key={i} name={route.role} secondary={`${route.level} / ${route.rule}`} state={route.doors.map((d) => `${d.source}: ${d.model}`).join(" → ")}>
            <details><summary>Model settings</summary>{route.doors.map((door, n) => <div key={n}><Caption>{n + 1}. {door.source}: {door.model}</Caption>{Object.keys(door.configuration).length ? <Facts>{Object.entries(door.configuration).map(([key, value]) => <Fact key={key} label={key} value={typeof value === "string" ? value : JSON.stringify(value)} />)}</Facts> : <Caption>No additional settings recorded.</Caption>}</div>)}</details>
          </RecordRow>
        )}</RecordList></Panel>
        <Panel><Subhead>Models actually used</Subhead><RecordList as="ul" label="Models actually used">{result.modelsUsed?.map((use, i) =>
          <RecordRow key={i} name={`${use.source}: ${use.model}`} secondary={`${use.role} / ${use.level}${use.fallback ? " / fallback" : ""}`} state={`${use.calls} calls`}>
            {measurement(use.inputTokens)} input; {measurement(use.outputTokens)} output
            <details><summary>Call evidence</summary><Evidence items={use.evidence} /><Facts>
              <Fact label="Input usage source" value={use.inputTokens.source} /><Fact label="Output usage source" value={use.outputTokens.source} />
              <Fact label="Cost (USD)" value={measurement(use.costUsd)} /><Fact label="Cost source" value={use.costUsd.source} />
            </Facts></details>
          </RecordRow>
        )}</RecordList></Panel>
      </> : null}
      <Panel><Subhead>Acceptance criteria</Subhead><RecordList as="ul" label="Acceptance criteria">{selected.criteria.map((c) => {
        const a = result?.assertions?.find((a) => a.criterionId === c.id);
        return <RecordRow key={c.id} name={c.claim} secondary={c.oracle} state={a?.verdict ?? "Unverified"}>{a?.evidence.length ? <Evidence items={a.evidence} /> : null}</RecordRow>;
      })}</RecordList></Panel>
      {result ? <Panel><Subhead>Measurements</Subhead><Facts>
        <Fact label="Input tokens" value={measurement(result.inputTokens)} />
        <Fact label="Output tokens" value={measurement(result.outputTokens)} />
        <Fact label="Cost (USD)" value={measurement(result.costUsd)} />
        <Fact label="Input usage source" value={result.inputTokens.source} /><Fact label="Output usage source" value={result.outputTokens.source} /><Fact label="Cost source" value={result.costUsd.source} />
        <Fact label="Started" value={result.startedAt} /><Fact label="Finished" value={result.finishedAt} />
        <Fact label="Elapsed" value={`${Math.round((Date.parse(result.finishedAt) - Date.parse(result.startedAt)) / 1000)} seconds`} />
      </Facts></Panel> : null}
      {result ? <>
        <Panel><Subhead>Skills and lessons supplied</Subhead>{result.bindings.length ? <RecordList as="ul" label="Skill and lesson versions">{result.bindings.map((b) => <RecordRow key={`${b.id}/${b.digest}`} name={b.id} secondary={`${b.source} · Version ${b.version}`} state={b.digest} />)}</RecordList> : <Caption>No binding receipts recorded.</Caption>}</Panel>
        {result.interventions.length ? <Panel><Subhead>Human interventions</Subhead><RecordList as="ul" label="Human interventions">{result.interventions.map((i, n) => <RecordRow key={n} name={i.description} secondary={`${i.kind} · ${i.at}`}><Evidence items={[i.evidence]} /></RecordRow>)}</RecordList></Panel> : null}
        <Panel><Subhead>Artifacts</Subhead><Evidence items={result.artifacts} /></Panel>
      </> : null}
      {selected.injections.length ? <Panel><Subhead>Controlled interruptions</Subhead><RecordList as="ul" label="Controlled interruptions">{selected.injections.map((i) => <RecordRow key={i.id} name={i.content} secondary={i.boundary} />)}</RecordList></Panel> : null}
      <Panel><Subhead>Constraints</Subhead><ul>{selected.constraints.map((c) => <li key={c}>{c}</li>)}</ul></Panel>
    </> : <>
      <Subhead>MemQL setups</Subhead>
      <table className="os-grid os-training-setups" aria-label="MemQL evaluation setups" tabIndex={0}>
        <thead><tr><th scope="col">Setup</th>{roles.map((role) => <th scope="col" key={role}>{role[0]?.toUpperCase()}{role.slice(1)}</th>)}{!setups.length ? <th scope="col">Other roles</th> : null}<th scope="col">Software development</th></tr></thead>
        <tbody>{setups.length ? setups.map((r) => {
          const runs = groups.get(setupKey(r))!;
          const describe = (role: string) => r.setup.routes.filter((route) => route.role === role).map((route) => route.doors.map((d) => `${d.source}:${d.model}`).join(" → ")).join("; ") || "Not configured";
          return <tr key={`${r.engineCommit}/${r.harnessCommit}/${r.setup.routingDigest}`}><th scope="row">MemQL · {r.setup.name}<Caption>Engine {r.engineCommit.slice(0, 12)} · Harness {r.harnessCommit.slice(0, 12)}</Caption></th>{roles.map((role) => <td key={role}>{describe(role)}</td>)}<td>{runs.filter((r) => r.outcome === "autonomous").length} autonomous; {runs.filter((r) => r.outcome === "assisted").length} assisted{["failed", "blocked", "cancelled"].map((outcome) => { const n = runs.filter((r) => r.outcome === outcome).length; return n ? `; ${n} ${outcome}` : ""; })}<Caption>{runs.length} {runs.length === 1 ? "run" : "runs"} · {new Set(runs.map((r) => r.scenarioId)).size}/{catalog.scenarios.length} scenarios</Caption></td></tr>;
        }) : <tr><th scope="row">MemQL · {matrix.setups[0]!.name}</th><td>{matrix.setups[0]!.reasoningModel}</td><td>Routing snapshot pending</td><td>{ready ? "Not run" : "Not read"}</td></tr>}</tbody>
      </table>
      <Caption>Each setup includes the harness and its routing configuration. Open a goal to inspect actual model calls and evidence. Monthly runs are off.</Caption>
      <Caption>The manual runner supports five local cases. Conflict, feedback, tool interruption, checkpoint and GitHub cases need their declared execution adapters before they can be measured.</Caption>
      <Subhead>Software development</Subhead>
      <RecordList as="ul" label="Goal scenarios">{catalog.scenarios.map((s) => <RecordRow key={s.id} name={s.title}
        secondary={matching(s)[0]?.setup?.name ?? `Scenario ${s.version}`} state={!ready ? "Not read" : evaluationOutcome(matching(s)[0])} onOpen={() => { select(s); selectRun(""); }} />)}</RecordList>
      <Subhead>More skill areas</Subhead>
      <RecordList as="ul" label="Planned skill areas">
        <RecordRow name="Document creation" state="Coming soon" />
        <RecordRow name="Research" state="Coming soon" />
      </RecordList>
    </>}
  </div>;
}
