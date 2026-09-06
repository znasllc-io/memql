---
title: Permissions and Access Control
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Permissions and Access Control

**Last Updated:** 2026-08-18

This document describes the permission model and access control rules for
users, groups, and agents.

## Table of Contents

- [Role-Based Access Control](#role-based-access-control)
- [Group-Based Access Control](#group-based-access-control)
- [Agent Management](#agent-management)
- [Error Responses](#error-responses)
- [Audit Logging](#audit-logging)
- [Best Practices](#best-practices)

## Role-Based Access Control

### User Roles

`v1:identity:user.role` (see `dsl/identity/concepts.memql`) supports five
values:

1. **`owner`**: Full system access. Manages all users and groups, with one
   carve-out: an owner cannot manage (edit role / delete) another owner --
   this prevents mutual-demotion lockout.
2. **`admin`**: User-management authority (create/view/manage users and
   groups, create agents) equivalent to `owner` for those operations, but
   **not** engineering power -- no DSL authoring, no inline DSL execution.
3. **`developer`**: Engineering power -- authoring DSL constructs, running
   inline DSL, cutting/deploying versions -- but **no** user-management
   authority (cannot create/manage users or groups). `owner` is a superset of
   `developer`; `admin` gains neither authoring nor inline-DSL access.
4. **`writer`**: Standard read/write access to data. No user-management or
   engineering authority.
5. **`reader`**: Read-only access to data.

`admin` and `developer` are **different power axes** at a similar privilege
level, not one strictly above the other -- see `component/auth/rbac.go` and
`component/auth/rbac_model.go` for the capability model each role resolves
through.

### Per-row authorization is the actual access gate

Role determines *capability* (may this caller perform this class of
operation at all). *Which rows* a caller can read or write is governed
separately by **per-row authorization**: every query and mutation in the
DSL classifies as **owned** (filter on `ownerUserId == actor.userId`),
**granted** (relationship predicate gates on `actor.userId`), **admin**
(cluster-owner spec), or **public** (`@public` annotation). See this
repo's `CLAUDE.md` "Authorization model" section for the canonical
description. There is no per-tenant/per-partition access dimension --
partition, where it still exists, scopes configuration and secrets
resolution, not row-level access (see
[Partition scoping](partition-scoping.md)).

### Role Helper Functions

The Go-side capability checks live in `component/auth/rbac.go`:

| Function | True When |
|----------|-----------|
| `IsOwner(u)` | Role is `owner` |
| `IsAdmin(u)` | Role is `admin` |
| `IsWriter(u)` | Role is `writer` |
| `IsReader(u)` | Role is `reader` |
| `IsPrivilegedUser(u)` | Owner or admin (user-management authority) |
| `AtLeastAdmin(u)` | Owner or admin (the user-management gate) |
| `AtLeastDeveloper(u)` | Owner, admin, or developer (the deploy-forward gate; rollback still requires `AtLeastAdmin`) |
| `CanWrite(u)` | Owner, admin, developer, or writer (any role except reader) |
| `CanAuthor(u)` | Owner or developer only -- may author DSL constructs |
| `CanRunInline(u)` | Owner or developer only -- may run ad-hoc inline DSL |
| `CanRead(u)` | Any valid role |
| `CanCreateAgent(u)` | Owner or admin |
| `CanManageGroup(u)` | Owner or admin |
| `CanViewUser(caller, target)` | Self, or caller holds user-management authority (owner sees everyone; admin sees everyone except other owners) |
| `CanManageUser(caller, target)` | Self, or caller out-ranks the target under user-management authority (an owner cannot manage another owner -- prevents mutual-demotion lockout) |
| `CanDeleteUser(caller, target)` | `CanManageUser` and not "owner deleting themselves" |

### Role Assignment

Roles are stamped on the `v1:identity:user.role` field by the
in-house identity service:

- The cluster owner is minted at /setup with `role=owner`.
- New internal users (email matches `MEMQL_IDENTITY_INTERNAL_DOMAINS`)
  default to the cluster's configured `MEMQL_IDENTITY_INTERNAL_DEFAULT_ROLE`
  (default `writer`).
- External users default to `reader` and are provisioned their own personal
  partition (MemQL's per-tenant configuration/secret scope -- see
  [Partition scoping](partition-scoping.md) -- not a row-level access
  boundary).
- Admins re-assign roles from the MemQL portal at `/admin/people`. The
  write is gated server-side in `component/identity/adminops` (owner or admin)
  and appends a `user_role_changed` audit event; a refusal appends
  `admin_auth_forbidden`.

### Capability Matrix

| Operation | Owner | Admin | Developer | Writer | Reader |
|-----------|-------|-------|-----------|--------|--------|
| Create / manage users | [OK] | [OK] | -- | -- | -- |
| Create agents | [OK] | [OK] | -- | -- | -- |
| Manage groups | [OK] | [OK] | -- | -- | -- |
| Author DSL constructs | [OK] | -- | [OK] | -- | -- |
| Run inline DSL | [OK] | -- | [OK] | -- | -- |
| Cut / deploy a version | [OK] | [OK]* | [OK] | -- | -- |
| Roll back a deployment | [OK] | [OK] | -- | -- | -- |
| Write data | [OK] | [OK] | [OK] | [OK] | -- |
| Read data | [OK] | [OK] | [OK] | [OK] | [OK] |

*Deploy-forward uses `AtLeastDeveloper` (owner/admin/developer); rollback
uses the stricter `AtLeastAdmin` (owner/admin only).

## Group-Based Access Control

### Overview

Groups (`v1:identity:group`, see `dsl/identity/concepts.memql`) are
organizational units used to scope agent visibility:
- Users belong to one or more groups (`memberIds`)
- Agents are assigned to one or more groups (`agentIds`)

### Group Model

`v1:identity:group` carries:
- `name` / `description`: Display name and short purpose blurb
- `externalId`: free-form back-reference for a legacy external sync source;
  empty for in-app-created groups
- `memberIds`: User IDs that belong to this group
- `agentIds`: Agent IDs assigned to this group
- `maxHumans` / `maxAgents`: Convention capacity hints (not enforced on
  insert -- a writer must supply them)
- `active`: Whether the group is active

### Agent Group Assignment

- Agents can be assigned to one or more groups via `groupIds`
  (`v1:agents:agent.groupIds`)
- Agents with no `groupIds` (empty array) are global/unscoped -- visible to
  all users
- `CanManageGroup` (owner or admin) gates group management operations

## Agent Management

### Creating Agents

Only users with `owner` or `admin` role can create agents
(`CanCreateAgent`).

### Agent Visibility

- Agents with `groupIds` set are scoped to those groups.
- Agents with no `groupIds` (empty array) are global/unscoped and visible
  to all users.

## Error Responses

### HTTP 403 Forbidden

Returned for role-gated operations when the caller's role lacks the
required capability (e.g., a non-admin attempting to create an agent, or a
non-privileged caller attempting a user-management operation).

### HTTP 404 Not Found

Returned when:
- A group or user does not exist
- A caller lacks ownership of a row behind one of the Library's byte routes
  (`GET /artifacts/{id}/content`, `component/server/artifact_handler.go`) --
  a deliberate 404-not-403, so probing an id cannot distinguish "exists but
  not mine" from "doesn't exist"

## Audit Logging

Access-relevant identity operations are logged for auditing purposes,
including:
- `user_role_changed` -- a role reassignment
- `admin_auth_forbidden` -- a refused admin-gated operation

Log entries include user id, role, resource id (if applicable), and
timestamp. See `component/identity/adminops` and
`v1:identity:auditEvent`.

## Best Practices

1. **Prefer the per-row authz classification over ad-hoc role checks** for
   data access -- owned/granted/admin/public, not a role comparison, is the
   canonical gate for whether a caller may read or write a given row.
2. **Use the `component/auth/rbac.go` helpers** (`CanCreateAgent`,
   `CanManageGroup`, `AtLeastAdmin`, `AtLeastDeveloper`, ...) rather than
   comparing `role` strings directly.
3. **Use group membership** to scope agent visibility for non-privileged
   users.
4. **Log access denials** for auditing.
5. **Do not conflate role (capability) with per-row ownership (data
   access)** -- a `writer` can write data they own; role alone does not
   decide which rows those are.
