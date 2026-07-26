---
title: Forge — Company Operating System
audience: public
status: stable
area: operate
sinceVersion: 0.9.75
owner: znas
---

# Forge — Company Operating System

Forge is a project-agnostic bundle, exposed over MCP, that a team uses through
Claude to run development and people-ops. Three layers compose the company
operating system:

- **MemQL** — the brain and memory: the time-series graph that stores every
  request, event, and audit trail durably and queryably.
- **Claude** — the interactive frontend: team members talk to Claude directly,
  and Claude calls the forge MCP tools on their behalf.
- **Forge** — the workshop: the DSL bundle (`dsl/forge/`) that defines the
  concepts, approval pipeline, role model, MCP tool surface, and automations.

Forge sits on top of the apps it develops (memql, product frontends, client builds). It
is not part of the core engine; it is a plugged-in namespace that can serve
multiple projects from one tenant.

---

## Concepts

Three concept types form the forge data model. All are partition-scoped (no
`@scope`), meaning rows are team-shared within the tenant — a validator or
approver can read a submitter's request. Read gating to developers and owners
is enforced at the query layer via the forge role specs; it is not per-row
ownership.

### project (`v1:forge:project`)

A unit of work context forge operates against. One project row per app or
codebase (e.g. `memql`, `exampleapp-acme`). Requests reference a `projectId`,
so the same forge bundle serves multiple apps without baking any one in.

Key fields: `slug` (stable short id), `name`, `targetApp`, `repo`,
`description`, `status` (`active` | `archived`), `createdByUserId`.

### request (`v1:forge:request`)

The spine. A typed unit of work submitted by a team member, flowing through
the role-gated approval pipeline. Today the types are development-oriented
(`bug`, `feature`, `chore`, `question`); HR types (`timeOff`, `checkIn`) are
modelled on the same spine for a future slice.

Key fields: `requestType` (named `requestType`, not `type`, because `type` is
a reserved row intrinsic in MemQL), `title`, `body`, `submitterUserId`,
`submitterRole`, `priority`, `status`, `targetEnv`, `validatedByUserId`,
`approvedByUserId`, `resolution`.

The `submitterUserId` and `submitterRole` fields are stamped server-side from
the authenticated actor. A tool call cannot submit as someone else.

### requestEvent (`v1:forge:requestEvent`)

An append-only row per transition or note: a routing decision, validation,
approval, rejection, comment, or mentoring touch. Every state transition
produces one event. This is the time-series audit trail — the "who did what
when" that makes the graph the memory of the company.

Key fields: `requestId`, `kind` (submitted / routed / validated /
changes_requested / approved / rejected / commented / mentored), `actorUserId`,
`actorRole`, `fromStatus`, `toStatus`, `note`.

---

## Request pipeline

A request moves through the following state machine after submission. The
initial transition fires automatically from the `routeRequest` automation.

```
                   [submit]
                       |
                       v
                   submitted
                       |
           routeRequest automation (by submitterRole)
           /               |               \
          /                |                \
  [owner]             [admin/writer]        [reader]
     |                     |                   |
     v                     v                   v
  queued*           needs_approval      needs_validation
                           |                   |
                           |            [developer validates]
                           |                   |
                           |                   v
                           |               validated
                           |                   |
                           +-------------------+
                                     |
                              needs_approval
                                     |
                             [owner approves]
                                     |
                                     v
                                  queued*      (terminal for this epic)

     At any needs_validation / needs_approval step:
       [changes requested]  -> changes_requested
       [rejected]           -> rejected
```

`*` `queued` means the request is approved and waiting for implementation.
Automated implementation and deploy via harness specialists is out of scope
for this epic and depends on the action-library epic
([#1734](https://github.com/znasllc-io/memql/issues/1734)). Until that
substrate is wired, a human or Claude carries implementation from the queue.

### Routing logic

The `routeRequest` automation fires on `node.created` for every
`v1:forge:request` and calls `logicRouteRequest`. The logic reads the
`submitterRole` stamped at insert time and picks exactly one transition:

| Submitter role | Next status | Rationale |
|---|---|---|
| `owner` | `queued` | Fast-track: self-approved, ready to implement. |
| `admin` or `writer` | `needs_approval` | Developer's own report skips first-line validation. |
| `reader` | `needs_validation` | Non-developer requests must be validated by a developer first, then approved by the owner. |

The trigger is `node.created`; `mutationAdvanceRequest` is an `update`
(emits `node.updated`), so the automation does not re-fire — no loop risk.

---

## Role mapping

Cluster roles are `owner` / `admin` / `writer` / `reader`. Forge maps these to
pipeline personas:

| Cluster role | Forge persona | Permissions |
|---|---|---|
| `owner` | Approver / fast-track | Final approval. Self-approves on submit (fast-track to `queued`). Can validate and approve other requests. Execute/deploy authority. |
| `admin` | Senior developer | May validate requests (advance to `needs_approval`) and approve them (advance to `queued`). |
| `writer` | Junior developer | May validate requests (first-line review). Cannot approve; moves requests to `needs_approval` for the owner. |
| `reader` | Non-developer employee | Submit-only. Requests enter the pipeline at `needs_validation`. Receives the mentoring layer while filing (see below). |

The validation and approval queues are gated at the query layer via the forge
role specs (`forgeDeveloper` for `owner` / `admin` / `writer`;
`forgeApprover` for `owner` only). A `reader` calling
`forgeValidationQueue` or `forgeApprovalQueue` receives an empty list, not an
error. Submission (`forgeSubmitRequest`) and reading own requests
(`forgeMyRequests`) are open to every authenticated team member.

### Mentoring layer

When a `reader` submits a request, the `forgeSubmitRequest` tool description
instructs Claude to teach the submitter about the part of the system or company
their request touches — a short, friendly explanation so they learn while they
file. This is client-side behaviour driven by the tool description; a
server-side `mentored` requestEvent is recorded when it happens. A dedicated
mentoring prompt template lands in a follow-on slice.

---

## MCP tool surface

All forge tools carry the `@mcp` annotation and are available to any MCP host
connected to a memQL cluster with the forge bundle loaded.

### Project management

| Tool | Summary |
|---|---|
| `forgeRegisterProject` | Register a new project forge develops against (call once per app or codebase). |
| `forgeActiveProjects` | List active projects (use to resolve a `projectId` before submitting). |

### Submit and browse (open to all team members)

| Tool | Summary |
|---|---|
| `forgeSubmitRequest` | Submit a request (bug, feature, chore, question, or HR action) for a project. |
| `forgeMyRequests` | List the caller's own submitted requests, newest first. |
| `forgeRequestById` | Fetch a single request by id (full detail). |
| `forgeRequestHistory` | Fetch the full lifecycle/audit trail for a request, newest first. |

### Developer/owner queues and transitions

| Tool | Summary |
|---|---|
| `forgeValidationQueue` | List requests awaiting first-line developer validation. Developer/owner only; returns empty for others. |
| `forgeValidateRequest` | Validate a request (developer action): advances to `needs_approval`, stamps the validator. |
| `forgeApprovalQueue` | List validated requests awaiting owner approval. Owner only; returns empty for others. |
| `forgeApproveRequest` | Approve a request (owner action): advances to `queued`, stamps the approver. |
| `forgeRequestChanges` | Send a request back for changes with a reason: sets status `changes_requested`. |

Identity is always stamped server-side from the authenticated actor
(`submitterUserId`, `validatedByUserId`, `approvedByUserId`, `actorUserId` on
events). No tool call can act as someone else.

---

## End-to-end employee flow

The following describes a complete request lifecycle as experienced through
Claude and the MCP connector.

### 1. Connect the MCP connector

The team member (or the operator on their behalf) connects Claude to the memQL
MCP server. See [Connect to the memQL MCP server](mcp-connect.md) for the
full connector setup.

### 2. Register a project (one-time, owner)

```
User (owner): "Register the memql repo as a forge project."
Claude: calls forgeRegisterProject with slug="memql", name="memQL", ...
```

Once a project exists, `forgeActiveProjects` resolves the `projectId` for
subsequent submissions.

### 3. Submit a request (any team member)

```
User (reader): "I keep getting a 500 when I open a space with more than 20 agents."
Claude: calls forgeSubmitRequest with requestType="bug", title="500 on spaces >20 agents",
        body="<description>", projectId="<memql project id>", priority="high"
```

If the submitter is not an `owner`, Claude also provides a short explanation of
what the system component or area of the codebase their request touches — the
mentoring layer. The request lands in the graph with `status=submitted`; the
`routeRequest` automation fires immediately and advances it to
`needs_validation` (for a `reader` submitter).

### 4. Validate (developer)

The developer's Claude calls `forgeValidationQueue` to see pending work:

```
User (writer): "What's in my validation queue?"
Claude: calls forgeValidationQueue → lists requests at needs_validation
User: "Validate request <id>."
Claude: calls forgeValidateRequest → status advances to needs_approval
```

If the request needs rework, the developer calls `forgeRequestChanges` with a
note; status moves to `changes_requested` and the submitter is informed.

### 5. Approve (owner)

```
User (owner): "Show me the approval queue."
Claude: calls forgeApprovalQueue → lists requests at needs_approval
User: "Approve <id>."
Claude: calls forgeApproveRequest → status advances to queued
```

A request submitted by an `owner` fast-tracks: `logicRouteRequest` sets status
directly to `queued` at submission time, skipping both the validation and
approval queues.

### 6. Event trail

At any point, any team member can call `forgeRequestHistory` to see the full
audit trail for a request: every routing decision, validation, approval,
rejection, comment, and mentoring touch — a durable, queryable time-series in
the memQL graph.

---

## Deferred: automated implementation

This epic (`#1785`) ends at `queued`. A request in `queued` status is approved
and ready for implementation; a human or Claude carries it from there using the
normal development tools.

Automated implementation and deploy — where an approved `queued` request
triggers harness specialists to code, test, and deploy the change — depends on
the action-library epic
([#1734](https://github.com/znasllc-io/memql/issues/1734)). Later pipeline
automations (spike / implement / deploy) will hang off subsequent request
transitions on the same spine.

---

## DSL source

All forge constructs live under `dsl/forge/`:

| File | Contents |
|---|---|
| `concepts.memql` | `project`, `request`, `requestEvent` concept schemas |
| `shapes.memql` | `projectFull`, `requestFull`, `requestEventFull` projections |
| `mutations.memql` | `mutationCreateProject`, `mutationCreateRequest`, `mutationAdvanceRequest`, `mutationValidateRequest`, `mutationApproveRequest`, `mutationRequestChanges`, `mutationRecordRequestEvent` |
| `queries.memql` | `queryActiveProjects`, `queryMyRequests`, `queryRequestById`, `queryProjectRequests`, `queryValidationQueue`, `queryApprovalQueue`, `queryRequestEvents` |
| `logic.memql` | `logicRouteRequest` — role-based pipeline router |
| `automations.memql` | `routeRequest` — fires on `node.created` for `v1:forge:request` |
| `tools.memql` | All `@mcp`-annotated tools (the team's Claude-facing surface) |
| `specs.memql` | `forgeDeveloper`, `forgeApprover` — actor-bound role gates |
