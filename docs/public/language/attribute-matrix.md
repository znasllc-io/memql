---
title: MemQL Attribute Matrix
audience: public
status: stable
area: language
sinceVersion: 0.9.0
owner: znas
---

# MemQL Attribute Matrix

> **Last Updated:** 2025-12-07

This document defines all `@attribute` decorators available in MemQL and their applicability to different definition types.

---

## Attribute Applicability Matrix

| Attribute | Query | Mutation | Automation | Description |
|-----------|:-----:|:--------:|:----------:|-------------|
| **Lifecycle** |
| `@enabled` | Yes | Yes | Yes | Activates the definition (required to use it) |
| `@disabled` | Yes | Yes | Yes | Explicitly disables the definition |
| `@deprecated` | Yes | Yes | Yes | Marks as deprecated with optional message |
| `@version("v1")` | Yes | Yes | Yes | Version tag for the definition |
| **Documentation** |
| `@description("...")` | Yes | Yes | Yes | Human-readable description |
| **Access Control** |
| `@internal` | Yes | Yes | Yes | Not exposed to external API |
| `@public` | Yes | Yes | Yes | Explicitly public (default behavior) |
| `@role("admin")` | Yes | Yes | Yes | Restrict to users with specified role |
| `@permission("...")` | Yes | Yes | Yes | Require specific permission |
| **Performance** |
| `@timeout("30s")` | Yes | Yes | Yes | Maximum execution time |
| `@cache(ttl="5m")` | Yes | No | No | Cache results for duration |
| `@rateLimit(...)` | Yes | Yes | Yes | Throttle calls (requests + window) |
| **Reliability** |
| `@retry(count=3)` | No | Yes | Yes | Retry on failure |
| `@idempotent` | No | Yes | No | Safe to retry without side effects |
| **Auditing** |
| `@audit` | No | Yes | Yes | Log all executions for audit trail |
| **Triggers (Automation Only)** |
| `@trigger(event="...")` | No | No | Yes | Event-based trigger |
| `@schedule(cron="...")` | No | No | Yes | Cron-based schedule |
| `@async` | No | No | Yes | Run asynchronously when triggered |

---

## Attribute Definitions

### Lifecycle Attributes

#### `@enabled`
Activates the definition. **Required** for the definition to be used.

```memql
@enabled
query activeUsers() { ... }
```

#### `@disabled`
Explicitly disables the definition. Takes precedence over `@enabled`.

```memql
@disabled
query legacyUsers() { ... }
```

#### `@deprecated`
Marks the definition as deprecated. Optionally includes a message.

```memql
@deprecated
query oldFunction() { ... }

@deprecated("Use activeUsers() instead")
query getUsers() { ... }
```

#### `@version("...")`
Version tag for the definition.

```memql
@version("v2")
query activeUsers() { ... }
```

---

### Documentation Attributes

#### `@description("...")`
Human-readable description of the definition.

```memql
@description("Returns all active user profiles with optional filters")
query activeUsers() { ... }
```

---

### Access Control Attributes

#### `@internal`
Marks the definition as internal-only. Not exposed to the external API.

```memql
@internal
query systemMetrics() { ... }
```

#### `@public`
Explicitly marks the definition as publicly accessible (default behavior).

```memql
@public
query publicData() { ... }
```

#### `@role("...")`
Restricts access to users with the specified role.

```memql
@role("admin")
query adminDashboard() { ... }

@role("admin", "moderator")
mutation deleteUser() { ... }
```

#### `@permission("...")`
Requires the caller to have the specified permission.

```memql
@permission("read:users")
query userProfiles() { ... }

@permission("write:config")
mutation updateConfig() { ... }
```

---

### Performance Attributes

#### `@timeout("...")`
Maximum execution time. Supports duration formats: `"30s"`, `"5m"`, `"1h"`.

```memql
@timeout("30s")
query heavyQuery() { ... }

@timeout("5m")
automation longRunningTask() { ... }
```

#### `@cache(ttl="...")`
Cache query results for the specified duration. **Query only** - mutations and automations should not be cached.

```memql
@cache(ttl="5m")
query frequentlyAccessedData() { ... }

@cache(ttl="1h")
query staticConfig() { ... }
```

#### `@rateLimit(requests=N, per="duration")`
Throttle calls to the definition.

```memql
@rateLimit(requests=100, per="1m")
query apiEndpoint() { ... }

@rateLimit(requests=10, per="1h")
mutation expensiveOperation() { ... }
```

---

### Reliability Attributes

#### `@retry(count=N)`
Retry the operation on failure. **Mutation and Automation only**.

```memql
@retry(count=3)
mutation createUser() { ... }

@retry(count=5)
automation syncData() { ... }
```

#### `@idempotent`
Marks the mutation as safe to retry without side effects. **Mutation only**.

```memql
@idempotent
mutation upsertUser() { ... }
```

---

### Auditing Attributes

#### `@audit`
Log all executions for audit trail. **Mutation and Automation only**.

```memql
@audit
mutation deleteUser() { ... }

@audit
automation processPayments() { ... }
```

---

### Trigger Attributes (Automation Only)

#### `@trigger(event="...", filter="...")`
Event-based trigger for automations.

```memql
@trigger(event="session.opened")
automation onUserConnect() { ... }

@trigger(event="graph.node.created", filter="payload.concept == 'v1:crm:lead'")
automation onLeadCreated() { ... }
```

#### `@schedule(cron="...")`
Cron-based schedule for automations.

```memql
@schedule(cron="0 0 * * *")
automation dailyCleanup() { ... }

@schedule(cron="*/30 * * * *")
automation frequentSync() { ... }
```

#### `@async`
Run the automation asynchronously when triggered. The caller doesn't wait for completion.

```memql
@async
@trigger(event="report.requested")
automation generateReport() { ... }
```

---

## Examples by Type

### Query Function

```memql
@enabled
@description("Returns active user profiles with optional filters")
@cache(ttl="5m")
@timeout("10s")
query activeUsers() {
  concept==v1:identity:user;
  payload.active==true;
  ?.payload.authorizerId==args.authorizerId;
  ?.payload.role==args.role
}
```

### Mutation Function

```memql
@enabled
@description("Creates a new user with the provided details")
@audit
@idempotent
@retry(count=3)
@role("admin")
mutation createUser() {
  insert("v1:identity:user",
    id=concat("user-", args.authorizerId),
    payload={
      "authorizerId": args.authorizerId,
      "email": args.email,
      "role": args.role,
      "active": true
    }
  )
}
```

### Automation

```memql
@enabled
@trigger(event="session.opened")
@description("Auto-provisions user on WebSocket connect")
@audit
@timeout("30s")
@retry(count=3)
automation bootstrapUser() {
  
  checkUser: query {
    concept==v1:identity:user;payload.authorizerId==event("payload.subject")
  }
  
  createUser: mutation when step("checkUser").metadata.itemCount == 0 {
    insert("v1:identity:user", payload={...})
  }
  
  return coalesce(step("createUser"), first(step("checkUser")))
}
```

---

## Default Behaviors

| Aspect | Default | Notes |
|--------|---------|-------|
| Enabled state | **Disabled** | Must use `@enabled` to activate |
| Visibility | Public | Use `@internal` to hide from API |
| Timeout | 30s | Platform default |
| Cache | None | No caching by default |
| Rate limit | None | No throttling by default |
| Audit | Off | Must explicitly enable |
| Retry | 0 | No retries by default |

