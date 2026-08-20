---
title: MemQL Cloud trials, hibernation, and the condensed profile
audience: public
status: stable
area: operate
sinceVersion: 0.13.0
owner: znas
---

# Trials, hibernation, and what "condensed" actually costs

**Audience:** operators running MemQL Cloud.
**Epic:** memql#3852. **This task:** memql#3856.
**Companions:** [the control plane](memql-cloud.md), [billing](memql-cloud-billing.md).

## The trial, end to end

```
day 0    signup, card required   →  trial instance (solo profile, entry DB, hard caps)
day 7    nudge
day 12   nudge — "two days, then it pauses"
day 14   suspend                 →  scale to zero, data intact
day 28   teardown                →  final backup, then destroy
```

Five scheduled sweeps do all of it, in `deploy/fleet/dsl/fleet/trial.memql`.
Convert at any point before day 28 and the instance resumes with nothing lost —
a resume is a scale-up, not a restore.

### What bounds a trial's cost is not the clock

The sweeps bound a trial in **time**. What bounds it in **spend** is the
[ceiling inside the tenant](memql-cloud-billing.md#two-layers-of-allowance-and-why-one-is-not-enough)
— 500 message credits, 60 voice minutes, `overagePolicy: throttle`, enforced
in-process with no network.

That distinction is the whole safety argument. **A sweep runs once a day, and a
runaway does not wait for it.** A trial's card has been authorized and never
charged, so an unbounded trial is an unbounded gift available to anyone who can
complete a checkout form.

### Why the sweeps read the way they do

Everything else in the fleet reacts to a row that changed. A clock cannot —
nothing writes a row when a date passes — so these five are scheduled, and they
**read**, which the rest of the domain never does.

An automation's actor is not the row's owner, so a caller-scoped read from
inside one returns nothing *while looking entirely correct*. What makes these
legal is `@serverOnly` (refuses a client-originated call) plus `@unbounded`
(declares that reading every row is the intent, with the reason recorded, so the
pagination gate does not truncate a sweep to its first page and leave the tail
of the fleet unswept). `deploy/fleet/authz_test.go` requires **both** — either
alone is a defect wearing the other's clothes.

**A sweep cannot join**, which is why the clocks are denormalised onto the rows
they sweep: `instance.trialEndsAt` is copied from the subscription, and
`instance.suspendedAt` is the grace clock on the *data* rather than on the
billing relationship. Not duplication for convenience — it is what makes the
sweeps possible at all.

### The `where` clause is the only thing between a sweep and the fleet

The candidate queries are unwindowed by design (a filter cannot call
`addDuration`), so the bracket lives in each automation's `forEach ... where`.

Drop it from `teardownAfterGrace` and the next 04:00 UTC run destroys **every**
suspended tenant, having taken a final backup of each and reported complete
success. `TestEverySweepNarrowsItsCandidates` refuses a bare `forEach`, and
`TestTheDestructiveSweepIsTheOnlyTeardownPath` keeps `requestInstanceTeardown`
to exactly one caller — because what protects a customer's data here is a
**sequence** (pause, fourteen days, teardown), and a second caller removes the
fourteen days without removing anything that looks like a safeguard.

## Hibernation applies to every tier

Any instance that has served nothing for 14 days scales to zero.

Not just trials, deliberately: a paying customer's idle instance costs us
exactly as much as an idle trial's, and resuming is a scale-up — the data never
moved. What a paid tier buys is capacity when they use it, not "always warm"; a
tier that genuinely needs zero cold starts is a product decision with a price
attached.

`lastActivityAt` is stamped by the **tenant's own heartbeat**, because the
control plane cannot see the tenant's traffic. An instance that has never
reported activity has an empty field and is **skipped** rather than hibernated —
fail-safe in the direction that costs money rather than the one that surprises a
customer.

## The condensed profile: what was asked, and what is actually true

The task asks us to *"verify what the untagged single-process build actually
covers (identity? cognition? voice?)"*. Measured rather than assumed, and the
answer changes the plan.

### There is no single-process build, and there cannot be one by combining tags

Build tags are **mutually exclusive** by design — `docs/public/build/build-tags.md`
says so explicitly, and `app/build_default.go` is guarded
`!cognition && !agent && !planner && !bff && !voice && !identity && !workbench && !mcp && !edge`.
There is no `-tags "identity cognition agent"` binary and adding one is not a
flag, it is an engine change.

### What the untagged binary does cover

| Included | How |
|---|---|
| bff transport, engine, core integrations | `build_default.go` |
| cognition | `integrations_cognition_init.go` — `!agent && !planner` |
| polyphon room tokens | `engine_polyphon.go` — `!agent && !planner` |
| STT | `integrations_stt.go` — `!planner` |
| voice transport | `transport_voice.go` — `!agent && !planner` |

| Excluded | Why |
|---|---|
| **identity** | its own tag — and a tenant with no sign-in is not a tenant |
| **agent** | its own tag — the tool surface, i.e. most of what an agent does |
| planner, workbench, mcp, edge | their own tags |
| audio WS | `transport_audio.go` — `agent \|\| voice` |

Measured at **76 MB** untagged (the build-tags table's "~25 MB" for bff is
stale; noted, not fixed here).

### So the floor is three app pods, not one

A functioning tenant needs at minimum: the untagged binary (bff + cognition +
polyphon + STT), **identity**, and **agent**. Plus the database.

**That is the finding.** The epic's "single-process/reduced-pod 'solo' overlay
(1-3 pods)" is right about the range and wrong about the mechanism: you get
there by *scaling the mesh down*, not by *collapsing it into one process*.

### What `solo` therefore is, today

The **replica-count** condensation, shipped in
[the tenant component](https://github.com/znasllc-io/memql/tree/main/deploy/k8s/components/tenant):
one replica of each mesh node, and the entire voice lane — including LiveKit's
redis and SIP sidecars — at zero.

| | pods | notes |
|---|---|---|
| base mesh (untuned) | 13 | every node at ≥1 |
| `solo` | **8** | voice lane (5) at zero |
| `solo` + HA add-on | 8 | mesh doubles to 2 replicas each; voice stays at zero |

The five zeroed pods are the honest, measured saving. **The dollar figure is not
measured here** — the epic's ~$143 → ~$90 estimate needs a running cluster and a
month of billing to confirm, and quoting it as measured when it is modelled
would be the same class of error as the
[provider-cost drift](memql-cloud-billing.md#provider-costs-feed-the-margins)
this epic already had to fix.

### Going below three pods

Would need a new engine build tag composing node types — and that is a bigger
decision than it looks. Multi-node is the engine's default for a reason: the bug
class where state lives on one node while another handles the request is what
the two-replica parity cluster exists to catch. A one-process build makes that
class *untestable in the shape customers would run*, which is how you ship a
tier whose bugs only appear on the tier above it.

Worth doing, worth doing deliberately, and not worth doing inside a trial task.

## Explicit non-goal: no shared-graph tenancy

**Partitioning stays retired** (memql#56). No shared-graph multi-tenancy, no
partition revival. Instance isolation is the model and it is a selling point,
not an implementation detail.

The economics say the same thing: shared-graph tenancy is months of
security-critical work to save roughly what the replica-count condensation above
saves in days — and it trades a property customers pay for (their data is on
their own database) for a property they cannot verify.

## Related

- [The control plane](memql-cloud.md) — the lifecycle these sweeps drive
- [Billing](memql-cloud-billing.md) — the ceiling that bounds a trial's spend
- [Build tags](../build/build-tags.md) — the mutual exclusivity measured above
- [Environment parity](environment-parity.md) — why `solo` is a profile, not a branch
