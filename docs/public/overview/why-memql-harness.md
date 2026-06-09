---
title: Why memQL Is a Harness, Not a Library
audience: public
status: stable
area: overview
sinceVersion: 0.9.0
owner: znas
---

# Why memQL Is a Harness, Not a Library

Most "AI frameworks" hand you parts: a model client, a prompt
template, a chain, maybe a tool interface. You still have to build the
hard part yourself — the **harness**: the thing that runs an agent
turn after turn, remembers what happened, keeps a runaway loop from
burning your budget, routes work across a fleet, and survives a
restart. memQL **is** that harness, shipped as a runtime + memory
substrate rather than a box of parts.

This page is the proof, not the pitch. Every claim below points at the
code that backs it.

## What a harness actually has to do

An agent in a demo is a `while` loop around one model call. An agent
in production has to:

- run a **tool-calling loop** that terminates — and prove it
  terminated for the right reason;
- **remember** across turns, sessions, and restarts, and tell episodic
  noise from durable knowledge;
- **not bankrupt you** when a model gets stuck repeating itself;
- **route and coordinate** when the work outgrows one process;
- be **inspectable** after the fact — what ran, what it cost, why it
  decided that.

These are the things teams rebuild badly, over and over. memQL makes
them the substrate.

## The proof

### 1. The agent loop is a running service, not your `for` loop

The turn loop, the tool dispatch, and the reply contract are part of
the engine. An agent ends every turn through a single structured
envelope (`respondToUser` with `{response, citations[]}`), intercepted
by name in the streaming tool loop — see
`integrations/agent/streaming.go` and `integrations/agent/envelope.go`.
Tools a browser must execute are relayed across nodes through the graph
event bus (`integrations/cognition/client_tool_relay.go`), so "call a
client tool" works whether the agent and the browser are in the same
binary or on different machines. You don't write the loop; you declare
the tools and the reply shape.

### 2. A safety + cost spine that is on by default

This is where libraries leave you exposed and memQL does the unglamorous
work:

- **A process-wide LLM rate ceiling** at the provider chokepoint —
  `component/memql/si_guard.go` — so no code path, buggy or malicious,
  can stampede a provider.
- **Per-plan token budgets** enforced *before* each call
  (`component/planner/budget.go`, wired in
  `integrations/planner/agent_loop_budget.go`): a plan parks instead of
  making the next call when it would exceed a cumulative, persisted
  ceiling that survives retries.
- **Loop breakers**: repeat-failure and redelegation-refusal guards in
  the agent tool loop stop the classic "model apologizes and tries the
  same thing forever" failure.
- **An up-front estimate + approval gate** and **model tiering** (cheap
  by default, escalate only on an explicit stuck signal) from the
  goal-resolution work (epic memql#836).

A trivial request takes one cheap path; an expensive one is parked for
approval before it spends. That is policy the runtime enforces, not a
README suggestion.

### 3. Memory is the database, and it consolidates

memQL is built on an append-only, time-series **memory graph**
(PostgreSQL + TimescaleDB). Every node carries its own history; the
primary key is `(partition, id, createdAt)`. That means provenance and
replay are free — you can ask what was true at a point in time, not
just what is true now. Retrieval blends semantic similarity with
recency, and the harness consolidates episodic rows into durable
semantic knowledge rather than dumping everything into a vector store
and hoping. Memory is not a plugin you bolt on; it is the substrate the
agent runs on.

### 4. Behavior is declared, not glued

The MemQL DSL lets you declare a system's behavior as data: `concept`
(schema), `mutation`/`query` (writes/reads), `automation`
(event → side-effect), `logic` (procedures), `tool` (the SI-callable
surface), `prompt`, `provider`, `spec`, `shape`. An event triggers an
automation triggers a tool — without you wiring callbacks in Go and
redeploying. The same declarations drive validation, authorization
(per-row, classified and test-enforced in `dsl/conformance_test.go`),
and the generated reference. Capability grants (what an agent is even
allowed to do — `computer_use_*`, `workbench_use`) are declared and
expanded centrally in `component/memql/operator_caps.go`.

### 5. It is multi-node, authenticated, and observable out of the box

The same code compiles by build tag into a mesh of node types (bff,
voice, cognition, agent, planner, workbench, identity) that discover
each other and bridge events with dedup + TTL. There is a real
identity service (magic-link + JWT + JWKS), per-node verifiers, and
machine credentials for service-to-service calls. Every invocation can
be recorded to a hypertable for per-FQN latency/error metrics that feed
the Cockpit's topology view. You get the distributed, secured,
observable version for free — not as "an exercise for the reader."

## How developers use it

1. **Declare** your concepts, tools, and automations in `.memql` files.
2. **Drive** it from the **Cockpit** — the terminal-native IDE + ops
   console — to author, run, and watch agents and the cluster.
3. **Run** it as a single binary for local dev, or as the node mesh for
   scale; same DSL, same behavior, only config changes per environment.
4. **Extend** in Go only when you need to, via self-registering
   plug-ins with a narrow `PluginContext`.

## Why "it's real"

The strongest proof that this is a harness and not a slide deck:
Visionarys is building **CoPresent** — a multi-agent, voice + canvas
product — on memQL right now, on the path to release. The breakers, the
budgets, the memory consolidation, the cross-node tool relay described
above exist because a shipping product needs them. memQL is the
extracted, open-source harness underneath.

> Next: [memQL vs. other harnesses](vs-other-harnesses.md) — an honest
> comparison with the Go and Python field.
