---
title: Engine, integration and workflow boundaries
audience: public
status: stable
area: build
sinceVersion: 0.25.0
owner: znas
---

# Engine, integration and workflow boundaries

MemQL workflows belong in `.memql`. Integrations supply bounded operations on
other systems; the engine interprets the language and enforces execution
invariants. A DSL action that merely calls an entire Go workflow does not make
that workflow declarative.

| Responsibility | Where it belongs | Examples |
| --- | --- | --- |
| Decide what happens, when and in what order | DSL automations, actions, queries, prompts and configuration | Select a release candidate; run checks; continue to notifications after failure; extract, summarize and index a document |
| Perform one external operation | Integration | Send a message; fetch an article; execute a constrained command; create a tag for an exact commit |
| Make execution correct and authorized | Engine or native capability runtime | Authorize each row; fence leases; preserve receipts; validate approval hashes; cancel work; enforce budgets |
| Compute or encode a result | Native library | Parse semantic versions; inspect an import graph; split text; render a binary document; escape an email payload |

The boundary follows meaning, not file size or the number of network requests.
An upload may need several protocol requests. A short function choosing a
release target is still workflow policy. Internal capabilities sometimes use
the integration registration API; that registration does not turn their locks,
transactions or execution journals into external integrations.

## A practical design test

Write the intended workflow in DSL before designing its native capabilities.
For each proposed capability, describe its input, one observable result, the
authority it requires, and what happens after an uncertain outcome. Then ask:

1. Could an operator change this decision without a new vendor protocol? Put
   that choice in DSL or declared configuration.
2. Would removing this check let a different workflow bypass authorization,
   approval, cost ceilings or isolation? Enforce it natively on every call.
3. Is this a reusable algorithm or codec rather than a workflow choice? Keep
   the implementation in a normal library.
4. Does a failure leave an external effect whose outcome is unknown? Keep an
   operation identity, receipt and reconciliation contract below the workflow.
   A retry statement alone cannot provide exactly-once external effects.

DSL may choose to retry a safe operation or continue after an optional failure.
Native code may retry a transport handshake or perform mechanical idempotency
checks needed to fulfill that operation. Neither may silently repeat an
unreconciled side effect. A native safety ceiling remains mandatory even when a
workflow chooses a smaller limit.

Use existing branches, collection expressions, loops, bounded parallelism,
child automations and actions. Add syntax only for a missing reusable semantic,
with parser, interpreter, diagnostics and conformance coverage. Do not add a
CI-specific language or a generic `executeGoWorkflow` escape hatch.

## Composing an already-authorized native operation

Some existing capabilities own an indivisible execution scope: a pipeline run,
a reviewed document revision or a release candidate. Their internal sequence
can still be authored as an installed `@template` automation.

The automation runtime supplies scoped execution through
`component/automations/workflowhost`. The native entry point first authenticates
and binds the candidate or selected rows. It supplies call-local callbacks for
only the operations that scope permits. The ordinary interpreter then executes
the template and synchronous children. Direct calls to those private callback
names fail closed; serialized arguments cannot create the scope.

Before any effect, the host checks the entire child call tree. Scoped recipes
cannot start an independent journal, acquire a new execution mode, detach child
work, or finish a parallel block before all branches join. Required arguments,
preconditions, cancellation, caller identity and the existing run association
are preserved. The ambient engine supplies expression context only, not a
second path to arbitrary engine operations. Generic scoped callbacks serialize
within one invocation; pipeline step execution has its own bounded parallel
host and receipt synchronization.

This host does not promise recovery for every recipe. Each native operation
retains its existing journal and reconciliation contract. Pipeline recovery
pins the installed workflow and child/action definitions, reconstructs control
flow from immutable inputs and receipts, and refuses changed definitions.
Unfinished compiled steps and required steps skipped as blocked cannot produce
a successful check result. Each action reference executes the exact definition
captured for that run, even if the global action registry later gains a version.

## Schedule placement

Installed bundles can declare node placement alongside a schedule:

```memql
@trigger(schedule="0 * * * * *", node="agent", lease="pipelines-poll")
automation pollPipelines {
  builtin pipelinesPoll()
}
```

The engine validates the node role and uses a shared PostgreSQL lease. Only the
selected node type's elected replica runs that schedule. Omit `lease` to use
`schedule:<automation-name>`. Lease scopes must be unique within the installed
bundle. Without `node`, the normal cron leader applies. Placement requires a
schedule, not an event or template. User-authored automations use owner-scoped
scheduling and cannot reserve an installation lease through this annotation.

Workflow cooldowns also need shared durable claims: an in-process map cannot
suppress duplicate work after a replica change. Claims with an explicit
lifetime remain until that lifetime expires, even if ordinary short-lived
execution claims are pruned sooner.

## Pipeline workflows

A pipeline manifest may name an installed template with `pipeline.workflow`.
Omitting it uses `runPipelineStages`. The manifest remains data describing
commands, images, selection and placement. The repository being tested cannot
supply executable workflow source through that field.

The template receives the compiled `stages` and may compose
`executePipelineStep`, `reportPipelineProgress`, `pipelineWorkflowFacts` and
`pipelineSkipStep`. The native host restricts every step key to its claimed run,
retains secret and worker isolation, and records actual effects. Calling the
same step twice still yields one receipt. The shipped recipe chooses stage
order, parallel execution, failure blocking and the notification exception.
Separate DSL templates own event-to-mode/version mapping and notification copy.

## Implementations in other languages

An in-process engine adapter is compiled Go. An external system or worker
capability may be implemented in any language supported by its execution
surface. Use the existing authenticated worker gRPC and named capability/script
contracts; see [Workers](../operate/workers-runbook.md). A Python process does
not need to become a Go plugin to perform a bounded operation.

That is not a universal remote-integration SDK. A protocol adapter still needs
a declared typed capability, authentication, deadline and cancellation,
structured results, secret handling, effect classification and an operation
identity where effects require reconciliation. Streaming protocols also need
flow control and reconnect semantics. Keep workflow policy in MemQL regardless
of the implementation language.

## Review and regression evidence

A boundary change must show a second workflow composing the same operations,
refusals before effects, and failure behavior with actual receipts. Test the
cross-node seam with a second host lacking the first host's local state. Keep
protocol, approval, row authorization and budget tests. Run strict DSL loading,
linting, language conformance and independent module builds as well as the
relevant integration and database tests.

Do not migrate an outage detector into the engine whose outage it must detect.
An independent observer should report facts; its instance configuration may
carry thresholds. Do not remove native ownership state machines simply because
they have branches. A capability may need multiple atomic writes to preserve
one invariant.

Related: [Component, integration and pack](../concepts/component-integration-pack.md),
[authoring rules](../language/authoring-rules.md),
[language reference](../language/memql.md), and
[pipeline operations](../operate/pipelines.md).
