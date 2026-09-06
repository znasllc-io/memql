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
images, pushes images, edits the repo-root `VERSION` file, or goes near
`scripts/release/release.sh` -- whose `--push` path remains break-glass.

Design record:
[2026-08-23-release-cut-automation-design.md](../../superpowers/specs/2026-08-23-release-cut-automation-design.md).

---

## 1. What a cut actually does

Three things happen, in this order, and the order is load-bearing:

1. **The tag is created** at the current head of `main`. GitHub's ref-create is
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
   workflow builds every node type as a product-agnostic image.

MemQL does step 1 and 2. CI does step 3. The platform records what it did on a
`v1:cluster:releaseCut` row and writes a `release_cut` audit event beside it.

**It cannot tell you the images exist.** Publishing a Release means the build
was *asked for*. Whether it finished is a question only the container registry
answers, which is what **Check images** is for -- see section 6.

---

## 2. Before the button works: two values to seed

The engine is product-agnostic and carries **no repository default**. An
installation that wants this button says which repository it cuts, and supplies
a credential. Until both are seeded, the Releases card renders the missing step
instead of the button.

### `MEMQL_RELEASE_REPO` -- a global variable

The repository, in `owner/name` form. Not a URL, no `.git` suffix:

```
acme/widget
```

### `MEMQL_GITHUB_RELEASE_TOKEN` -- a global secret

A **fine-grained personal access token** (or a GitHub App installation token)
scoped to that one repository:

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

Seed both from the console's configuration surface, or as environment values on
the node. Resolution order is **global secret -> global variable -> environment**,
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

The card issues a dry run on load, and that is also how you validate a freshly
seeded credential by hand:

```
builtin releaseCut(bump: "patch", dryRun: true)
```

It computes the plan -- the next version and the base sha -- and **creates
nothing, publishes nothing, writes nothing**. It exercises the real credential
against the real API, so it is a genuine test of the setup rather than a
simulation of one.

A successful dry run tells you three things: the token works, the repository
name is right, and the version arithmetic found your existing tags.

---

## 5. Cutting

On a console's **Deployments** surface, the **Releases** card. It is visible only
to owners -- a non-owner sees no card at all, and the engine refuses the call
independently before any network request is made.

The card shows:

- **the newest existing TAG**, read from GitHub. Not the newest row: a release
  cut by hand creates a tag this cluster never hears about, so the tag is the
  truth for "newest" and the rows below are only what this installation did.
- **the form** -- a bump (patch, minor or major; major and minor zero the parts
  below them), optional notes, and the extension pin-bump checkbox.
- **the confirm phrase** `cut-a-release`, typed. A release is not undoable from
  here: reversing one means deleting the tag and the Release on GitHub by hand.

Notes are **prepended** to GitHub's generated release notes rather than
replacing them, so a sentence about why you cut sits above the generated list of
changes.

---

## 6. Afterwards: Check images

Each row carries a **Check images** button. It asks the container registry for
the manifests of a representative node-image set at the bare version, and gives
one of three answers:

| Answer | What it means |
|---|---|
| Every image is published | the row moves to `images_available` |
| Still building, cut *N* minutes ago | the row stays `dispatched`; the missing images are named |
| The check could not tell | the registry errored; **the status is unchanged and nothing is guessed** |

The third answer is the point of the design. A workflow can fail *after* the
Release publishes, so only the registry knows whether a version is deployable --
and a registry that cannot be reached knows nothing either way. Reporting that
as "not built" would call a good release broken; reporting it as "built" would
call a failed build deployable.

The check is on demand. There is no poller and no schedule.

---

## 7. What each refusal means

| Code | What happened | What to do |
|---|---|---|
| `release_repo_unconfigured` | no repository configured, or GitHub cannot see it | seed `MEMQL_RELEASE_REPO`; check the token's repository access |
| `credential_unavailable` | no token, or GitHub rejected it (401/403) | seed or re-mint `MEMQL_GITHUB_RELEASE_TOKEN` with Contents: read/write |
| `github_unreachable` | transport failure or a 5xx. **Nothing was created** | retry; check GitHub's status |
| `ref_exists` | the computed tag already exists | someone else cut it, or it was cut by hand. Re-read the card and cut again if you still need to |
| `already_released_at_head` | `main`'s head already carries a release tag | land a change first. Cutting again would publish a second version of identical code |
| `no_release_tags` | the repository has no `vX.Y.Z` tag at all | create the first tag and Release by hand. The first version of a repository is one a human chooses; the button takes over after that |
| `tag_created_release_failed` | **half done** -- see below | act; nothing is building |
| `version_not_cut` | Check images was asked about a version with no row here | it was cut by hand or on another installation. There is no row to move |
| `registry_check_failed` | the image check itself errored | the status is unchanged. Retry later |
| `not_owner` | the caller does not hold the owner role | you will not meet this from a console -- the card is absent for a non-owner rather than refusing. It is what a direct SDK or MCP caller gets |
| `invalid_bump` | the bump was not major/minor/patch | same: a console only offers the three, so this is a direct caller's typo |

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

With the checkbox ticked, a successful cut also opens a pull request bumping
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
- **No touching `VERSION`, `release.sh`, or the dispatch workflows.**
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

The console button above is one path. `releaseEngine`
(`scripts/release/release-engine.sh`, capability `release.engine`) is the
other: the deploy pack's action, for a lifecycle that cuts a version as a step
rather than a person pressing a button.

It exists because of a failure the button cannot have and a script can:

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

Three behaviours worth knowing before using it:

| Situation | What happens |
|---|---|
| the release already exists, published | idempotent; it still waits for the build |
| the release exists as a **draft** | **refused**. A draft emits no `release: [published]` event, so it builds no images while looking like a release in the UI |
| the build ran and **failed** | reported (`buildRunConclusion`), not swallowed. The bridge fired, which is what this checks; a failed build is a different problem with a different fix |

```bash
scripts/release/release-engine.sh --version=0.19.10 --dryRun=true   # verify credential + state, create nothing
scripts/release/release-engine.sh --version=0.19.10
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

Read the history from the console card, or with `query releaseCuts()` as an
owner.
