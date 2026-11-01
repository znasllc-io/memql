---
title: Evaluation case records and archives
description: A common evidence contract for real MemQL goals across categories, durations and execution routes.
audience: public
status: stable
area: overview
sinceVersion: 0.28.0
owner: znas
---

# Evaluation case records and archives

A case records a real request, every observed attempt, its results and the
evidence needed to judge them. The [proof-of-work library](proof-of-work.md)
uses this contract for both assisted development exercises and repeatable
evaluations. This is the recording and publication protocol; automatic export
for every goal and the public results website remain planned.

## Categories and goal lengths

The harness and record format remain independent of a project or task. Categories
are tags, not separate runtimes. Each case declares its expected length before
execution; measured elapsed time is a separate fact. Complexity is a declared
rubric with a version, not a score inferred from how long a run struggled.

| Area | Example request | Result and independent checks |
|---|---|---|
| Software and UI | Change how a message menu works | Patch, behavior tests, before/after visual evidence, interactions and requested delivery receipts |
| PDF and documents | Create a short report | Final file, successful open/render, required content, layout and page checks |
| Other files | Create a spreadsheet or presentation | File, format/open checks, formula or content checks and relevant rendered views |
| Research | Answer a question with sources | Report, source dates and references, claim checks and unresolved uncertainty |
| Extended workflows | Research, create several outputs and deliver them | Dependency-linked outputs, checkpoints, per-stage checks and final delivery evidence |

Use short, medium and long planned goal lengths across areas, including simple
conversation where relevant. Include unsuccessful, interrupted, blocked and
cancelled cases. A planned case is **not run** until an execution receipt exists.
Do not claim the categories above already have measured results.

## Record contract

Use stable case, scenario-version, trial and snapshot identifiers. A case may
have several trials; a trial may have root and child runs, retries and Apps
sessions. Those are different counting units. Retrying a step does not create
an independent trial. Append a new snapshot or trial instead of replacing an
earlier failure with the eventual success.

| Group | Required information |
|---|---|
| Request | Original plain-language request, ordered user/operator guidance, author kind, timestamps where known, scope and acceptance criteria |
| Setup | Project references where applicable, fixture/revision digests, engine/worker/harness versions and changes during the trial |
| Execution | Root/child run and session links, configured route/model/effort, actual served-model receipts, skill/lesson selection versions and fallback events |
| Timeline | Attempts, refusals, failures, recovery, approvals, coaching, external repairs, verification and delivery; each with attribution and evidence |
| Outcome | Status per criterion, final deliverables, independent verdict, remaining work, native CI/merge/cleanup receipts when required |
| Measurements | Wall time, recorded human waits, model/tool durations where available, calls, tokens, billing and cost; each with unit, scope, source, coverage and quality |
| Reuse | Sources/artifacts considered or used, provenance, versions/freshness checks, omissions and invalidation; missing receipts remain unknown |
| Archive | Included files, media types, byte sizes, hashes, creator, goal/run provenance and omission reasons |

Record user and operator prompts exactly when available and authorized. Preserve
public tool arguments/results needed to reproduce the finding, with redactions
listed. Do not export credentials, private instructions or private model reasoning.
When earlier prompts or events cannot be recovered, state the coverage gap.

Separate distinct defects from failed tool calls, retries, permission requests
and interruptions. A permission request is not inherently a failure. Keep both
the runtime status and independent verdict: a run marked successful can still
fail its artifact contract. Attribute outside patches and coaching explicitly;
an evaluation with implementation assistance cannot count as autonomous.

## Honest measurements and comparisons

Unknown values are null with a reason, not zero. App-reported tokens and
estimated visible-text counts remain distinct. Record configured models separately
from actual served models. A subscription or unknown billing record with a zero
cost field does not prove that the work was free. Do not infer provider charges
from a public price table or fill missing effort from the requested setting.

Report API-equivalent token estimates separately for Apps and federated inference,
using current official prices for the actual served model. Retain input/output,
cache reads and cache writes where reported, currency, source URLs, effective
dates, checked-at/refresh dates, explicit model mappings and assumptions. Unknown
model, cache allocation or context brackets remain unavailable or explicitly
bounded; session totals cannot recover per-request pricing thresholds. Keep
stale quotes labelled stale and preserve the price snapshot used by each estimate.

Local inference uses electricity estimates based on the machine’s physical
location, confirmed utility/tariff and measured or explicitly estimated energy
(kWh, or watts over a duration). Token counts do not determine energy. Preserve
time-of-use/tier uncertainty, timezone, taxes/fees and energy-allocation assumptions.
Precise machine addresses stay private and never enter evaluation archives;
publish only the minimum location and tariff evidence needed to assess the
estimate. Missing power data is unavailable cost, never free work.

Deduplicate event identities before counting append-only records. Aggregate
non-overlapping usage once: an Apps session and its router ledger receipt may
describe the same tokens. Preserve source counts and coverage; do not call a
partial session sum the complete goal's usage. Separate observed elapsed time
from active compute, queueing and human wait when the receipts permit it.

Compare trials only with compatible scenario/fixture versions, acceptance
criteria, assistance policy and measurement basis. Show model, effort, hardware,
route, cache/reuse state and harness changes. A single assisted product exercise
is an exploratory baseline, not a general success rate or proof that a particular
repair caused an improvement. Repeated compatible trials establish variability;
regressions remain visible alongside improvements.

## Downloadable evidence ZIP

Every recorded case should have a bounded snapshot archive, including unfinished
cases. Use this layout; omit empty directories and explain missing deliverables:

```text
README.md                 verdict, scope, reproduction and limitations
case.json                 request, setup, attempts, metrics and criteria
events.jsonl              ordered observed events and interventions
deliverables/             requested final outputs, when available
evidence/                 checks, renders, screenshots and receipts
support/                  useful small scripts and supporting documentation
manifest.json             file inventory, sizes, SHA-256 and provenance
```

The manifest hashes each included file except itself; an external receipt records
the archive's own hash and size. Hashes establish byte identity, not correctness.
Verify extraction, manifest coverage and checksums before retention. Record
compressed and uncompressed size limits and every exclusion. Large necessary
outputs may use separately retained, hashed parts with access-controlled references.

Whole Git repositories, dependency directories, caches, environments and unrelated
workspace files stay outside the evidence bundle. Small temporary scripts can be
evidence when they explain or reproduce the result. Keep reusable checkouts in
the workspace lifecycle; retain their repository/commit references, relevant
patches and receipts. Save evidence before cleaning up only this task's temporary
resources. A public archive is a reviewed projection of the owned MemQL journal
and Library records; creating it does not change permissions on private records.

## Planned public website

The deployable website will provide a case list filtered by category, planned
length, status and setup; a case page with request, attempt timeline, assistance,
criterion verdicts and artifact previews; compatible baseline comparisons; and
downloadable verified ZIPs. Failed and unfinished trials remain navigable.

Publication should consume versioned, reviewed exports from the existing journal,
Library and evaluation records. MemQL retains source provenance and private
records. The site displays export timestamps, coverage gaps and unavailable
metrics, and never needs runtime credentials in its public assets. Website
implementation, automatic case capture and scheduled suites are future work;
the current documentation and manually captured cases do not claim those features.

## Current corpus and packaging tool

Catalog version 1.1.0 extends the existing 14 software scenarios with six planned
scenarios: an accessible UI menu, PDF/editable-document report, workbook/slide
analysis, short sourced brief, longer research revision, and checkpointed
multi-artifact delivery. Their fixtures have exact file hashes, predeclared
short/medium/long lengths and required independent oracle contracts. They have
**no measured results**. Domain-specific oracle execution remains a prerequisite;
the existing verifier returns unverified for unimplemented criteria. The original
controlled matrix and its not-run baseline are unchanged.

`scripts/proving/archive_case.py` packages a reviewed explicit file selection.
It refuses traversal, symlinks, dependency/credential directories, duplicate
members, excessive sizes and private structured fields. It requires review
attribution because arbitrary prose and images cannot be proven private-data-free
by a filename or JSON-key check. It verifies CRC, exact manifest coverage, bytes
and SHA-256 before retaining the ZIP and a separate archive receipt. It never
uploads, grants access, publishes, or replaces an earlier snapshot.

```sh
python3 scripts/proving/archive_case.py --root /path/to/reviewed-evidence \
  --selection /path/to/selection.json --output /path/to/new-snapshot.zip
python3 scripts/proving/archive_case.py --verify /path/to/new-snapshot.zip
```

The selection names case/snapshot IDs, review scope and reviewer, omissions, and
one entry per file with `source`, archive `path`, `mediaType` and `provenance`.
`README.md` and `case.json` are required. Output limits are 200 selected files,
20 MiB per file and 100 MiB total compressed/uncompressed bytes. Selected files
are read once; the manifest binds those exact bytes. Private case snapshots remain outside the public repository until an exact
reviewed export is authorized; the public case index is intentionally empty.
