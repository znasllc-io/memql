---
title: Permissions and Access Control
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Permissions and Access Control

**Last Updated:** 2026-03-17

This document describes the permission model and access control rules for conversations, assistants, attachments, groups, and system features.

## Table of Contents

- [Role-Based Access Control](#role-based-access-control)
- [Group-Based Access Control](#group-based-access-control)
- [Conversation Access Control](#conversation-access-control)
- [Assistant Access Control](#assistant-access-control)
- [Agent Management](#agent-management)
- [Space Invitation Rules](#space-invitation-rules)
- [Attachment Access Control](#attachment-access-control)
- [System Assistant Restrictions](#system-assistant-restrictions)

## Role-Based Access Control

### User Roles

The system supports four user roles in a strict hierarchy:

1. **`owner`**: Full system access, including System assistant. Can manage all groups and users.
2. **`admin`**: Administrative access, including System assistant. Can create agents and manage assigned groups.
3. **`manager`**: Mid-level access. Can create spaces within groups and manage users in their groups.
4. **`user`**: Standard user access. Can create spaces with same-group members only.

### Privileged User Check

The `IsPrivilegedUser(user)` / `AtLeastAdmin(user)` helper function returns `true` when:
- `user.Role == "owner"` OR
- `user.Role == "admin"`

This check is used consistently across all System assistant access enforcement points.

### Role Helper Functions

| Function | True When |
|----------|-----------|
| `IsOwner(u)` | Role is "owner" |
| `IsAdmin(u)` | Role is "admin" |
| `IsManager(u)` | Role is "manager" |
| `IsUser(u)` | Role is "user" |
| `AtLeastAdmin(u)` | Owner or Admin |
| `AtLeastManager(u)` | Owner, Admin, or Manager |
| `IsPrivilegedUser(u)` | Owner or Admin (alias for AtLeastAdmin) |
| `CanCreateAgent(u)` | Owner or Admin |
| `CanManageGroup(u)` | Owner or Admin |

### Role Assignment

Roles are stamped on the `v1:identity:user.role` field by the
in-house identity service:

- The cluster owner is minted at /setup with `role=owner`.
- New internal users (email matches `IDENTITY_INTERNAL_DOMAINS`)
  default to the cluster's `internalDefaultRole`.
- External users default to `reader` and live in their own
  personal partition.
- Admins re-assign roles via the /admin/users/detail editor.

### Permission Matrix

| Operation | Owner | Admin | Manager | User |
|-----------|-------|-------|---------|------|
| View System assistant | [OK] | [OK] | -- | -- |
| Create conversation with System | [OK] | [OK] | -- | -- |
| Invite System to conversation | [OK] | [OK] | -- | -- |
| Create agents | [OK] | [OK] | -- | -- |
| Assign agents to groups | [OK] (any) | [OK] (own groups) | -- | -- |
| Manage groups | [OK] | [OK] | -- | -- |
| View regular assistants | [OK] | [OK] | [OK] | [OK] |
| Create spaces | [OK] | [OK] | [OK] | [OK] |
| Invite to space | [OK] (any user) | [OK] (any user) | [OK] (same group) | [OK] (same group) |
| See all agents | [OK] | [OK] | group only | group only |
| Manage users | [OK] | own group | -- | -- |
| Upload attachments | [OK]* | [OK]* | [OK]* | [OK]* |
| Use STT transcription | [OK] | [OK] | [OK] | [OK] |

*Requires participant membership in the conversation.

## Group-Based Access Control

### Overview

Groups represent organizational units inside CoPresent. They
provide scoped access control:
- Users belong to one or more groups
- Agents are assigned to groups
- Space invitations are scoped by group membership

### Group Model

Groups are stored as `v1:identity:group` concepts with:
- `name`: Display name
- `externalId`: free-form back-reference for external systems;
  empty for cluster-native groups
- `memberIds`: User IDs in this group
- `agentIds`: Agent IDs assigned to this group

### Group Membership

Group membership is managed manually from the CoPresent Settings
panel today. A future SCIM-style sync job can populate `externalId`
+ `sourceRef` and own its own rows without disturbing manual ones.

### Agent Group Assignment

- Agents can be assigned to one or more groups via `groupIds`
- Agents with no `groupIds` (empty array) are global/unscoped -- visible to all users
- Owner can assign agents to any group; Admin can assign to their own groups

## Conversation Access Control

### Participant-Based Access

Conversations use a participant-based access control model. Only users listed in a conversation's `participants` array can:

- View the conversation
- List the conversation
- Read messages from the conversation
- Post messages to the conversation
- Upload attachments
- Retrieve attachments
- Invite other users
- Invite assistants

### Default Behavior

When a conversation is created:
- The `createdBy` user is automatically added to the `participants` list.
- The creator becomes the first participant.

## Assistant Access Control

### Listing Assistants

**Access Rules:**
- All authenticated users can list assistants.
- **Exception**: The "System" assistant is filtered out for non-privileged users (users without `owner` or `admin` role).

### Creating Conversations with Assistants

**Access Rules:**
- All authenticated users can create conversations with regular assistants.
- **Exception**: Creating conversations with the "System" assistant requires `owner` or `admin` role.
- Non-privileged users receive HTTP 403 when attempting to create a conversation with System assistant.

## Agent Management

### Creating Agents

Only users with `owner` or `admin` role can create agents. The backend enforces this via `CanCreateAgent()` check.

### Agent Visibility

- Owner/Admin see all agents regardless of group assignment
- Manager/User see only agents assigned to their groups (plus global agents with no group)

## Space Invitation Rules

### Group-Scoped Invitations

- Owner/Admin can invite any user to a space
- Manager/User can only invite users who share at least one group with them
- The InviteUserModal filters the user list based on shared group membership

## Attachment Access Control

### Upload Permissions

- Only conversation participants may upload attachments
- Non-participants receive HTTP 403

### Retrieval Permissions

- Only conversation participants may retrieve attachments
- Non-participants receive HTTP 403

## System Assistant Restrictions

### Overview

The "System" assistant is a privileged SI persona meant exclusively for platform setup, configuration, and administrative workflows. It is restricted to users with `owner` or `admin` roles.

### Visibility Restrictions

- System assistant is **excluded** from assistant listings for non-privileged users
- System assistant **appears** normally for owners and admins

### Access Rules on Existing Conversations

If a non-privileged user ends up in a conversation where System assistant is present:
- **Reading past messages**: Allowed (for consistency)
- **Selecting System as active assistant**: Prevented by frontend restrictions
- **Initiating messages addressed to System**: Prevented by backend validation

## Error Responses

### HTTP 403 Forbidden

Returned when:
- User is not a conversation participant (for conversation operations)
- Non-privileged user attempts to access System assistant
- User lacks required role for the operation (e.g., non-admin trying to create agent)

### HTTP 404 Not Found

Returned when:
- Conversation, assistant, attachment, group, or user does not exist

## Audit Logging

All access denials are logged for auditing purposes:
- Attempts to list System assistant by unauthorized users
- Attempts to create agents by non-admin users
- Attempts to invite users from outside the caller's groups

Log entries include:
- User ID
- User role
- Resource ID (if applicable)
- Timestamp

## Best Practices

1. **Always Check Participant Membership**: Before performing any conversation operation, verify the user is in the `participants` list.
2. **Use Privileged User Helper**: Use `IsPrivilegedUser(user)` / `AtLeastAdmin(user)` consistently for System assistant and agent creation checks.
3. **Use Group Helpers**: Use `sharesGroup()` for invitation scoping.
4. **Log Access Denials**: Log all 403 responses for security auditing.
5. **Respect Role Hierarchy**: Owner > Admin > Manager > User.
6. **Validate Agent Visibility**: Filter agents by group membership for non-privileged users.
