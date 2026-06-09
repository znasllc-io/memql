---
title: Concept Seeding
audience: public
status: stable
area: concepts
sinceVersion: 0.9.0
owner: znas
---

# Concept Seeding

**Last Updated:** 2026-02-21

Concept payloads can include optional `seed.json` files under each concept directory (for example, `concepts/assistant/seed.json`). These files describe the initial data that should exist in a fresh deployment without hard-coding payloads inside Go services.

## File Structure

```json
{
  "actor": "system",
  "records": [
    {
      "id": "system",
      "actor": "bootstrapper",
      "payload": { "... concept payload ..." },
      "match": [
        { "field": "id", "value": "system" },
        { "field": "payload.role", "value": "owner" }
      ]
    }
  ]
}
```

- `actor` (optional): default `createdBy` value used for every record unless overridden.
- `records`: list of payloads to insert.
  - `id` (optional): seed identifier; if omitted the concept library assigns one.
  - `actor` (optional): record-level override for `createdBy`.
  - `payload` (required): JSON payload that must satisfy the concept definition schema.
  - `match` (optional): zero or more equality filters used to decide whether the record already exists. Matching supports:
    - `id`, `concept`, `createdBy`, or `type` (compared to node columns).
    - `payload.<path>` for nested JSON values, where `<path>` uses dot-notation (e.g., `payload.role`, `payload.profile.displayName`).

If `match` is omitted but the record has an `id`, the seeder automatically uses the `id` field. Records without an `id` must define at least one `match` filter. Supplying a `match` clause is useful for uniqueness rules such as "only seed an owner user if no owner exists":

```json
{
  "payload": {
    "email": "owner@example.com",
    "phoneNumber": "+1-555-0100",
    "role": "owner"
  },
  "match": [
    { "field": "payload.role", "value": "owner" }
  ]
}
```

## Environment Variable Substitution

Seed files support environment variable substitution using the `${VAR_NAME}` syntax. This allows secrets and environment-specific values to be injected at runtime without hardcoding them in the repository.

### Syntax

```json
{
  "id": "api-token",
  "payload": {
    "name": "API_TOKEN",
    "value": "${MY_API_TOKEN}",
    "description": "API authentication token"
  }
}
```

When the seeder runs, `${MY_API_TOKEN}` is replaced with the value of the `MY_API_TOKEN` environment variable.

### Rules

- **Pattern**: `${VAR_NAME}` where `VAR_NAME` starts with a letter or underscore, followed by letters, digits, or underscores.
- **Missing variables**: If an environment variable is not set, the placeholder remains unchanged (e.g., `${MISSING_VAR}` stays as `${MISSING_VAR}`). This makes it obvious when a required variable is missing.
- **Recursive substitution**: Environment variables are substituted in all string values throughout the payload, including nested objects and arrays.
- **Non-string values**: Only string values are processed for substitution. Numbers, booleans, and null values are left unchanged.

### Example: Variable Concept Seed

The `v1:variable` concept uses environment variable substitution to seed configuration values:

```json
{
  "actor": "system",
  "records": [
    {
      "id": "discord-webhook",
      "payload": {
        "name": "DISCORD_WEBHOOK_URL",
        "value": "${DISCORD_WEBHOOK_URL}",
        "description": "Discord webhook for notifications",
        "category": "webhook",
        "sensitive": false,
        "active": true
      }
    }
  ]
}
```

## System Assistant Seed

`concepts/assistant/seed.json` defines the System assistant with the same payload previously created in `service/seeding.go`. The concept seeder will insert it once and skip future attempts when either the `assistant:system` record exists or another assistant matches the configured `match` filters.
