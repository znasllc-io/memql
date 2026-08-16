---
title: TimescaleDB license compliance (TSL) -- posture, client notice, confirmation request
audience: ops
status: stable
area: internal
sinceVersion: 0.9.0
owner: znas
---

# TimescaleDB license compliance (TSL)

memQL embeds **TimescaleDB Community Edition**, which is source-available under
the **Timescale License (TSL)** rather than an OSI-approved open-source licence.
Self-hosting it costs nothing for our deployment model, but the grant that makes
that true is conditional, and one of its conditions is about *how memQL describes
itself*. This document records the posture, the two affirmative artifacts, and
the one gate that keeps the posture from decaying.

Compliance pack for epic memql#3842, task memql#3843.

---

## 1. Why Community Edition, and why not Apache

The schema requires TSL-licensed features. They are not optional and not
replaceable:

| Feature | Where it is used | Verified in |
|---|---|---|
| Continuous aggregates | `code_invocation_1m`, `code_invocation_1h` | `component/database/memory-nodes/migrations/` |
| Compression policies | `code_invocation`, `MemoryNodes` | same |
| Retention policies | 7-day retention on `code_invocation` | same |

All three live in TimescaleDB's **Community (TSL)** build. The Apache-2.0 build
of TimescaleDB ships none of them. That single fact eliminates the managed
alternatives: Azure Database for PostgreSQL carries only the Apache build, and
AWS RDS ships no TimescaleDB at all. The only managed provider of the edition we
need is Tiger Cloud itself, which is what this epic is moving off.

So the choice is not "Apache vs Community". It is "Community, self-hosted" or
"Community, rented from Tiger" -- and the licence question applies either way.

## 2. The grant we rely on

**TSL §2.1(b), the "Value Added Products or Services" grant**, permits use of the
Community features inside a larger product that adds substantial value beyond the
database itself.

**TSL §3.10 prong (i)** withholds that grant from a product that is *primarily a
database storage or operations product* -- that is, one whose value proposition
is being a database. §3.10's own illustrative example of a qualifying value-added
product is "an IoT platform or vertical-specific application."

**Our reading.** memQL is an AI platform: agents, automations, tool calling,
voice, cognition routing, an identity service, and a DSL. The time-series memory
graph is the substrate those run on, not the product. A customer buys the agent
and voice platform; nobody buys memQL to get a database. That places us on the
"vertical-specific application" side of §3.10's own example.

**This is our analysis, not counsel's.** Counsel review is a parallel,
non-blocking track. §4 below is the written confirmation request to TigerData,
which is the belt to that braces.

## 3. The posture this obliges -- and the gate that holds it

If the grant turns on memQL not being primarily a database product, then **every
public claim that memQL *is* a database is a compliance liability**, not just
imprecise marketing. This repository is PUBLIC, so its prose is a public claim.

The rule:

- **Allowed** -- "built on / backed by a time-series memory graph", "PostgreSQL +
  TimescaleDB substrate", naming the storage layer precisely in architecture
  docs, and any internal document under `docs/internal/` being technically exact
  about what the storage layer is. It genuinely is a time-series graph on
  TimescaleDB, and pretending otherwise inside the repo would help nobody.
- **Not allowed** -- "memQL is a (time-series / memory graph / any kind of)
  database", in any public-facing file.

**Canonical positioning line:**

> memQL -- the AI memory platform: agents, automations, and voice on a
> time-series memory graph.

**The gate.** `TestNoDatabaseProductClaims` (`database_positioning_test.go`)
sweeps every git-tracked file for the product claim and fails the build on a hit.
It exists because this is precisely the kind of statement that gets re-introduced
by someone writing a perfectly reasonable sentence: "memQL is a time-series
database with automations" is a natural thing to type, reads as accurate, and is
the exact sentence §3.10 prong (i) is about. A convention nobody enforces would
have decayed within a release. `docs/internal/**` is exempt for the reason in the
bullet above.

## 4. Artifact A -- client-agreement notice text

To be included in client agreements for operator-run memQL instances. It does two
things: gives the TSL notice the licence expects, and closes the DDL question
contractually as well as technically.

> **Third-party components.** The memQL platform embeds TimescaleDB Community
> Edition, which is licensed by Timescale, Inc. (TigerData) under the Timescale
> License ("TSL"), available at <https://www.tigerdata.com/legal/licenses>. Your
> rights in the memQL platform are granted by this Agreement; your rights in
> TimescaleDB Community Edition are additionally subject to the TSL, and nothing
> in this Agreement grants you rights in TimescaleDB beyond those the TSL confers.
>
> **Database access.** The memQL platform is provided as an application service.
> Customer is granted access through the memQL application interfaces only.
> Customer shall not (a) issue schema data-definition statements (DDL) against
> the underlying database, (b) create, alter, or drop database objects, extensions,
> hypertables, continuous aggregates, or policies, or (c) access, resell, or offer
> to any third party the embedded database as a standalone database service.
> Provider retains sole administrative control of the database layer, including
> schema, migrations, backups, and extension lifecycle.

**Why the DDL prohibition is belt-and-braces.** It is already true technically --
customers reach memQL over gRPC/HTTP application surfaces and hold no database
credential; there is no path by which a customer could run DDL. The contractual
clause matters anyway, because it is the sentence that makes the *shape* of the
offering legible to a licensor reading the agreement: this is an application
service with a database inside it, not a database being resold.

## 5. Artifact B -- confirmation request to TigerData

Send to **licensing@tigerdata.com**. Purpose: obtain written confirmation of the
§2.1(b) reading before the platform is sold to clients, so the position is not
resting solely on our own analysis.

> **Subject:** Confirmation request -- TSL value-added use of TimescaleDB
> Community Edition
>
> Hello,
>
> We operate memQL, an AI platform (agents, automations, tool calling, and voice)
> that stores its state in PostgreSQL with TimescaleDB Community Edition. We would
> like written confirmation that our deployment model falls within the "Value
> Added Products or Services" grant in TSL §2.1(b).
>
> Our deployment model:
>
> - We self-host unmodified official TimescaleDB Community Edition packages inside
>   Kubernetes clusters we operate, one instance per client. There is no
>   multi-tenant shared database.
> - Clients access the platform exclusively through memQL's own application APIs
>   (gRPC and HTTP). No client is issued database credentials, and no client has
>   SQL, DDL, or administrative access to the database. Schema, migrations,
>   extensions, and backups are under our sole control, and client agreements
>   prohibit DDL contractually in addition to the technical block.
> - We do not offer, resell, or expose a database service, hosted or otherwise.
>   TimescaleDB is an internal implementation detail of an AI platform; our
>   product's value is the agent, automation, and voice functionality built on it.
> - We rely on Community features -- continuous aggregates, compression policies,
>   and retention policies -- as internal mechanics of that platform.
>
> Our reading is that this is the "vertical-specific application" case described
> in §3.10, and that prong (i) is not engaged because memQL is not primarily a
> database storage or operations product. We would appreciate your written
> confirmation of that reading, or notice of any condition we should meet.
>
> We are happy to provide further architectural detail.
>
> Regards,
> [name], memQL

**Status:** drafted here for the repository owner to send from a company address.
Record the send date and any reply below.

| Date | Event |
|---|---|
| _pending_ | Confirmation request sent to licensing@tigerdata.com |
| _pending_ | Reply received |

## 6. Operating rules that follow

- **Ship unmodified official packages.** The database image installs
  `timescaledb-2-postgresql-16` from Timescale's own package repository and does
  not patch it. Modifying TimescaleDB would raise questions this posture does not
  answer.
- **Never expose the database as a product surface.** No customer-facing
  "bring your own SQL", no direct DSN handed to a client, no database-as-a-feature
  in pricing copy. The memql-cloud epic's pricing pages inherit this rule.
- **Keep the positioning gate green.** If `TestNoDatabaseProductClaims` fails,
  the fix is the sentence, not the test.

## 7. Related

- Epic memql#3842 -- self-hosted database platform (CNPG on AKS, off Tiger Cloud)
- [Database setup](../../public/operate/database-setup.md) -- the operational side
- [DR runbook](dr-runbook.md) -- recovery from object-store backups
- `database_positioning_test.go` -- the gate
