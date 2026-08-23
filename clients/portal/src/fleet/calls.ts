import { renderMemQLValue, type QueryClient, type Result } from "@znasllc-io/memql-sdk-core/client";

// The Fleet's named calls, composed here rather than through the generated
// typed surface.
//
// ===========================================================================
// WHY NOT THE GENERATED METHODS (memql#4232's normal path)
// ===========================================================================
// The Fleet DSL landed in the same epic as this page, and sdk/ts's
// generated_queries.ts / generated_mutations.ts are code-generated from
// `dsl/**/*.memql` by scripts/sdk-gen -- which has not been re-run for it. So
// `query.myWorkersWithStatus` does not exist as a method and would be a
// typecheck failure, not a runtime one.
//
// executeNamed is the seam underneath every generated builder (each one is
// `this.executeNamed(name, buildX(args), opts)`), so nothing is being
// smuggled past a boundary here -- the SAME call string reaches the same wire.
// The builders below are written to the generator's own shape: required
// arguments in declared order, optionals omitted when undefined, every value
// through renderMemQLValue and never hand-interpolated. When sdk-gen runs,
// swapping a call site onto `query.myWorkersWithStatus({})` is a rename and
// nothing else, because the composed string is byte-identical.
//
// TWO CALLS ARE NOT HERE, deliberately: revokeWorker and releaseWorkspace
// predate this epic, so they ARE generated and their call sites use the typed
// methods like every other write in the portal. Hand-building a call the SDK
// already exposes would be the actual violation.

// ---------------------------------------------------------------------------
// Object arguments
// ---------------------------------------------------------------------------

// renderLabelMap composes a MemQL object literal with QUOTED keys.
//
// renderMemQLValue renders an object's keys BARE (`{os: "darwin"}`), which the
// parser accepts only while every key happens to lex as one identifier. Label
// keys are operator-authored free text -- the workers runbook's own example
// carries `has-blender` -- and a hyphen makes the lexer split the key, so the
// bare form turns a legal label into a parse error naming a token the operator
// never wrote. component/language/parser's parseObject takes a TokenString key
// as its first branch, so the quoted form is the one that is correct for every
// key rather than for most of them.
//
// Values still go through renderMemQLValue: quoting is exactly the rule the
// whole call-building path in this portal is held to, and a label value is
// arbitrary text.
function renderLabelMap(labels: Record<string, string>): string {
  const keys = Object.keys(labels).sort();
  const parts = keys.map((key) => `${renderMemQLValue(key)}: ${renderMemQLValue(labels[key] ?? "")}`);
  return "{" + parts.join(", ") + "}";
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// The signed-in person's own machines. Declares no arguments.
export function myWorkersWithStatus(query: QueryClient): Promise<Result> {
  return query.executeNamed("myWorkersWithStatus", "query myWorkersWithStatus()");
}

// Every machine in the cluster. The engine refuses this to anyone who is not a
// cluster owner (the query's filter opens `actor.isClusterOwner==true`), so a
// non-owner reaching it gets an empty set rather than a leak -- the role check
// on the page decides what to OFFER, never what is allowed.
//
// ownerUserId is optional and narrows to one owner. Omitted when blank: the
// DSL's `when(args.ownerUserId)` guard drops an ABSENT argument and keeps an
// empty string, so sending "" would filter for rows owned by nobody.
export function allWorkersWithStatus(query: QueryClient, ownerUserId?: string): Promise<Result> {
  const parts: string[] = [];
  if (ownerUserId !== undefined) parts.push("ownerUserId: " + renderMemQLValue(ownerUserId));
  return query.executeNamed(
    "allWorkersWithStatus",
    "query allWorkersWithStatus(" + parts.join(", ") + ")",
  );
}

// The caller's routing policies, newest first. The active one is the editor's
// subject; superseded rows are kept because an invocation's routing.policyId
// names whichever row made the choice.
export function myRoutingPolicies(query: QueryClient): Promise<Result> {
  return query.executeNamed("myRoutingPolicies", "query myRoutingPolicies()");
}

// Recent calls dispatched to one machine, newest first.
//
// TWO QUERIES FOR ONE LIST, and the split is server-side. v1:worker:invocation
// declares no row tier, so the caller scope has to live in the FILTER -- and
// one filter cannot be both "mine" and "any, if you are a cluster owner".
// invocationsForWorker is `ownerUserId==actor.userId`; the operator variant is
// `actor.isClusterOwner==true`. Calling the self-scoped one from the
// all-machines view would render an empty list for every machine the operator
// does not personally own, which reads as "this machine is idle" rather than
// "you are not its owner".
export function invocationsForWorker(
  query: QueryClient,
  workerId: string,
  asOperator: boolean,
): Promise<Result> {
  const name = asOperator ? "invocationsForWorkerAsOperator" : "invocationsForWorker";
  return query.executeNamed(
    name,
    "query " + name + "(workerId: " + renderMemQLValue(workerId) + ")",
  );
}

// The caller's own workspaces, live and released, newest first.
export function myWorkspaces(query: QueryClient): Promise<Result> {
  return query.executeNamed("myWorkspaces", "query myWorkspaces()");
}

// Every workspace in the cluster, for a cluster owner. `status` narrows to one
// lifecycle value and is omitted when absent, for the same `when(...)` reason
// allWorkersWithStatus's ownerUserId is.
export function allWorkspaces(query: QueryClient, status?: string): Promise<Result> {
  const parts: string[] = [];
  if (status !== undefined) parts.push("status: " + renderMemQLValue(status));
  return query.executeNamed("allWorkspaces", "query allWorkspaces(" + parts.join(", ") + ")");
}

// ---------------------------------------------------------------------------
// Writes
// ---------------------------------------------------------------------------

// Rename one of the caller's machines. Writes `displayName`; `name` stays the
// cockpit's hostname, which the next reconnect re-stamps -- which is the whole
// reason a rename needs its own field rather than overwriting the reported one.
export function renameWorker(
  query: QueryClient,
  registrationId: string,
  displayName: string,
): Promise<Result> {
  return query.executeNamed(
    "renameWorker",
    "mutation renameWorker(registrationId: " +
      renderMemQLValue(registrationId) +
      ", displayName: " +
      renderMemQLValue(displayName) +
      ")",
  );
}

// Replace the operator-set labels on one machine. The whole map is REPLACED,
// not merged -- the page edits the set as a set, and a merge would make
// removing a label impossible through this surface.
export function setWorkerOperatorLabels(
  query: QueryClient,
  registrationId: string,
  operatorLabels: Record<string, string>,
): Promise<Result> {
  return query.executeNamed(
    "setWorkerOperatorLabels",
    "mutation setWorkerOperatorLabels(registrationId: " +
      renderMemQLValue(registrationId) +
      ", operatorLabels: " +
      renderLabelMap(operatorLabels) +
      ")",
  );
}

// The routing policy's two writes. `active: true` is written by both bodies,
// so the editor never has to say it.
export interface RoutingPolicyInput {
  policyId: string;
  strategy: string;
  requireLabels: Record<string, string>;
  preferLabels: Record<string, string>;
  fallback: string;
}

function routingPolicyArgs(input: RoutingPolicyInput): string {
  return (
    "policyId: " +
    renderMemQLValue(input.policyId) +
    ", strategy: " +
    renderMemQLValue(input.strategy) +
    ", requireLabels: " +
    renderLabelMap(input.requireLabels) +
    ", preferLabels: " +
    renderLabelMap(input.preferLabels) +
    ", fallback: " +
    renderMemQLValue(input.fallback)
  );
}

// Run once, for a caller who has no policy row at all.
export function createRoutingPolicy(
  query: QueryClient,
  input: RoutingPolicyInput,
): Promise<Result> {
  return query.executeNamed(
    "createRoutingPolicy",
    "mutation createRoutingPolicy(" + routingPolicyArgs(input) + ")",
  );
}

// Edit the caller's existing policy in place. ONE ACTIVE ROW PER USER is the
// model and the DSL cannot enforce it (no @unique check, no filter on a
// mutation), so the write side holds it by editing rather than inserting.
export function updateRoutingPolicy(
  query: QueryClient,
  input: RoutingPolicyInput,
): Promise<Result> {
  return query.executeNamed(
    "updateRoutingPolicy",
    "mutation updateRoutingPolicy(" + routingPolicyArgs(input) + ")",
  );
}
