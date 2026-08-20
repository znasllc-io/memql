---
title: memQL Proving Log
audience: public
status: stable
area: overview
sinceVersion: 0.15.0
owner: znas
---

# memQL Proving Log

A timestamped scorecard that memQL is exercised as a production-grade
platform. A PM should be able to open a day file and see what was
validated, when, and the result.

This is **pass / fail / confounder per surface** — a portal section,
a component, a pack, or an engine action (query, mutation,
automation). It is not a bug dump. Individual bugs stay on GitHub.

## How to read a row

Every entry uses the same columns:

| Column | Meaning |
| --- | --- |
| **Date** | `YYYY-MM-DD` in America/Phoenix |
| **Surface** | `portal` · `unit` · `integration` · `load` · `pack` · `component` |
| **What** | One line (for example, Concept registry walkthrough) |
| **Overlay** | Short SHA (`2aaa768`). No env, no tokens |
| **Result** | `pass` · `fail` · `confounder` |
| **Engine** | Did the query, mutation, or automation land? One sentence. No payloads, no DSL dump |
| **Evidence** | In-repo cropped shot under [proving/assets/](proving/assets/) or a CI check name |

A cookie-flush on a second headless process is a **confounder**, not
a fail. Evidence never includes local paths, secrets, vulnerability
detail, PII, or customer data.

## Days (newest first)

- [2026-08-20](proving/2026-08-20.md) — owner walkthrough started on overlay `2aaa768`
