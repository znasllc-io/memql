import { useMemo, useState, type ReactNode } from "react";

import { Chip, EmptyState, Measure, Notice, RecordList, RecordListSkeleton, RecordRow, RefreshButton, Refine, Select } from "../../../kit";
import { InfoDetail } from "../../../kit/InfoDetail";
import { formatDuration, formatMoment } from "../../../kit/format";
import {
  NO_FILTERS,
  billingShort,
  billingWords,
  consideredSentence,
  costFigure,
  costIsMoney,
  levelWords,
  useDecisions,
  type DecisionRow,
} from "../../settings/decisionFacts";
import { LEVELS } from "../../settings/routingFacts";
import { useRules, type RuleRow } from "../../settings/rulesFacts";
import { levelTitle, routeTitle, sourceLabel, whenWords } from "./vocabulary";

// Fleet > Routing > History: what actually served recent calls (was Settings
// > Decisions). It is how a rule is checked -- a rule set nobody can read the
// consequences of is a set of assertions.
//
// A ROW EXPANDS IN PLACE to the walk: which rule decided, which route it took,
// and every source the call passed over on the way, with why. Paging away to
// read three lines would lose the reader's place in the list they are
// scanning (DESIGN.md rule 11 is about a DETAIL PAGE; this is four lines).
//
// THE RULE IS NAMED BY WHAT IT MATCHES. A decision records the rule's engine
// name ("fastLane"), which is an id; the row says "Fast work", read off the
// rules this cluster actually has. A name that is no longer among them is said
// to be a removed rule rather than printed raw.
//
// FILTERED SERVER-SIDE: the query is keyset-paged at 200, so a facet filtered
// in the browser would narrow the page rather than the search.

const SOURCE_WORD: Record<string, string> = {
  local: "your machines",
  app: "a signed-in app",
  federation: "a vendor",
};

export function HistoryTab({ header }: { header: (actions: ReactNode, meta?: ReactNode) => ReactNode }) {
  const [level, setLevel] = useState("");
  const [source, setSource] = useState("");
  const [outcome, setOutcome] = useState("");
  const [rule, setRule] = useState("");
  const [open, setOpen] = useState("");
  const [search, setSearch] = useState("");
  const decisions = useDecisions(true, { ...NO_FILTERS, level, door: source, outcome, rule });
  const rules = useRules(true);
  const byName = useMemo(() => new Map(rules.rules.map((r) => [r.name, r] as const)), [rules.rules]);

  // THE SEARCH NARROWS WHAT WAS READ; the facets narrow what is read. A
  // prompt or model name is not an argument the query takes.
  const q = search.trim().toLowerCase();
  const rows = q === "" ? decisions.rows : decisions.rows.filter((r) => r.promptName.toLowerCase().includes(q) || r.model.toLowerCase().includes(q));

  const chips = [
    ...(level === "" ? [] : [{ id: "level", label: levelTitle(level), onRemove: () => setLevel("") }]),
    ...(source === "" ? [] : [{ id: "source", label: SOURCE_WORD[source] ?? source, onRemove: () => setSource("") }]),
    ...(outcome === "" ? [] : [{ id: "outcome", label: outcome === "ok" ? "Served" : outcome === "error" ? "Failed" : "Cancelled", onRemove: () => setOutcome("") }]),
    ...(rule === "" ? [] : [{ id: "rule", label: ruleWords(rule, byName), onRemove: () => setRule("") }]),
  ];

  return (
    <div className="os-deploy-scroll">
      {header(
        <>
          <InfoDetail title="History">
            <p>Every routed call: the rule that decided it, the route it took, what served it and what it cost. Open a call to see every source it passed over, and why.</p>
          </InfoDetail>
          {decisions.supported ? (
            <Refine label="Refine history" search={search} onSearch={setSearch} placeholder="Prompt or model" chips={chips}>
              <Select id="fleet-history-level" label="Level" value={level} onChange={setLevel}>
                <option value="">Any level</option>
                {LEVELS.map((one) => <option key={one} value={one}>{levelTitle(one)}</option>)}
              </Select>
              <Select id="fleet-history-source" label="Source" value={source} onChange={setSource}>
                <option value="">Any source</option>
                <option value="local">Your machines</option>
                <option value="app">Signed-in apps</option>
                <option value="federation">Vendors</option>
              </Select>
              <Select id="fleet-history-outcome" label="Outcome" value={outcome} onChange={setOutcome}>
                <option value="">Any outcome</option>
                <option value="ok">Served</option>
                <option value="error">Failed</option>
                <option value="cancelled">Cancelled</option>
              </Select>
              <Select id="fleet-history-rule" label="Rule" value={rule} onChange={setRule}>
                <option value="">Any rule</option>
                {rules.rules.map((r) => <option key={r.name} value={r.name}>{`${whenWords(r)} to ${routeTitle(r.policy)}`}</option>)}
              </Select>
            </Refine>
          ) : null}
          <RefreshButton label="Read the history again" busy={decisions.loading} onClick={decisions.reload} />
        </>,
        !decisions.loading && !decisions.error && decisions.supported && decisions.fetchedAt !== null ? rows.length : undefined,
      )}

      {decisions.error ? <Notice tone="warn" sentence="The history could not be read." detail={decisions.error} /> : null}

      {!decisions.supported ? (
        <EmptyState title="History unavailable">This cluster does not record routed calls yet.</EmptyState>
      ) : decisions.loading && decisions.rows.length === 0 ? (
        <RecordListSkeleton label="Loading the history" rows={4} />
      ) : rows.length === 0 && !decisions.error ? (
        <EmptyState title={chips.length > 0 || q !== "" ? "No matching calls" : "No calls yet"}>
          {chips.length > 0 || q !== "" ? "Remove a filter to see more." : "The first routed call appears here."}
        </EmptyState>
      ) : (
        <RecordList as="ul" label="Recent routed calls">
          {rows.map((row) => {
            const key = row.id || row.requestId;
            return <DecisionLine key={key} row={row} byName={byName} open={open === key} onOpen={() => setOpen(open === key ? "" : key)} />;
          })}
        </RecordList>
      )}
    </div>
  );
}

/** A rule named by what it matches; never its engine id. */
function ruleWords(name: string, byName: ReadonlyMap<string, RuleRow>): string {
  const rule = byName.get(name);
  return rule ? `${whenWords(rule)}` : "a removed rule";
}

function clockOf(value: string): string {
  if (value === "") return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return value;
  return at.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

/**
 * One call.
 *
 * THE COST NEVER PRINTS A ZERO FOR A FREE CALL: "$0.00" claims a measurement
 * came out zero, which would make a free call and a mis-priced metered one
 * identical. `costFigure` hands an absent figure carrying the reason.
 */
function DecisionLine({ row, byName, open, onOpen }: { row: DecisionRow; byName: ReadonlyMap<string, RuleRow>; open: boolean; onOpen: () => void }) {
  const level = levelWords(row);
  const where = SOURCE_WORD[row.door] ?? row.door;
  const spoken = [formatMoment(row.createdAt), row.promptName, level, where, row.model, row.outcome === "ok" ? "served" : row.outcome].filter((p) => p !== "").join(", ");
  return (
    <div className="os-decision-item" data-os-source={row.door} data-os-outcome={row.outcome}>
      <RecordRow
        name={row.promptName || "a call"}
        label={spoken}
        onOpen={onOpen}
        open={open}
        secondary={<>{clockOf(row.createdAt)} · {where} · <span className="os-mono">{row.model}</span></>}
        state={row.outcome === "ok" ? "served" : row.outcome}
        stateExtra={row.degraded ? <Chip tone="muted">stepped down</Chip> : undefined}
      >
        <span>{level}</span>
        <span data-cost>{costIsMoney(row) ? <Measure figure={costFigure(row)} format={(v) => `$${v.toFixed(4)}`} /> : <span title={billingWords(row)}>{billingShort(row)}</span>}</span>
        <span>{row.totalDurationMs.kind === "measured" ? formatDuration(row.totalDurationMs.value) : ""}</span>
      </RecordRow>
      {open ? (
        <div className="os-decision-detail">
          <p className="os-decision-detail-line">
            {row.rule === "" ? "No rule was recorded for this call." : `Decided by ${ruleWords(row.rule, byName)}, which takes ${row.policy ? routeTitle(row.policy) : "no route"}.`}
          </p>
          <p className="os-decision-detail-line">{consideredSentence(row)}</p>
          {row.considered.length === 0 ? null : (
            <ol className="os-decision-walk" aria-label="Sources the call passed through">
              {row.considered.map((c, i) => (
                <li key={`${c.entry}-${i}`} data-os-served={c.served || undefined}>
                  <span>{sourceLabel(c.entry)}</span>
                  <span className="os-decision-walk-door">{SOURCE_WORD[c.door] ?? c.door}</span>
                  <span className="os-decision-walk-why">{c.served ? "served this call" : c.why || "passed over"}</span>
                </li>
              ))}
            </ol>
          )}
          {row.touches.length === 0 ? null : <p className="os-decision-detail-line">Touched {row.touches.join(", ")}.</p>}
          {row.executionSurface === "" ? null : <p className="os-decision-detail-line os-mono">{row.executionSurface}</p>}
        </div>
      ) : null}
    </div>
  );
}
