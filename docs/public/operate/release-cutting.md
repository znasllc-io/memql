---
title: Cutting a release
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: znas
---

# Cutting a release

Only owners may cut a new version of MemQL, and the platform does it end to
end: it computes the next version, creates the tag, and publishes the GitHub
Release that starts the image build for every node type.

This is the manual half of an otherwise automatic pipeline. Nothing here builds
images, pushes images, or goes near `scripts/release/release.sh` -- whose
`--push` path remains break-glass.

The repo-root `VERSION` file is **read, never written**. `VERSION` equals the
tag of the commit a cut tags (`VERSIONING.md`), so a pull request setting it
lands on `main` first and the cut then refuses with `version_file_stale` if the
file names anything but the version it computes. Section 5 has the order.

Design record:
[2026-08-23-release-cut-automation-design.md](../../superpowers/specs/2026-08-23-release-cut-automation-design.md).

---

## 1. What a cut actually does

Three things happen, in this order, and the order is load-bearing. Before any
of them, the cut reads `VERSION` at `main`'s head -- the exact commit it is
about to tag -- and refuses unless the file reads the version it computed.

1. **The tag is created** at the reviewed head of `main`. Publication requires
   the repository, commit and version from a reviewed dry run, and refuses
   if the current plan differs. GitHub's ref-create is
   atomic, which makes this the concurrency gate for the whole feature: two
   owners cutting at the same moment produce one tag and one `ref_exists`
   refusal. There is no lock anywhere because none is needed.
2. **A GitHub Release is published** for that tag -- published, never a draft.
   Only `release: published` fires the cascade, so a draft would create a
   Release that builds nothing while looking exactly like success.
3. **The cascade runs.** `.github/workflows/dispatch-engine-images-on-release.yml`
   (#2519) fires on the published Release and re-dispatches
   `build-engine-images.yml` with the **bare** version -- it strips the leading
   `v`, because git tags carry it and image tags do not (memql#4061). That
   workflow builds every node type as a product-agnostic image. The bridge also
   forwards the release event's exact commit as `source_sha`. The build checks
   out that commit and verifies its tag and `VERSION` before registry login;
   advancing `main` after publication cannot change the release's source.

For a manual image-build dispatch, select `main` for the workflow and supply
both the bare `version` and the full 40-character `source_sha` named by
`v<version>`. The workflow stays on `main` for Azure OIDC; its source checkout
and the commit stamped into every binary are the verified release commit.

MemQL does step 1 and 2. CI does step 3. The platform records what it did on a
`v1:cluster:releaseCut` row and writes a `release_cut` audit event beside it.

**It cannot tell you the images exist.** Publishing a Release means the build
was *asked for*. Whether it finished is a question only the container registry
answers, which is what `releaseCutStatus` is for -- see section 6.

---

## 2. Configure the repository and release access

The engine is product-agnostic and carries **no repository default**. An
installation that cuts releases says which repository it cuts, and supplies a
credential. Release access can use the cluster’s GitHub App or an explicit
release token. Missing repository configuration or access refuses the call with
`release_repo_unconfigured` or `credential_unavailable`.

### `MEMQL_RELEASE_REPO` -- a global variable

The repository, in `owner/name` form. Not a URL, no `.git` suffix:

```
acme/widget
```

### Use the cluster’s GitHub App

Install the configured App on the release repository and approve **Contents:
read and write**. With no explicit `MEMQL_GITHUB_RELEASE_TOKEN`, each owner call
mints a fresh installation token restricted to that repository and that one
permission. The token is held only for the call; it is never stored in a row or
sent to a worker. App configuration follows the same environment/cluster-row
resolution as the GitHub connection, so a registration change takes effect
without restarting the engine.

This path does not request Pull requests permission. The optional extension
pin-bump therefore remains a follow-up note if GitHub refuses it. Owner access
and exact-candidate checks still apply; App installation alone authorizes no
release call.

### `MEMQL_GITHUB_RELEASE_TOKEN` -- an optional global secret

A **fine-grained personal access token** (or a GitHub App installation token)
scoped to that one repository. When configured, this credential takes
precedence over the App; a refused explicit credential does not fall back to
another identity:

| Permission | Level | Needed for |
|---|---|---|
| Contents | Read and write | creating the tag and publishing the Release |
| Pull requests | Read and write | **only** the optional extension pin-bump PR |

Mint it at **Settings -> Developer settings -> Personal access tokens ->
Fine-grained tokens**, with *Repository access* limited to the single
repository. Give it the shortest expiry your release cadence tolerates.

The Pull requests scope is genuinely optional. A token holding Contents alone
is a correct token: a cut works, and the pin-bump follow-on records a note
saying it could not open a PR rather than failing the release.

Both are rows in `scripts/secrets/manifest.yaml`, so the secrets seeder writes
them (`go run ./scripts/secrets seed --env-file <path>`, see
[adding a global secret](env-vars.md#adding-a-global-secret)); the
`setGlobalSecret` / `setGlobalVariable` mutations write them directly, and
environment values on the node work too. Resolution order is
**global secret -> global variable -> environment**,
secret first because the token is a credential; the environment tier exists for
the bootstrap window after `make up-refresh`, when concept storage is empty.

---

## 3. Recommended: protect the `v*` tags

A leaked `MEMQL_GITHUB_RELEASE_TOKEN` can tag and publish in the one repository
it is scoped to. That is bounded by the fine-grained scope, by encryption at
rest, by owner-only reach, and by the audit row -- and you can bound it further
on GitHub's side.

Add a **tag protection rule** for `v*` (Settings -> Rules -> Rulesets, targeting
tags), restricting tag creation to the identities that should be cutting
releases. A token that leaks then cannot create a release tag at all.

Do this before you seed the token, not after.

---

## 4. First run: a dry run

A dry run is how you validate a freshly seeded credential, and how you see the
plan before cutting for real:

```
builtin releaseCut(bump: "patch", dryRun: true)
```

It computes the plan -- the next version and the base sha -- without creating
a tag, release or audit row. The App path mints a short-lived access token for
these repository reads. It exercises the real credential
against the real API, so it is a genuine test of the setup rather than a
simulation of one.

A successful dry run tells you four things: the token can read the
repository, the repository name is right, the version arithmetic found your
existing tags, and `VERSION` at `main`'s head already reads the version the cut
would create.

A dry run stops before the first write. It runs every check that comes before
the tag is created and none of the ones that come with the writes, so it cannot
prove the token may create tags or Releases: a token with only Contents: read
passes a dry run, and the real cut then refuses with `credential_unavailable`
when GitHub rejects the tag. `ref_exists` (somebody cut the same version in
between), a `github_unreachable` on a write and `tag_created_release_failed`
also appear only on a real cut.

**Between cuts, the dry run answers `version_file_stale`, and that is
expected.** `VERSION` reads the release `main` was last cut at until the
prepare pull request for the next one merges (section 5), so until then the
call above is refused with `version_file_stale`. The refusal comes after the token has listed the tags,
read `main`'s head and read `VERSION`, and its message names the version the
cut computed and the sha it would tag. So it still proves the credential can
read the repository, that the repository name is right, and that the
arithmetic found your tags. What it cannot tell you yet is whether the cut
would go through; that waits for the prepare pull request.

---

## 5. Cutting

### First: set `VERSION` by pull request

`VERSION` equals the tag of the commit a cut tags, and `main` refuses direct
pushes, so the value arrives the way every other change does:

1. Open a pull request whose one change sets `VERSION` to the version you are
   about to cut -- `0.24.0`, bare, no leading `v`. A minor release also records
   in `releasedForms` (`deprecation_window_version_test.go`) every deprecated
   form whose warning it is the first to carry; that test says so when it
   applies.
2. Merge it.
3. Run the dry run with the bump that reaches that version. Review its
   `repository`, `baseSha` and `version`, then carry those exact values into
   the publication call. A change to `main` after review refuses publication
   instead of tagging an unreviewed commit.

A cut whose computed version differs from `VERSION` is refused with
`version_file_stale` before anything is created, and so is its dry run. The
refusal names both values. It is what stops the lag that shipped docs bundle
0.21.25 labelled as engine 0.15.0: `VERSION` read `0.15.0` at every tag from
v0.16.1 to v0.21.25.

### Then: the cut

There is **no UI control** for a cut today. The browser card that drove it was
retired in epic memql#4984, and neither MemQL OS nor the VS Code extension
replaced it. A cut is the `releaseCut` builtin, called by an owner:

```
builtin releaseCut(
  bump: "patch",
  expectedRepository: "acme/widget",
  expectedSha: "<baseSha from the reviewed dry run>",
  expectedVersion: "v0.24.1",
  notes: "Why this cut",
  bumpExtensionPin: true
)
```

The TS `QueryClient.releaseCut` and Go `client.ReleaseCut` accept the same
arguments. The embedded `cluster.publishEngineRelease` automation template
provides a manual DSL entry point carrying the three reviewed values. It has
no event or schedule trigger. Instances configure credentials and repository;
the orchestration stays in the engine's core DSL.

Manual automation runs preserve the current caller's resolved role, including
a badge's role ceiling and expiry, on both the receiving node and a selected
remote node. Running an automation requires owner or admin; `releaseCut` still
requires owner. Supplying event payload fields cannot grant either role.
The template permits omitted notes, just like the builtin.

The engine refuses a non-owner in Go before any network request is made.

The arguments:

- **`bump`** -- patch, minor or major; major and minor zero the parts below
  them. The next version is computed from the repository's newest `vX.Y.Z`
  TAG, not from any row here: a release cut by hand creates a tag this cluster
  never hears about, so the tag is the truth for "newest" and the rows are only
  what this installation did.
- **`notes`** -- optional. Notes are **prepended** to GitHub's generated release
  notes rather than replacing them, so a sentence about why you cut sits above
  the generated list of changes.
- **`bumpExtensionPin`** -- also open the pin-bump pull request (section 8).
- **`dryRun`** -- compute the plan and create nothing (section 4).
- **`expectedRepository`, `expectedSha`, `expectedVersion`** -- required when
  publishing. Copy `repository`, `baseSha` and the tagged `version` from the
  reviewed dry run. Omission refuses with `release_candidate_required`; a
  changed candidate refuses with `release_candidate_changed`. Values travel
  with the call, so review and publication may reach different replicas.

The call asks for no confirmation, and a release is not undoable from here:
reversing one means deleting the tag and the Release on GitHub by hand. Run the
dry run first and read its plan.

---

## 6. Afterwards: check the images

`releaseCutStatus` checks one cut version:

```
builtin releaseCutStatus(version: "v0.19.10")
```

(`releaseCutStatus({ version })` on the TS `QueryClient`, `ReleaseCutStatus` in
`sdk/go/client`). It asks the container registry for the manifests of a
complete engine node-image set at the bare version, and gives one of three
answers:

| Answer | What it means |
|---|---|
| Every image is published | the row moves to `images_available` |
| Still building, cut *N* minutes ago | the row is `dispatched`; the missing images are named, and an earlier availability claim is corrected |
| The check could not tell | the registry errored; **the status is unchanged and nothing is guessed** |

The third answer is the point of the design. A workflow can fail *after* the
Release publishes, so only the registry knows whether a version is deployable --
and a registry that cannot be reached knows nothing either way. Reporting that
as "not built" would call a good release broken; reporting it as "built" would
call a failed build deployable.

The check is on demand. There is no poller and no schedule.

Every engine role must be present, including edge and workbench: other images
can publish even when either of those builds fails. This checks manifest
availability in the public registry. Before deployment, verify the exact
digests, source revision and target architecture in the registry the instance
actually pulls from.

---

## 7. What each refusal means

| Code | What happened | What to do |
|---|---|---|
| `release_candidate_required` | a publication call omitted part of the reviewed candidate; nothing was created | review a dry run and supply its repository, commit and tagged version |
| `release_candidate_changed` | the current plan differs from the reviewed candidate; nothing was created | review a fresh dry run before publishing |
| `release_repo_unconfigured` | no repository configured, or GitHub cannot see it | seed `MEMQL_RELEASE_REPO`; check the token's repository access |
| `credential_unavailable` | no release access, or GitHub rejected it | approve the App’s Contents: read/write access on the release repository, or configure `MEMQL_GITHUB_RELEASE_TOKEN` |
| `github_unreachable` | transport failure or a 5xx. **Nothing was created** | retry; check GitHub's status |
| `ref_exists` | the computed tag already exists | someone else cut it, or it was cut by hand. Run the dry run again and cut if you still need to |
| `already_released_at_head` | `main`'s head already carries a release tag | land a change first. Cutting again would publish a second version of identical code |
| `version_file_stale` | `VERSION` at `main`'s head is missing, or does not read the version this cut would create. **Nothing was created** | the message says which case it is. `VERSION` still names a release that is already cut: the prepare pull request has not merged, so merge one setting `VERSION` to the version you mean to cut (section 5) and cut again -- and if you were only validating a credential, this is the expected answer (section 4). `VERSION` names a newer release that a different bump reaches: cut with that bump. Anything else: fix `VERSION` by pull request |
| `no_release_tags` | the repository has no `vX.Y.Z` tag at all | create the first tag and Release by hand. The first version of a repository is one a human chooses; `releaseCut` takes over after that |
| `tag_created_release_failed` | **half done** -- see below | act; nothing is building |
| `version_not_cut` | `releaseCutStatus` was asked about a version with no row here | it was cut by hand or on another installation. There is no row to move |
| `registry_check_failed` | the image check itself errored | the status is unchanged. Retry later |
| `not_owner` | the caller does not hold the owner role | ask a cluster owner. The check runs before any network request, so nothing was created |
| `invalid_bump` | the bump was not major/minor/patch | pass one of the three |

### The half-done state

`tag_created_release_failed` means the tag exists on the repository and the
Release does not. The cascade never fired, so **nothing is building**, and the
tag is invisible to everyone except this cluster's history.

Two ways out, both by hand on GitHub:

- **publish a Release** for that tag, which starts the build; or
- **delete the tag**, which undoes the cut.

Until you do one of them, the row keeps saying so and the next cut of the same
bump will refuse with `ref_exists` naming that tag.

---

## 8. The extension pin-bump follow-on

With `bumpExtensionPin: true`, a successful cut also opens a pull request bumping
`editors/vscode/src/install/stackPin.ts`'s `DEFAULT_STACK_TAG` to the new tag --
the release an install checks out when it is not told otherwise.

A **pull request and never a push**: `main` refuses direct pushes (a repository
ruleset, not a convention), so the change goes through review and the merge
queue like any other. That is the right shape for a value this consequential,
not a workaround.

**It can never fail the cut.** By the time it runs the Release is published and
the build is going. If the token lacks the Pull requests scope, or the branch
exists, or the constant has moved, the reason is recorded as a note on the
release row and the cut still reports success -- because reporting a shipped
release as failed would invite you to cut again, producing a second version of
the same code.

---

## 9. What this deliberately does not do

- **No scheduled cuts.** The construct exists for something to call; no
  automation is seeded and none should be. An unattended release with nobody
  watching is not wanted.
- **No image building or pushing.** The CI cascade owns that.
- **No writing `VERSION`, and no touching `release.sh` or the dispatch
  workflows.** `VERSION` is read and checked; the value itself reaches `main`
  through a reviewed pull request, because `main` takes no push.
- **No cutting other repositories.** The bundle and client repos have their own
  dispatch paths; the repository variable is singular on purpose.
- **No workflow-run mapping.** The Actions API does not expose a run's dispatch
  inputs, so matching a run to a version is a guess. The registry is the truth
  an operator actually needs.

  This is still true, and the `releaseEngine` capability below does not
  contradict it. That capability asserts something strictly weaker and
  checkable -- *a build was dispatched after this release was published* -- which
  is what a bridge firing looks like and what its **not** firing does not. It
  never claims the run it finds is building that exact version.

---

## 10. The same cut from a lifecycle automation

The `releaseCut` builtin above is one path. `releaseEngine`
(`scripts/release/release-engine.sh`, capability `release.engine`) is the
other: the deploy pack's action, for a lifecycle that cuts a version as a step
rather than a person making the call.

It exists because of a failure the builtin cannot have and a script can:

> A pushed git tag builds **nothing**. `build-engine-images` is
> `workflow_dispatch`-only and its single automatic trigger is a
> `release: [published]` event.

Thirteen tags between `v0.16.0` and `v0.19.7` carry no release, so every 0.19.x
image came from a dispatch somebody remembered to run. A tag nobody dispatched
for is a version that looks cut and cannot be deployed.

So the capability publishes a **release**, then waits for a build-engine-images
run that postdates it, and **fails when none appears**. That second step is the
point: the bridge is an event handler, and one that silently does not fire is
indistinguishable from one that has not fired yet -- until the version is
deployed and every pod lands in `ImagePullBackOff`.

Five behaviours worth knowing before using it:

| Situation | What happens |
|---|---|
| the release already exists, published | idempotent; it still waits for the build |
| the release exists as a **draft** | **refused**. A draft emits no `release: [published]` event, so it builds no images while looking like a release in the UI |
| `VERSION` at the commit to be tagged is missing or names another version | **refused** (exit 3, `result.reason` `version_file_stale`), dry run included, before anything is created -- the same rule as `version_file_stale` above. The commit checked is the existing tag, else `targetSha`, else the default branch's head read once and passed as `--target`, so the commit checked is the commit tagged. For a tag not yet created, a prepare pull request setting `VERSION` resolves it. For a tag that **already exists** it cannot: no merge moves a tag. Delete the tag and re-run at a commit whose `VERSION` reads the version (passed as `targetSha`, or the default branch's head once a prepare pull request has merged). A tag cut before `VERSION` had to equal the tag -- the unreleased `v0.16.0` to `v0.19.7` tags among them -- cannot pass where it stands |
| `version` carries a pre-release or build suffix | **refused** as a bad parameter (exit 2). `VERSION` is never suffixed and must equal the tag, so a suffixed release could pass only by breaking that rule; the console cut and the docs bundle accept bare `X.Y.Z` alone too |
| the build ran and **failed** | reported (`buildRunConclusion`), not swallowed. The bridge fired, which is what this checks; a failed build is a different problem with a different fix |

```bash
scripts/release/release-engine.sh --version=0.24.0 --dryRun=true   # verify credential + state, create nothing
scripts/release/release-engine.sh --version=0.24.0
```

The result reports the **GHCR** prefix as well as the tag. That half of the
build is public and tenant-independent, and it is what made a full cloud
bring-up possible without ever authenticating to the retired subscription -- an
instance lifecycle should pull from there by digest and `az acr import` into its
own registry, rather than assuming access to whichever ACR the build workflow
targets.

---

## 11. Where the records live

- `v1:cluster:releaseCut` -- append-only, one timeline per version, cluster-owner
  tier. The row id **is** the version.
- `v1:identity:auditEvent` with `action=release_cut` -- who cut what, from which
  sha. The decisions log ([the split](auth/access-model.md)), not the
  high-volume activity stream.

Read the history with `query releaseCuts()` as an owner.
