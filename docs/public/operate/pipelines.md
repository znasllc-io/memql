---
title: Pipelines -- a repository's checks, run by the cluster and reported on GitHub
audience: public
status: stable
area: operate
sinceVersion: 0.25.0
owner: znas
---

# Pipelines

**Audience:** owners connecting a repository's checks, and operators running the cluster they run on.
**Design:** `docs/superpowers/specs/2026-09-16-pipelines-program-design.md`, decisions D1-D16 and its implementation notes for epic memql#5477.

A pipeline is a repository's checks: declared in the `pipeline:` block of the
repository's `memql-package.yaml`, delivered by the cluster's GitHub App, run by
the cluster as a work goal, and reported back to GitHub as a check run on the
commit it checked. A pull request runs what it affects; the merge queue, a push
to the default branch and a published release run everything.

> **What runs today.** This release takes deliveries, opens and deduplicates
> runs, plans them, executes their steps and reports them on GitHub. A step
> runs as a Kubernetes Job on the cluster, or on one of the owner's own machines
> when it names a need: [the pipelines substrate](pipelines-substrate.md) is
> that half, and what an operator sets up before the first run. Read
> [What runs today](#what-runs-today) before you make the check required.

A pipeline is a fact about a source. Each Deployables source
(`v1:platform:package`) has at most one, owned by the source's owner, and every
GitHub token a run uses is minted through that owner's GitHub connection. It is
not the order a deploy runs in, which [Packages](packages.md#the-order-and-why-it-never-changes)
also calls a pipeline: a pipeline here checks commits, and never starts a
deploy by itself.

**A repository has one pipeline.** Two would write two check runs of the same
name on every commit, so connecting a repository that another source's pipeline
already runs is refused `pipeline_already_connected`. To move a repository's
checks to another source, disconnect the pipeline it has first
([Connecting a pipeline](#connecting-a-pipeline)).

| Row | One per | What it holds |
|---|---|---|
| `v1:pipelines:pipeline` | source | The repository, how deliveries arrive, where steps may run, which secrets they may resolve, what the poll has seen, the timing table. Never the manifest: every run reads that at its own commit |
| `v1:pipelines:run` | attempt of one run key | The event and mode, status and conclusion, the refusal that ended it, the check run's id and state, the stage table, the agent driving it |
| `v1:pipelines:channel` | notify target | Where a notify stage delivers. Its writers arrive with epic memql#5480 |

A run compiles into one `v1:work:goal` with one `v1:work:step` per step, so the
work spine's journal and the Nexus drawing show it like any other work. Every
row is readable by its owner, by a cluster owner, and by members of the account
its source belongs to -- exactly the people who read the source. None holds a
token, a secret value or a log line.

---

## Connecting a pipeline

**Who:** the source's owner, holding `execute` on `app:deployables/connect`.
**How:** in MemQL OS, **Connect pipeline** on the source's page in Deployables
([below](#in-memql-os)); or the `pipelinesConnect` builtin.

Before connecting:

1. **The cluster has a GitHub App that holds checks write.** See
   [GitHub Connect](github-connect.md#creating-the-github-app); an app
   registered before pipelines needs
   [the upgrade](github-connect.md#upgrading-an-app-registered-before-pipelines).
   Webhook delivery also needs the app's webhook on and subscribed to the five
   events that page lists.
2. **The source is a GitHub repository fetched through your GitHub
   connection**, not a pasted token. A pipeline acts through the app's
   installation, which a pasted token does not have: switch the source to the
   connection first.
3. **`memql-package.yaml` at the default branch's head has a `pipeline:` block
   that validates** ([the block](#the-pipeline-block)).

Then:

```
builtin pipelinesConnect(packageId: "<package id>", delivery: "webhook")
```

or from an SDK: `pipelinesConnect({ packageId, delivery })` on the TS
`QueryClient`, `PipelinesConnect` in `sdk/go/client`.

| Argument | Values | Meaning |
|---|---|---|
| `packageId` | required | The source to connect. It must be yours |
| `delivery` | `webhook` or `poll`, required | How changes reach the pipeline ([Delivery](#delivery-webhook-or-poll)) |
| `compute` | `cluster`, the default, or `cluster_and_fleet` | Where steps may run ([Compute](#compute)) |
| `secretNames` | a list of names | The secrets steps may resolve: the owner's allowlist ([Secrets](#secrets)). A name beginning `MEMQL_` is refused |

Before any request leaves the cluster, connect refuses a repository another
source's pipeline already runs, `pipeline_already_connected`: a repository has
one pipeline, so its checks are reported once per commit. The remedy is to
disconnect the other source's pipeline -- its owner does, with
`pipelinesDisconnect`, or a cluster owner does when that owner has left -- or to
work from that source. The refusal says that such a pipeline exists and nothing
about whose it is; a disconnected pipeline blocks nothing.

Connect then proves the source's connection still reaches the repository by
minting an installation token through it, reads the default branch's head and
the manifest there, and validates the block. A source with no block is refused
`pipeline_not_declared`, and a block that does not validate is refused with its
own code ([Refusal codes](#refusal-codes)), so a block that can never compile is
never connected; a manifest that does not parse at all is refused
`package_manifest_invalid`, as it is for a deploy. Connect also asks what this
connection allows of every step, whatever event would plan it: a step naming a
need on a `cluster` pipeline, or a secret `secretNames` does not hold, is
refused with its own code now rather than on the first push that reaches it,
and every run asks again of the stages it runs ([Secrets](#secrets),
[Compute](#compute)). The connection's own failures are the ones a fetch gives
-- `credential_not_found` (the source fetches under a pasted token, or names no
connection), `credential_revoked`, `reconnect_required`,
`repository_not_installed`, `installation_pending`,
`github_app_not_configured` -- as
[GitHub Connect](github-connect.md#what-a-person-sees) describes them.

It answers `{pipelineId, repository, delivery, compute, stages}`, the stages
being the block's stage names in order. **Connecting runs nothing:** the first
run comes with the next push or pull request.

Afterwards:

- **Reconnect** with the same call. The pipeline keeps its id; the call restates
  delivery, compute and the allowed secret names and reactivates a
  disconnected pipeline, while what the poll has seen and the timing table
  carry over. Pass the whole configuration each time. Reconnecting is refused
  `pipeline_already_connected` when another source's pipeline has taken the
  repository meanwhile, as any second pipeline is. Reconnect, too, after
  reinstalling the GitHub App on the repository or renaming the repository:
  deliveries are matched on the installation and the repository name a
  pipeline was connected with ([Delivery](#delivery-webhook-or-poll)).
- **Disconnect** with `builtin pipelinesDisconnect(pipelineId: "<pipeline id>")`
  (`execute` on `app:deployables/connect`). It opens no more runs, and a run no
  agent has started yet concludes `pipeline_disconnected`. The pipeline and its
  runs stay, as the history, and the repository is free for another source's
  pipeline.
- **Re-run** with `builtin pipelinesRerun(runId: "<run id>")` (`execute` on
  `app:deployables/rerun`): the next attempt of the run's key, in the
  original's mode and event, with a new check run. The original stays as it
  ended.
- **Re-run failed** with `builtin pipelinesRerun(runId: "<run id>", failedOnly:
  true)`: the same new attempt, running only what the original did not pass.
  Every step the original passed **with the same package slice** is carried
  over as skipped `pipeline_passed_earlier` (*Passed in attempt 1.*), and
  everything else runs. A shard whose slice moved since -- the timing table
  learned between the attempts -- runs again, because a pass over other
  packages is not a pass of these. A run with no failed or cancelled step (one
  that passed, or one refused before any step) is refused
  `pipeline_nothing_to_rerun`. GitHub's own re-request stays a whole re-run:
  one check run reports the whole run.
- **Cancel** with `builtin pipelinesCancel(runId: "<run id>")` (`execute` on
  `app:deployables/cancel`). It flags the run; the agent driving it cancels
  what is executing at its next heartbeat and concludes the run cancelled. A
  queued run no agent has claimed is concluded cancelled at once. A new push
  to a pull request cancels the pull request's earlier runs the same way
  ([Cancel-on-push](pipelines-substrate.md#cancel-on-push)).

Each acts on your own pipeline or run only. A caller who does not own it is
refused by name before anything is written. The one exception is disconnect: a
cluster owner may disconnect any pipeline, so an operator can free a repository
whose pipeline belongs to somebody who has left. It writes the pipeline's
status and nothing else.

**Preview** with `builtin pipelinesPreview(packageId: "<package id>")`
(`execute` on `app:deployables/connect`) to read what connecting would act on,
writing nothing: the same read connect makes -- the grant proved by a mint, the
default branch's head and the block there, validated -- answered as the stages
and steps (their shape, never a command), the needs and secrets they name, the
check's name, the source's existing pipeline for a reconnect, and a delivery
suggestion (webhook where GitHub can plausibly reach the cluster, poll
otherwise). A typed refusal is the answer's `refusal`, not an error.

## In MemQL OS

A pipeline lives in **Deployables**, on its source -- not in an app of its own
(design record D12).

- **The source's page** carries a **Checks** part between its facts and the
  apps it produces: the newest run on each branch, the default branch first,
  each opening its run. Its facts gain a **Pipeline** line (the manifest, how
  many stages the newest run planned, how changes arrive, where steps run), and
  **Latest upstream** says what that commit's checks said -- *checks passed,
  not yet deployed* among them. The bar reads *checks on* and carries **Pipeline
  settings**, or **Connect pipeline** for a source that has none. A source that
  produces no apps and runs a pipeline -- the engine repository is one -- reads
  *pipeline* where its app count goes in the Sources list, and the Overview map
  draws a **Checks** node beside the deployables a checked source serves.
- **Connect pipeline** is the add-a-deployable wizard's device over the source,
  three steps. **Repository** reads `memql-package.yaml` at the default
  branch's head with `pipelinesPreview`, so the stages show before anything is
  confirmed. **Compute** offers *Cluster* and, only when one of your machines
  reports `pipelines=allowed`, *Cluster and your fleet* -- the fleet alone
  when a step names a need, which the cluster refuses -- and waits for a
  choice, because running steps on your own machines is consent. **Confirm**
  names the check, the stages and the secrets the steps read, and asks how
  changes arrive. A refusal lands at the stop it is about, with its remedy: a
  fleet code at Compute, a secret code at Confirm, everything else at
  Repository. Nothing is written until Connect, and leaving keeps the answers.
- **Runs** is a tab of its own, after Sources (`read app:deployables/runs`):
  every run of every source you connected, newest first, grouped by day,
  Refine for the source, branch and outcome. A run rings once, when it has an
  answer.
- **A run's page** draws the stages as stops across the top, read from the step
  rows; the open stop's steps say where each ran and for how long; a failed
  step shows its last lines -- read from its full log, which the runner keeps in
  your Library -- with **Open the full log**; a skipped step says why; the
  artifacts open in Files. One bar carries the state and the acts the run
  allows: **Open on GitHub**, **Re-run**, **Re-run failed**, or **Cancel** while
  it runs. The check run's details link, `https://os.<domain>/?pipelineRun=<run
  id>`, opens it.
- **Settings -> Pipelines** (`read app:settings/pipelines`, the cluster owner)
  is the [readiness](#readiness) item.

---

## The `pipeline:` block

The block sits beside `deployables:` in [the manifest](packages.md#the-manifest);
a repository with checks and no apps declares it alone. An example -- the
pipeline the design record sketches for MemQL's own repository:

```yaml
formatVersion: 1
name: memql
pipeline:
  image: ghcr.io/znasllc-io/memql-toolchain@sha256:...
  services:
    postgres:
      image: ghcr.io/znasllc-io/timescaledb-pgvector@sha256:...
      env:
        POSTGRES_PASSWORD: postgres
      ready: pg_isready -U postgres
  caches: [go, npm]
  select:
    go: import-graph
    dbGated: [component/memql, component/database]
    buckets:
      gates: ["**/*.md", "**/*.memql", "dsl/**", "scripts/**", "deploy/**"]
      os:    ["clients/**", "sdk/ts/**", "brand/**"]
  stages:
    - name: checks
      steps:
        - name: build-vet
          run: go build ./... && go vet ./...
    - name: tests
      needs: [checks]
      steps:
        - name: go-tests
          run: go test $MEMQL_PACKAGES
          packages: affected
          shards: 4
        - name: db-tests
          run: MEMQL_REQUIRE_DB=1 go test $MEMQL_PACKAGES
          packages: affected
          only: db-gated
          services: [postgres]
          shards: 4
        - name: os-checks
          run: make os-typecheck os-test os-build
          when: { bucket: os }
          needs: { docker: true }
    - name: deploy
      on: [push]
      steps:
        - name: verify-rollout
          run: ./scripts/verify-rollout.sh --target=https://api.<domain> --version="$MEMQL_VERSION"
          secrets: [VERIFY_TOKEN]
    - name: notify
      on: [push]
      channel: znas-instance
```

Connected with `compute: cluster_and_fleet` (the `os-checks` step needs
`docker`) and `secretNames: [VERIFY_TOKEN]`, it runs `checks` and then `tests`
on every run, and `deploy` and `notify` only on a push to the default branch.
`dbGated` is what `only: db-gated` reads, and the block is refused without it;
the sidecar's `env` and `ready` are what a Postgres image needs to start and to
be waited on.

**Two checks, at two times.** The block is read by the manifest's one strict
decoder, so a misspelled key anywhere inside it -- `shardz:`, `cachez:` --
refuses the whole manifest with `package_manifest_invalid`, the deploy as well
as the run, exactly as `deployabels:` does. What the block *means* -- a need
nobody offers, a bucket nobody declared, a timeout that is not a duration -- is
checked when a run compiles, and fails only that run, with a `pipeline_*` code:
a mistake of meaning never refuses a deploy of the source it sits in.

The meaning is checked at connect, against the default branch's head, and again
on every run, against the run's own commit, because a pull request can change
its own pipeline. A run reads the whole block, including stages it will not
execute -- a typo in the deploy stage fails the pull request that makes it, not
the push after it lands -- and reports the first problem in a fixed order: the
services, the `select` block, then each stage and its steps as written. What
depends on the pipeline's own settings, an allowed secret or consent to the
fleet, is asked only of the stages the run executes.

### Names

Every name the block declares -- a stage, a step, a service, a bucket, a
channel -- is lower-case letters, digits and hyphens, starts with a letter or a
digit, and is at most 40 characters. A name is part of a step's key and of a
refusal's scope, so it never holds a dot, a slash or a hash.

### The block's keys

| Key | Value | Default | Refused when |
|---|---|---|---|
| `image` | The toolchain image every command step runs in. Pin it by digest | none | -- |
| `services` | Named sidecars a step may ask for, each with `image` (required), `env` (plain `NAME: value` configuration, never a secret) and `ready` (a shell probe the runner waits on before the step's command starts) | none | A name that breaks the rule, a service with no image, or an `env` name that is not upper-case letters, digits and underscores starting with a letter or underscore: `pipeline_step_invalid`, scoped `services` or `services/<name>` |
| `caches` | Caches the runner mounts for every step, such as `go` and `npm` | none | -- |
| `select` | How steps choose what to run. Required once a step names `packages` | none | [Below](#select) |
| `stages` | The stages, in the order they run. At least one | required | Absent or empty: `pipeline_stage_invalid` |

`image` and `caches` are passed to the runner as written; what each cache name
mounts is the runner's ([Caches](pipelines-substrate.md#caches)): it knows
`go` and `npm`, and a step declaring any other fails `pipeline_job_rejected`.

### `select`

| Key | Value | Refused when |
|---|---|---|
| `go` | `import-graph`, the one Go selection there is | Any other value: `pipeline_select_invalid`. Absent while a step names `packages`: `pipeline_select_missing` |
| `dbGated` | Repository directories whose Go packages need a database, such as `component/memql`. `.` is the root package alone | An entry that is empty, absolute, holds `..` or a glob character, or names a directory starting with `.` or `_`: `pipeline_select_invalid` |
| `full` | Globs whose change selects every package, beside the built-in ones ([Which packages a step tests](#which-packages-a-step-tests)) | A glob that cannot be read, or a list of only `!` globs: `pipeline_select_invalid` |
| `buckets` | Named lists of globs a step gates on with `when: { bucket: <name> }` | A name that breaks the rule, an empty list, a glob that cannot be read, or a list of only `!` globs: `pipeline_select_invalid` |

### A stage's keys

| Key | Value | Refused when |
|---|---|---|
| `name` | The stage's name | Missing, breaking the rule, or used twice: `pipeline_stage_invalid` |
| `needs` | Earlier stages this one depends on. Recorded rather than scheduled on: stages run in the order written whatever it says | A stage that comes later, the stage itself, or no stage at all: `pipeline_stage_invalid` |
| `on` | The events (`pull_request`, `merge_group`, `push`, `release`) and modes (`affected`, `full`) the stage runs for. Absent means every run | Anything else: `pipeline_event_unknown` |
| `steps` | The stage's steps, which run at once | Both `steps` and `channel`, or neither: `pipeline_stage_invalid` |
| `channel` | Makes the stage a notify stage naming a channel. It carries no steps, and compiles to one step, `<stage>.notify` | A name that breaks the rule: `pipeline_stage_invalid` |

### A step's keys

| Key | Value | Default | Refused when |
|---|---|---|---|
| `name` | The step's name, unique in its stage | required | Missing, breaking the rule, or used twice in the stage: `pipeline_step_invalid` |
| `run` | A shell command, run in the image's working copy of the commit -- the same contract as a deployable's `build.command`. There is no step language | required | Blank: `pipeline_step_invalid` |
| `packages` | `affected` or `all`: select Go packages into `MEMQL_PACKAGES` | none: the step selects no packages | Another value: `pipeline_step_invalid`. No `select.go`: `pipeline_select_missing` |
| `only` | `db-gated` or `not-db-gated`: keep the selected packages under a `select.dbGated` tree, or the rest | none | Another value, or set without `packages`: `pipeline_step_invalid`. No `select.dbGated`: `pipeline_select_invalid` |
| `shards` | Split the packages over up to this many steps, at most 8. 0 and 1 mean one step | one step | Below 0 or above 8, or above 1 without `packages`: `pipeline_step_invalid` |
| `when` | `{ bucket: <name> }`: skip the step on a pull request that touches nothing in the bucket | always runs | A bucket `select.buckets` does not declare: `pipeline_bucket_unknown` |
| `needs` | `{ <need>: true }`, the need one of `display`, `docker`, `gpu`, `macos_tooling`, `user_files`: the step needs a fleet machine that offers it. A need set `false` is the same as none | runs on the cluster | A need outside the set: `pipeline_need_unknown`. Any need on a pipeline whose compute is `cluster`: `pipeline_fleet_not_consented` |
| `services` | Declared services the step runs beside | none | A name `services` does not declare: `pipeline_service_unknown` |
| `timeout` | A duration such as `20m` or `1h30m`, from 1 minute to 2 hours. A step still running at its timeout is stopped and fails `pipeline_step_timeout` | `20m` | Not a duration, under `1m` or over `2h`: `pipeline_step_invalid` |
| `artifacts` | Paths the runner saves as Library files owned by the pipeline's owner ([Artifacts](pipelines-substrate.md#artifacts)) | none | -- |
| `secrets` | Names of secrets the step's environment receives, each under its own name ([Secrets](#secrets)) | none | A name that is not upper-case letters, digits and underscores starting with a letter (at most 128 characters), or that begins `MEMQL_`: `pipeline_secret_invalid`. A name the pipeline does not allow: `pipeline_secret_not_allowed` |

Two keys share the word `needs`: a stage's names earlier stages, a step's names
what the machine running it must offer.

### Globs

A bucket's globs and `select.full`'s are anchored at the repository root:

- `**`, as a whole path segment, matches zero or more directories. `dsl/**`
  matches `dsl` itself and everything under it; `**/*.md` matches `README.md`
  and `docs/a/b.md`. `**` mixed into a segment is refused.
- `*` matches within one segment, and `?` matches one character within one
  segment. A dot is an ordinary character.
- `[`, `]`, `{`, `}`, `(`, `)`, `+`, `@` and `\` are refused rather than
  guessed at, and so is a `!` anywhere but first.

**A `!` glob means except.** A path is in the list when one of its plain globs
matches it and none of its `!` globs does, in any order:

```yaml
    buckets:
      os: ["clients/**", "!clients/**/*.md"]
```

is every change under `clients/` except its Markdown. A list of only `!` globs
includes nothing and is refused. This is not how `dorny/paths-filter` reads a
list -- it ORs the patterns, so its `!` widens a filter -- which matters when a
bucket is copied from a GitHub Actions workflow: its plain globs mean what they
meant, and its `!` globs now narrow it.

### What a step's command sees

Every runner exports the same environment to a step's command:

| Variable | Value |
|---|---|
| `MEMQL_RUN_ID` | The pipeline run's id |
| `MEMQL_WORK_RUN_ID` | The work run it compiled into |
| `MEMQL_STEP` | The step's key: `<stage>.<step>`, with `#<i>` for a shard |
| `MEMQL_REPOSITORY` | `owner/name` |
| `MEMQL_SHA` | The commit under test |
| `MEMQL_MODE` | `affected` or `full` |
| `MEMQL_EVENT` | `pull_request`, `merge_group`, `push` or `release` |
| `MEMQL_VERSION` | The release's tag on a release, the commit otherwise |
| `MEMQL_PACKAGES` | The step's Go import paths, separated by spaces. Empty for a step that selects none |
| `MEMQL_SHARD` | `<i>/<k>` on a shard, absent otherwise |

Each allowed secret is added under its own name. The platform's names win over
a secret of the same name, which the manifest refuses anyway.

---

## How a run is chosen

### Events and modes

| GitHub delivers | Opens | Mode |
|---|---|---|
| `pull_request` opened, synchronize or reopened | a run for the pull request's head | affected |
| `merge_group` checks_requested | a run for the merge group's head | full |
| `push` to the default branch | a run for the pushed commit | full |
| `release` published | a run for the commit the tag names | full |
| `check_run` or `check_suite` rerequested | a new attempt of the run it names | the original's mode and event |

*Affected* runs what the change touches; *full* runs everything once, on the
exact tree that lands or ships. A pull request can therefore pass and its merge
group fail: the merge group's full run is the gate.

Every other delivery opens nothing: a push to another branch (its pull request
is what runs), a tag push, a deleted branch, a pull request closed or edited, a
review, a ping, an installation event, and any delivery that did not come
through the GitHub App's installation ([Delivery](#delivery-webhook-or-poll)).
Each is ignored, never failed on. What a delivery is comes from the shape
of its signed body. The `X-GitHub-Event` header is not covered by the
signature, so it is a cross-check only, and a delivery whose header disagrees
with its body is ignored.

A re-requested check run re-runs the run that check run reports; a re-requested
check suite re-runs the newest run of the pipeline at that commit.

### The run key

A run is keyed on **(repository, SHA, mode, event)**, written
`owner/name@<sha>:<mode>:<event>`. A second sighting of a key opens nothing:
GitHub redelivering a webhook, or a webhook and the poll seeing the same head,
is one run and one check run.

The event is part of the key because a merge queue's run and the push that
lands its commit share a SHA and a mode. Keyed without the event, the push --
which carries the deploy and notify stages -- would fold into the queue's run
and never execute.

A re-run is not a second sighting: it opens the key's next attempt, a new run
row with a new check run, and leaves the original as it ended.

### Stages, in order

Stages run one at a time, in the order written, and the steps of a stage run at
once. Every step waits for every step of the stage before it, whatever `needs`
says. The first stage that fails stops the run: every later step is skipped
`pipeline_stage_blocked`, and the check run's table says *Not run: an earlier
stage failed*.

`on` names events, modes or both. `on: [push]` runs on the default branch's
pushes only; `on: [full]` on the merge queue, pushes and releases;
`on: [pull_request, merge_group]` before a change lands. A stage whose `on`
leaves a run out is absent from that run's plan: it is not a skipped stage, and
the check run does not list it.

### When a step is skipped

A skip is reserved for work that had nothing to do. It is never a refusal in
disguise, and it fails nothing.

- **`when: { bucket: <name> }`** narrows pull requests only. In an affected
  run the step is skipped `pipeline_not_affected` -- *No change under bucket
  os.* -- when the changed files are known, there is at least one, and none is
  in the bucket. In a full run it always runs, and so it does when the change
  cannot be read.
- **A packages step with nothing selected** is skipped `pipeline_not_affected`:
  *No affected Go packages.*, or *No db-gated packages are affected.* under
  `only: db-gated`.

### Which packages a step tests

`packages: affected`, in an affected run, reads the change and the repository's
Go import graph:

- The changed files are GitHub's compare of the pull request's base with its
  head.
- The graph is read from source at the run's commit. Every `go.mod` names a
  module, every directory of `.go` files is a package of the nearest module
  above it, and every import of a path under one of those modules is an edge --
  under any build tag, test files included, and whether or not a package still
  answers the path. That union is wider than any one build's graph, which is
  the safe direction. Imports of anything else, the standard library and
  dependencies, are not edges. Directories named `testdata` or `vendor`, or
  starting with `.` or `_`, are skipped, as the go tool skips them.
- A changed file selects the package that owns it -- its directory, or the
  nearest package above -- and every package importing one of those,
  transitively.
- A changed file in a directory that holds no package -- where a package was
  until the change deleted or moved it -- also selects every package still
  importing the path that directory would have (its module's path plus the
  directory below the module's root), and their importers, transitively. A
  change that deletes a package therefore tests the packages still importing
  it, which no longer build, rather than selecting nothing and skipping green.
- A change that touches no Go package, in no directory an import names, selects
  none.

It selects **every package** instead whenever the graph cannot say: the change
could not be read (the compare failed, or listed 300 files, where GitHub stops
listing), the change is empty, a `go.mod`, `go.sum`, `go.work` or `go.work.sum`
changed anywhere, `memql-package.yaml` changed, vendored code changed, a path
under `select.full` changed, or a Go file did not parse. Selecting too much
costs minutes; selecting too little merges a red build.

In a full run, `packages: affected` means every package. `packages: all` means
every package in every run.

`only: db-gated` then keeps the packages whose directory is a `select.dbGated`
tree or lies under one, on a `/` boundary (`component/database` holds
`component/database/x`, not `component/databasex`); `only: not-db-gated` keeps
the rest.

`shards: k` splits what is left over up to `k` steps, keyed `<stage>.<step>#1`
to `#k`, each with its slice in `MEMQL_PACKAGES` and `MEMQL_SHARD=<i>/<k>`. The
split balances on the pipeline's timing table, heaviest package first, each
into the lightest shard so far; a package the table has not measured weighs
five seconds. Packages that cannot be split -- one, or several measured at
zero -- run as one step under the step's own key. The table learns: after every
successful full run, each passing package's time from its step's `go test`
output (an `ok  <package>  <seconds>s` line; a cached result is not a
measurement) replaces that package's row.

---

## Refused, never run

### Pull requests from forks

A pull request whose head lives in another repository -- a fork, or a head
whose repository GitHub reports deleted -- is **refused, never queued**. The
cluster writes the run row `completed` with conclusion `refused` and code
`pipeline_fork_refused`, and completes the check run with conclusion
**failure**, titled *Refused: pull request from a fork*: a neutral conclusion
would satisfy a required check. No step runs and no secret is resolved. This is
deliberately stricter than GitHub Actions' approval for a first-time
contributor.

To run such a change, run it from a branch of the repository: a maintainer
pushes the contributor's commits to a branch here and opens a pull request from
it, and that pull request's run executes. The commits keep their SHA, so it is
the same run key as the refused run: a fork's refusal answers only a fork, and
the new pull request opens the key's next attempt, whose check run replaces the
failing one. Once a run of this repository's answers the key, a fork's pull
request at that commit is answered by it rather than refused beside it.

### A manifest that cannot compile

A run whose `memql-package.yaml` cannot be read (`package_manifest_invalid`),
has no `pipeline:` block (`pipeline_not_declared`), or does not validate or
compile (a `pipeline_*` code) **fails, typed, before any step runs**. It is
never skipped, because a skipped check reads as green. The run concludes
`failure` carrying the code, its sentence and its scope -- `<stage>/<step>`, a
stage, or a path into the block such as `select/buckets/os` -- and the check run
is titled *Refused: \<what\> (\<where\>)*, its summary carrying the sentence and
what to do. Fix the block and push: the next run reads the fixed manifest.

---

## Secrets

A manifest carries secret **names**, never values:

```yaml
        - name: verify-rollout
          run: ./scripts/verify-rollout.sh
          secrets: [VERIFY_TOKEN]
```

- The name is the cluster's `v1:platform:globalSecret` of that name, and the
  step's environment receives it under the same name.
- It resolves only when the pipeline allows it. `secretNames`, set when the
  pipeline is connected, is the owner's allowlist. A step naming a secret not
  on it refuses the whole run before any step runs
  (`pipeline_secret_not_allowed`), whenever that step's stage is part of the
  run.
- A name beginning `MEMQL_` never resolves: the manifest refuses it
  (`pipeline_secret_invalid`), connect refuses it in `secretNames`, and the
  platform's own variables win over it in any case.
- An allowed name with no value on the cluster fails its step
  `pipeline_secret_missing`.

Values exist only in the request handed to the runner for one step. They are
never written to a pipeline row, a run row, a work step, a log line or the
check run, and the log tails a check run shows have every secret value of four
bytes or more replaced with `***`.

**Allow only what anybody who can push a branch may see.** A pull request can
change its own pipeline, so a branch pushed to the repository can add a step
that reads an allowed secret. A fork cannot: its run is refused before anything
resolves.

## Compute

`compute` is set when the pipeline is connected:

- **`cluster`**, the default: every step runs on the cluster. A step naming a
  need fails the run's compile with `pipeline_fleet_not_consented`, scoped to
  the step -- nothing about somebody's laptop is a default.
- **`cluster_and_fleet`**: a step naming a need may run on a machine in the
  owner's fleet that offers it; every other step stays on the cluster. How the
  step reaches a machine, and what the machine must allow, is
  [the fleet](pipelines-substrate.md#the-fleet).

A need outside the closed set is refused (`pipeline_need_unknown`), never routed
by guesswork.

## Delivery: webhook or poll

`delivery` is set when the pipeline is connected:

- **`webhook`**: GitHub posts each delivery to the cluster's inbound seam,
  `https://api.<domain>/inbound/github`, signed with the app's webhook secret,
  and a shipped automation opens the runs it asks for. The runs are decided by
  the delivery the seam staged and verified, never by anything else: a
  `github` source configured to verify no signature opens no run. A delivery
  opens runs only for pipelines connected under the installation it came from,
  for the repository name they were connected with. **Reinstalling the GitHub
  App on the repository, or renaming the repository, therefore needs a
  reconnect** (the same `pipelinesConnect` call): until then its deliveries
  carry an installation or a name no pipeline matches, and open nothing. A
  delivery through no installation -- a repository webhook, which
  [Packages](packages.md#update-detection) documents for Deployables' update
  feed on the same seam -- opens nothing, and is ignored rather than failed
  on. Setting the webhook up: [GitHub Connect](github-connect.md#the-webhook)
  and [Inbound delivery](inbound-delivery.md).
- **`poll`**: every minute, one agent replica reads the default branch's head
  and the heads of the open pull requests -- the hundred most recently opened
  -- through the owner's connection. It opens a `push` run for a default-branch
  head it has not seen and a `pull_request` run for a new or moved pull request
  head, forks refused as above, and forgets closed pull requests. **The first
  poll records what it sees and opens nothing**, so connecting never floods a
  repository's history with runs; runs start with the next change.

A cluster GitHub cannot reach -- a local cluster above all -- polls. A polled
pipeline sees heads only: a merge group, a published release and a re-run
pressed on GitHub are only ever delivered, so they start nothing there, and
`pipelinesRerun` re-runs from the cluster instead. A pipeline set to poll still
takes a delivery that does arrive, and the run key makes the second sighting of
a head a no-op either way.

## Who drives a run

A run is opened where its trigger fires -- the bff for a webhook, an agent
replica for the poll -- as a `queued` row, and its check run is created
`queued` there. An agent replica claims it under a lease (`driverNodeId`,
renewed every 30 seconds) and drives it: it reads the tree at the commit,
compiles the plan, opens the work goal, hands each step to the runner, and
writes the check run as the run advances. If that replica stops, another agent
replica takes the run over once the lease is 120 seconds stale, and the
every-minute poll claims any queued run no agent picked up. **A cluster with no
agent replica opens runs and never starts them.**

A pipeline's work run belongs to its runner (`triggeredBy: pipeline:<mode>`):
the work dispatcher never executes one, the spine's waiting sweep leaves it
alone, and Nexus's re-run and branch refuse it. Re-run a pipeline run with
`pipelinesRerun`.

## The check run

Each run writes one check run on its commit, named **`MemQL / <name>`** --
`<name>` being the manifest's `name` when the pipeline was connected -- which is
the name a ruleset requires. It is `queued` when the run opens, `in_progress`
when an agent starts driving it, and `completed` with the run's conclusion. Its
`external_id` is the run's id, and its details link is the run's page in MemQL
OS, `https://os.<domain>/?pipelineRun=<run id>`.

| State | Title |
|---|---|
| Queued | *Queued* |
| Running | *Preparing the run* until the plan is read, then *Running: \<stage\>* |
| Passed | *Passed: \<n\> stages in \<time\>*, or *Passed: nothing to run* |
| Failed | *Failed at \<stage\>: \<step key\>*, ending *and \<n\> more* when several steps failed |
| Cancelled | *Cancelled* |
| Refused | *Refused: \<what\> (\<where\>)* |

The summary is one line saying which run, then the stage table, then the failed
steps, each with its message and code:

```text
Mode full · Push to the default branch · Commit 3f9c2ab

| Stage  | Status                           | Steps              | Time   |
| ---    | ---                              | ---                | ---    |
| checks | Passed                           | 1 passed           | 52s    |
| tests  | Failed                           | 8 passed, 1 failed | 7m 40s |
| deploy | Not run: an earlier stage failed | 1 not run          | -      |
| notify | Not run: an earlier stage failed | 1 not run          | -      |
```

A stage reads *Passed*, *Failed*, *Cancelled*, *Running*, *Waiting*, *Not run*,
*Not run: an earlier stage failed* or *Skipped: \<reason\>*; a stage's time is
its longest step's, and `-` is a time nobody measured. The check run's text is
the last lines of each failed step's output, with secret values masked. A
refused run shows the refusal and what to do instead of a table.

**A 403 never stops a run.** An installation that has not accepted the checks
permission answers 403 to every check-run write. The run opens, executes and
concludes as usual; it records `checkRunState: refused` and carries the note
`pipeline_check_permission_missing`. Accepting the wider permissions is the
whole repair -- [Upgrading an app registered before pipelines](github-connect.md#upgrading-an-app-registered-before-pipelines)
-- and the next run reports its check. A run that can write no check run at
all, with no app or no token, records `checkRunState: unavailable`.

**A final report that did not land is written again.** A run that concluded
while GitHub would not take its last check-run write -- GitHub down, the token
unobtainable -- records `checkRunState: unavailable`, and its check run still
shows it unfinished, which holds a merge on a required check. The every-minute
recovery on an agent replica republishes the final report of each such run
concluded in the last 24 hours, trying less often while it keeps failing, and
records `written` once it lands. The republished report carries the stage table
and each failed step's message and code, but not the log excerpt, which only
the agent that drove the run held.

## Readiness

Pipelines is an optional item in the cluster's
[readiness](configuration-readiness.md) list, reported by agent nodes. It is
there for the three things a pipeline's checks need: a GitHub App whose
installations hold checks write, a connected repository, and a runner to
execute steps. A repository counts as connected once a pipeline is.

Today the item reports configured once all three hold: this cluster has a
GitHub App, at least one pipeline is connected and active -- whoever owns it --
and a runner is registered on the agent node reporting it. Until then it says
which are missing. Every agent node registers a runner, so that fact cannot
tell whether a workbench replica can run steps, or whether the cluster passes
the substrate's isolation proof
([Known limitations](pipelines-substrate.md#known-limitations)). It cannot see whether an installation accepted checks write,
which is a fact per installation: a run that could not write its check run says
so itself ([The check run](#the-check-run)).
Nothing needs the item, so the first-run wizard does not walk it, and an owner
may answer *Not now*.

**Settings -> Pipelines** is the item, for the cluster owner. Its three
sub-steps are the report's three facts, as the agent nodes that run steps
report them: the GitHub App (registered from Settings through GitHub's
app-manifest flow, so the permissions arrive filled in), a repository
connected, and a runner -- with a line about how many of your machines allow
pipelines. While the item is neither set up nor dismissed, the Settings icon
carries the marker; *Not now* clears it for you and the section stays.

**An app whose permissions grew is named there.** Every existing installation
of the GitHub App keeps the permissions it accepted until the account that
installed it approves the change GitHub sends it, and until then every
check-run write there answers 403. Settings -> Pipelines asks GitHub
(`pipelinesInstallations`, `read app:settings/pipelines`) which installations
lag what the app asks for and links each to the page where the change waits --
before a run finds out by failing to report.

## What runs today

This release is the seam (epic memql#5477), MemQL OS's surfaces for it (epic
memql#5479) and the substrate that executes its steps (epic memql#5478):
deliveries, the poll, run keys, the plan, the work goal, the check run, the
Runs tab, run page, Checks and connect flow, and every command step run as a
Kubernetes Job on the cluster, or on one of the owner's machines for a step
naming a need, with its logs in the log store and its log and artifacts in the
owner's Library ([Pipelines substrate](pipelines-substrate.md)). One thing
waits for a later epic:

| Not yet | Arrives with | Until then |
|---|---|---|
| Channels and notify delivery | epic memql#5480 | A notify stage's step is skipped `pipeline_notify_unavailable`, which fails nothing |

**Do not make `MemQL / <name>` a required check until a run has passed on this
cluster.** A cluster has to be ready for steps before any runs: on one that does
not enforce network policy -- an AKS cluster as `azure-provision.sh` creates it
-- every step is refused `pipeline_isolation_unenforced`, and a step's images
must be pullable with no registry credential.
[Owner actions](pipelines-substrate.md#owner-actions) lists what to set up
first.

## Refusal codes

A **refusal** ends the run before any step runs; a **failure** fails one
step; a **skip** or a **note** changes no outcome. The scope a code carries
says where: `<stage>/<step>`, a stage, or a path into the block.

| Code | Kind | Meaning | What to do |
|---|---|---|---|
| `pipeline_not_declared` | refusal | `memql-package.yaml` has no `pipeline:` block | Add one, or disconnect the pipeline |
| `pipeline_stage_invalid` | refusal | No stages; or a stage missing a name, misnamed or repeated, needing itself, a later stage or no stage, with both or neither of `steps` and `channel`, or naming a misnamed channel | Correct the stage the scope names |
| `pipeline_step_invalid` | refusal | A step missing a name, misnamed or repeated in its stage, with no command, or with a bad `packages`, `only`, `shards` or `timeout`; or a service misnamed, with no image, or with a bad `env` name | Correct the step or service the scope names |
| `pipeline_select_invalid` | refusal | `select` names an unknown Go selection, a `dbGated` entry that is no directory, a misnamed or empty bucket, or a glob that cannot be read; or a step uses `only` with no `dbGated` | Correct the entry the scope names |
| `pipeline_select_missing` | refusal | A step names `packages`, and the block has no `select: { go: import-graph }` | Add it |
| `pipeline_event_unknown` | refusal | A stage's `on` names neither an event nor a mode | Use one the sentence lists |
| `pipeline_bucket_unknown` | refusal | `when.bucket` names a bucket `select.buckets` does not declare | Declare it, or correct the name |
| `pipeline_service_unknown` | refusal | A step names a service `services` does not declare | Declare it, or correct the name |
| `pipeline_need_unknown` | refusal | A step's `needs` names something outside `display`, `docker`, `gpu`, `macos_tooling`, `user_files` | Use one of them, or remove it |
| `pipeline_secret_invalid` | refusal | A secret name is not upper-case letters, digits and underscores, or begins `MEMQL_` | Rename it |
| `pipeline_secret_not_allowed` | refusal | A step uses a secret the pipeline does not allow | Reconnect with it in `secretNames`, or remove it from the step |
| `pipeline_fleet_not_consented` | refusal | A step names a need, and the pipeline's compute is `cluster` | Reconnect with `compute: cluster_and_fleet`, or remove the need |
| `pipeline_fork_refused` | refusal | The pull request's head is in another repository | Push the branch to this repository and open the pull request from it |
| `pipeline_disconnected` | refusal | The pipeline was disconnected before an agent started the run | Connect it again; the next change runs |
| `pipeline_already_connected` | refusal | Connect only: another source's pipeline already runs the repository, and a repository has one | Disconnect that pipeline -- its owner does, with `pipelinesDisconnect` -- or work from that source |
| `pipeline_nothing_to_rerun` | refusal | Re-run failed only: the run has no failed or cancelled step to run again | Re-run it whole |
| `pipeline_runner_unavailable` | failure | No runner could take the step: the cluster is not set up to run steps, or the part that would run it was unreachable | Set the cluster up as [Where a step runs](pipelines-substrate.md#where-a-step-runs) says, then re-run. Nothing in the repository is wrong |
| `pipeline_executor_error` | failure | The runner could not report how the step ended | Re-run; if it repeats, look at the runner |
| `pipeline_secret_missing` | failure | An allowed secret has no value on this cluster | Store a value under that name, then re-run |
| `pipeline_stage_blocked` | skip | An earlier stage failed | Fix that stage |
| `pipeline_not_affected` | skip | The change touched nothing the step covers | Nothing |
| `pipeline_notify_unavailable` | skip | Notify delivery arrives with epic memql#5480 | Nothing |
| `pipeline_passed_earlier` | skip | A failed-only re-run carried the step over: the attempt it re-ran passed it with the same packages | Nothing |
| `pipeline_check_permission_missing` | note | GitHub answered 403 to a check-run write; the run went ahead | Accept the app's wider permissions on GitHub |

The runner adds codes of its own -- a step past its timeout
(`pipeline_step_timeout`), an image that will not pull, a clone that fails, a
cluster that cannot prove its steps isolated -- listed with their remedies in
[the substrate's codes](pipelines-substrate.md#codes). Connecting refuses with
Deployables' codes as well as these
([Connecting a pipeline](#connecting-a-pipeline)).

---

## Related

- [Pipelines substrate](pipelines-substrate.md) -- where a step runs, what it
  may reach, its logs and artifacts, how it ends, and what an operator sets up
  first
- [GitHub Connect](github-connect.md) -- the app that delivers a pipeline's
  events and writes its check runs, and the upgrade an older app needs
- [Inbound delivery](inbound-delivery.md) -- the webhook seam and its signatures
- [Packages and deployables](packages.md) -- the source, the manifest, and the
  update feed that reads the same deliveries
- [Deployables](deployables.md) -- where a pipeline's source lives
- [Configuration readiness](configuration-readiness.md) -- what the Pipelines
  item's marks mean
