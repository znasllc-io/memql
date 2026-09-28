import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { GripVertical, Lock } from "lucide-react";

import { AddButton } from "../../../kit/AddButton";
import { InfoDetail } from "../../../kit/InfoDetail";
import { EmptyState, Notice, RecordList, RecordListSkeleton, RefreshButton, Refine, Select } from "../../../kit";
import { LEVELS } from "../../settings/routingFacts";
import {
  FLOOR_RULE_SENTENCE,
  LOCKED_RULE_SENTENCE,
  isFloorRule,
  ruleSentence,
  rulesInOrder,
  shadowedBy,
  useRuleActions,
  type RuleRow,
  type RulesState,
} from "../../settings/rulesFacts";
import type { TaskPolicy } from "../taskPolicies";
import { DescribeRulePage } from "./DescribeRulePage";
import { reorderPlan } from "./ruleOrder";
import { RuleFieldsPage } from "./RuleFieldsPage";
import { RuleSentence } from "./RuleSentence";
import { RuleWizard } from "./RuleWizard";
import type { RoutingFacts } from "./sources";
import { levelTitle, routeTitle, whenWords } from "./vocabulary";

// Fleet > Routing > Rules: "when work looks like X, take route Y", one per
// row, in the order they are tried (was Settings > Rules).
//
// ===========================================================================
// THE ORDER IS THE SPINE
// ===========================================================================
// First match wins, so the list IS the evaluation order: shipped rules with
// conditions first, then yours by precedence, then the shipped floor -- the
// conditionless rule that catches everything else and therefore runs LAST
// (memql#5127; `rulesInOrder` recognises it by shape, not by name).
//
// Shipped rules carry a lock and no acts: the engine refuses to change or
// remove them by name, so an act could only ever end in a refusal (rule 12).
// Yours can be opened, and REORDERED -- by dragging, or with Alt+Up / Alt+Down
// on the row -- which writes precedences through `reorderPlan`, so no
// intermediate state ever ties two rules.
//
// A RULE A SHIPPED ONE ALREADY DECIDES NEVER FIRES. Locked rules evaluate
// before yours regardless of precedence, and the engine saves a rule they
// shadow without a word. Such a row says so quietly ("a shipped rule decides
// first"), and the wizard and the rule's page point at the route to change
// instead (`shadowedBy`).
//
// THE RULES ARE READ BY THE SECTION, not here: routes and rules share one
// engine revision, so every write re-reads both (`onChanged`).

type Page = { kind: "list" } | { kind: "add" } | { kind: "describe" } | { kind: "fields"; seed: RuleRow | null };

export function RulesTab({
  header,
  rules,
  routes,
  facts,
  onChanged,
  onOpenRoute,
  request,
}: {
  header: (actions: ReactNode, meta?: ReactNode) => ReactNode;
  /** Read once by the section, shared with Routes and History. */
  rules: RulesState;
  routes: readonly TaskPolicy[];
  facts: RoutingFacts;
  /** Re-read routes AND rules after a write, landed or refused. */
  onChanged: () => void;
  /** Open a route's page in Routes. */
  onOpenRoute?: (route: string) => void;
  /** Another tab asking for the rules that take a route. */
  request?: { route: string; n: number } | null;
}) {
  const actions = useRuleActions(onChanged);
  const [page, setPage] = useState<Page>({ kind: "list" });
  const [routeFilter, setRouteFilter] = useState("");
  const toList = () => {
    actions.clear();
    setPage({ kind: "list" });
  };
  const ordered = useMemo(() => rulesInOrder(rules.rules), [rules.rules]);

  useEffect(() => {
    if (!request) return;
    actions.clear();
    setRouteFilter(request.route);
    setPage({ kind: "list" });
  }, [request?.n]);

  if (page.kind === "add") {
    return <RuleWizard actions={actions} rules={ordered} routes={routes} facts={facts} onCancel={toList} onDescribe={() => { actions.clear(); setPage({ kind: "describe" }); }} onAdded={toList} onOpenRoute={onOpenRoute} />;
  }
  if (page.kind === "describe") {
    return (
      <DescribeRulePage
        actions={actions}
        revision={ordered.find((r) => typeof r.revision === "number")?.revision}
        rules={ordered}
        onOpenRoute={onOpenRoute}
        onActivated={toList}
        onEditAsFields={(seed) => { actions.clear(); setPage({ kind: "fields", seed }); }}
        onBack={toList}
      />
    );
  }
  if (page.kind === "fields") {
    return <RuleFieldsPage actions={actions} routes={routes} seed={page.seed} existing={ordered} onDone={toList} onBack={toList} onOpenRoute={onOpenRoute} />;
  }
  return (
    <RuleList
      header={header}
      rules={rules}
      actions={actions}
      ordered={ordered}
      routes={routes}
      routeFilter={routeFilter}
      setRouteFilter={setRouteFilter}
      onReload={onChanged}
      onAdd={() => setPage({ kind: "add" })}
      onOpen={(seed) => setPage({ kind: "fields", seed })}
    />
  );
}

function RuleList({
  header,
  rules,
  actions,
  ordered,
  routes,
  routeFilter,
  setRouteFilter,
  onReload,
  onAdd,
  onOpen,
}: {
  header: (actions: ReactNode, meta?: ReactNode) => ReactNode;
  rules: RulesState;
  actions: ReturnType<typeof useRuleActions>;
  ordered: RuleRow[];
  routes: readonly TaskPolicy[];
  routeFilter: string;
  setRouteFilter: (route: string) => void;
  /** Read routes AND rules again: they share one engine revision. */
  onReload: () => void;
  onAdd: () => void;
  onOpen: (rule: RuleRow) => void;
}) {
  const hint = useId();
  const [search, setSearch] = useState("");
  const [level, setLevel] = useState("");
  const [origin, setOrigin] = useState("");
  // THE ORDER A MOVE ASKED FOR, until the re-read brings it back. The writes
  // take a round trip each; a list that snapped back while they ran would say
  // the move had not happened.
  const [pending, setPending] = useState<string[] | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const [focusName, setFocusName] = useState("");
  const rows = useRef(new Map<string, HTMLButtonElement | null>());
  const dragging = useRef<string | null>(null);

  useEffect(() => {
    setPending(null);
  }, [rules.rules]);

  useEffect(() => {
    if (focusName === "") return;
    rows.current.get(focusName)?.focus();
    setFocusName("");
  }, [focusName, pending, ordered]);

  const custom = ordered.filter((r) => !r.locked);
  const customOrder = pending ?? custom.map((r) => r.name);
  const shownOrder = [
    ...ordered.filter((r) => r.locked && !isFloorRule(r)),
    ...customOrder.map((name) => custom.find((r) => r.name === name)).filter((r): r is RuleRow => r !== undefined),
    ...ordered.filter((r) => isFloorRule(r)),
  ];

  const q = search.trim().toLowerCase();
  const filtering = q !== "" || level !== "" || origin !== "" || routeFilter !== "";
  const shown = shownOrder.filter((rule) => {
    if (origin === "shipped" && !rule.locked) return false;
    if (origin === "mine" && rule.locked) return false;
    if (routeFilter !== "" && rule.policy !== routeFilter) return false;
    if (level !== "" && rule.level !== level && rule.when["level"] !== level) return false;
    if (q === "") return true;
    return [whenWords(rule), routeTitle(rule.policy), ruleSentence(rule), rule.locked ? "" : rule.name].some((text) => text.toLowerCase().includes(q));
  });

  function move(name: string, by: number) {
    const current = customOrder.map((n) => custom.find((r) => r.name === n)!).filter(Boolean);
    const from = current.findIndex((r) => r.name === name);
    const to = from + by;
    if (from < 0 || to < 0 || to >= current.length) return;
    const plan = reorderPlan(current, from, to);
    if (plan.length === 0) return;
    const next = [...current.map((r) => r.name)];
    const [moved] = next.splice(from, 1);
    next.splice(to, 0, moved!);
    setPending(next);
    setFocusName(name);
    const rule = current[from]!;
    setAnnouncement(`${whenWords(rule)} to ${routeTitle(rule.policy)} is now ${to + 1} of ${current.length} of your rules.`);
    void actions.reorder(plan, rules.rules).then((ok) => {
      if (!ok) setPending(null);
    });
  }

  const canMove = actions.reorderable && !filtering && custom.length > 1 && !actions.state.busy && pending === null;
  const chips = [
    ...(level === "" ? [] : [{ id: "level", label: levelTitle(level), onRemove: () => setLevel("") }]),
    ...(origin === "" ? [] : [{ id: "origin", label: origin === "shipped" ? "Shipped" : "Yours", onRemove: () => setOrigin("") }]),
    ...(routeFilter === "" ? [] : [{ id: "route", label: `Takes ${routeTitle(routeFilter)}`, onRemove: () => setRouteFilter("") }]),
  ];

  return (
    <div className="os-deploy-scroll">
      {header(
        <>
          <InfoDetail title="How rules are tried">
            <p>Rules are tried top to bottom and the first that matches decides the route. {LOCKED_RULE_SENTENCE}</p>
            <p>Move one of yours with Alt and the arrow keys, or drag it.</p>
          </InfoDetail>
          {ordered.length > 0 ? (
            <Refine label="Refine rules" search={search} onSearch={setSearch} placeholder="Search" chips={chips}>
              <Select id="fleet-rules-level" label="Level" value={level} onChange={setLevel}>
                <option value="">Any level</option>
                {LEVELS.map((one) => <option key={one} value={one}>{levelTitle(one)}</option>)}
              </Select>
              <Select id="fleet-rules-origin" label="Origin" value={origin} onChange={setOrigin}>
                <option value="">Shipped and yours</option>
                <option value="shipped">Shipped only</option>
                <option value="mine">Yours only</option>
              </Select>
              <Select id="fleet-rules-route" label="Route" value={routeFilter} onChange={setRouteFilter}>
                <option value="">Any route</option>
                {routeFilter !== "" && !routes.some((r) => r.name === routeFilter) ? <option value={routeFilter}>{routeTitle(routeFilter)}</option> : null}
                {routes.map((r) => <option key={r.name} value={r.name}>{routeTitle(r.name)}</option>)}
              </Select>
            </Refine>
          ) : null}
          <RefreshButton label="Read the rules again" busy={rules.loading} onClick={onReload} />
          {actions.supported && rules.supported ? <AddButton label="Add a rule" onClick={onAdd} /> : null}
        </>,
        rules.read && !rules.loading && !rules.error && rules.supported ? shown.length : undefined,
      )}

      {rules.error ? <Notice tone="warn" sentence="The rules could not be read." detail={rules.error} /> : null}
      {actions.state.failed && actions.state.partial ? (
        <Notice tone="warn" sentence="The order changed part-way." next="The list shows what was saved. Move the rule again to finish, or back to undo." detail={actions.state.message} />
      ) : actions.state.failed && actions.state.message ? (
        <Notice tone="error" sentence="That did not go through." detail={actions.state.message} />
      ) : null}

      {!rules.supported ? (
        <EmptyState title="Rules unavailable">This cluster does not route by rules yet.</EmptyState>
      ) : rules.loading && ordered.length === 0 ? (
        <RecordListSkeleton label="Loading the rules" rows={4} />
      ) : ordered.length === 0 && !rules.error ? (
        <EmptyState title="No rules yet">Add one and every call that matches it takes the route you choose.</EmptyState>
      ) : shown.length === 0 && filtering ? (
        <EmptyState title="No matching rules">Try other words or remove a filter.</EmptyState>
      ) : (
        <>
          <p id={hint} className="os-sr-only">Alt and the up or down arrow moves this rule.</p>
          <RecordList as="ol" label="Rules, in the order they are tried" className="fleet-rules">
            {shown.map((rule) => (
              <li key={rule.name} data-os-locked={rule.locked || undefined} data-os-floor={isFloorRule(rule) || undefined}>
                {rule.locked ? (
                  <div className="fleet-rule-line" data-locked title={isFloorRule(rule) ? `${ruleSentence(rule)} ${FLOOR_RULE_SENTENCE}` : ruleSentence(rule)}>
                    <Lock size={14} aria-hidden className="fleet-rule-lock" />
                    <span className="os-sr-only">Shipped. </span>
                    <RuleSentence rule={rule} />
                  </div>
                ) : (
                  <button
                    ref={(el) => {
                      rows.current.set(rule.name, el);
                    }}
                    type="button"
                    className="fleet-rule-line"
                    draggable={canMove}
                    title={ruleSentence(rule)}
                    aria-describedby={canMove ? hint : undefined}
                    aria-keyshortcuts={canMove ? "Alt+ArrowUp Alt+ArrowDown" : undefined}
                    onClick={() => onOpen(rule)}
                    onKeyDown={(event) => {
                      if (!canMove || !event.altKey) return;
                      if (event.key === "ArrowUp") {
                        event.preventDefault();
                        move(rule.name, -1);
                      } else if (event.key === "ArrowDown") {
                        event.preventDefault();
                        move(rule.name, 1);
                      }
                    }}
                    onDragStart={(event) => {
                      dragging.current = rule.name;
                      event.dataTransfer?.setData("text/plain", rule.name);
                    }}
                    onDragEnd={() => {
                      dragging.current = null;
                    }}
                    onDragOver={(event) => {
                      if (canMove && dragging.current !== null) event.preventDefault();
                    }}
                    onDrop={(event) => {
                      event.preventDefault();
                      const from = dragging.current;
                      dragging.current = null;
                      if (!canMove || from === null || from === rule.name) return;
                      const at = customOrder.indexOf(rule.name);
                      const was = customOrder.indexOf(from);
                      if (at < 0 || was < 0) return;
                      move(from, at - was);
                    }}
                  >
                    {canMove ? <GripVertical size={14} aria-hidden className="fleet-rule-grip" /> : <span className="fleet-rule-grip" aria-hidden />}
                    <RuleSentence rule={rule} note={shadowedBy(rule.when, ordered) ? "a shipped rule decides first" : ""} />
                  </button>
                )}
              </li>
            ))}
          </RecordList>
        </>
      )}
      <p role="status" className="os-sr-only">{announcement}</p>
    </div>
  );
}
