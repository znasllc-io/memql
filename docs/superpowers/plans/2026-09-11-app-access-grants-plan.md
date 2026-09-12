# App access grants -- Plan

- **Date:** 2026-09-11
- **Design record:** `docs/superpowers/specs/2026-09-11-app-access-grants-design.md`
  (decisions D1 to D14; every task below cites its section).
- **Status:** planned. Implementation has not started from this plan; the owner merges
  this document and instructs separately.
- **Issues:** one GitHub issue per epic, labelled `epic`, `feature`, `claude` and
  `epic:<slug>`; one issue per task, labelled `task`, `claude` and `epic:<slug>`, each a
  native sub-issue of its epic. Numbers are filled in below once filed.
- **Local ccpm files:** `.claude/prds/app-access-grants.md` and `.claude/epics/<slug>/`
  on the planning machine. They are gitignored by repository rule (memql#3344) and are
  the working copies the GitHub sync was run from; this document is the tracked record.

## Why four epics

One epic per independently mergeable layer, ordered by dependency:

1. **Deployables ownership prerequisites.** Two engine defects and a cloud repair that
   depend on no design decision and are why deployables looked broken. First, so the
   cluster is correct while the model is built.
2. **The engine.** The grant concept, the resolver and the call-site switch, then
   governance, audit and the reads. Pure engine, testable with no app naming a resource.
3. **Apps as resources, Deployables first.** The seeds that reproduce today's floors,
   the Deployables parts, the parity gate, and the one policy departure (the account tie
   on packages), which deserves its own review.
4. **The OS.** The registry switch, attribution, and the Access settings section. Needs
   2 and 3 merged for its parity gate and its effective read.

Fewer would put a policy departure and a client rewrite in one review; more would split
work that shares files.

## PR budget

Each epic lands in one implementation PR, two at most. Never one PR per issue. The split
below is repeated in each epic issue.

## Epic 1: Deployables ownership prerequisites

Slug `deployables-ownership-prerequisites`. Epic issue: (filed after sync).

| Task | Title | PR |
|---|---|---|
| 1 | Owner erasure on the pipeline's internal-origin bind write (PR #5284, already open) | PR A = #5284 |
| 2 | Bare-versus-canonical package id compare at the four sites | PR B |
| 3 | Cloud repair: re-stamp the owner on sites blanked by the erasure | PR B |
| 4 | Archive closes a source's parked runs; the upstream feed skips archived packages | PR B |

PR A is #5284 as it stands. PR B carries tasks 2 to 4 together: they touch
`component/packages` and its tests and are reviewed as one change to the pipeline's
identity handling.

## Epic 2: Access grants, the engine

Slug `access-grants-engine`. Epic issue: (filed after sync).

| Task | Title | PR |
|---|---|---|
| 1 | The `v1:rbac:grant` concept: fields, derived id, tier, server-only mutations, snapshot | PR A |
| 2 | The actor-shaped resolver: most specific wins, deny within a level, memoised per request | PR A |
| 3 | Switch the seven `auth.Capable` call sites; database-gated enforcement tests | PR A |
| 4 | Governance builtins `grantSet` and `grantRevoke` with the four rules, codes and audit | PR B |
| 5 | The reads: `grantsForSubject`, `grantsForResource`, `effectiveCapabilitiesForActor` | PR B |
| 6 | Documentation: access-model.md, CLAUDE.md pointer, record status | PR B |

PR A makes the engine answer grants and proves it through the gates; PR B makes grants
writable and readable. PR B depends on PR A.

## Epic 3: Access grants, apps as resources, Deployables first

Slug `access-grants-app-vocabulary`. Epic issue: (filed after sync). Depends on epic 2
PR A.

| Task | Title | PR |
|---|---|---|
| 1 | Seed `read app:<id>` for every OS app on the roles the registry admits today | PR A |
| 2 | The five Deployables parts: seeds and `@requiresCapability` on their constructs | PR A |
| 3 | The registry-to-seeds parity gate | PR A |
| 4 | The account tie on `package` and `packageDeployment`; compose defaults to the self account | PR B |

PR A is the vocabulary and its gate; PR B is the data-layer half and the recorded
departure from the accounts rule. Independent of each other; PR B may land first.

## Epic 4: Access grants, the OS

Slug `access-grants-os`. Epic issue: (filed after sync). Depends on epics 2 and 3.

| Task | Title | PR |
|---|---|---|
| 1 | The shell reads the effective set; registry moves from `roles:` to `requires:` | PR A |
| 2 | Deployables controls declare their parts; hidden when missing; refusal copy | PR A |
| 3 | Attribution: "deployed by", and the Sources group lists the viewer's own first | PR A |
| 4 | Settings, Access: the by-person and by-app views over the grant builtins | PR B |

PR A is the switch nobody notices (parity gate keeps every desktop identical) plus the
two presentation fixes the owner asked for; PR B is the new screen.

## Order of merge

Epic 1 PR A (already open), epic 1 PR B, epic 2 PR A, epic 2 PR B and epic 3 PR B in any
order, epic 3 PR A, epic 4 PR A, epic 4 PR B.

## Not in this plan

Expiry on grants, grants over hosted sites, per-action grants, a live subscription to
grant rows, and the full Users-app administration surface (record C of the access
program). Each is named in the design record's "Not here".
