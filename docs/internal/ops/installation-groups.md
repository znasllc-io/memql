---
title: Coordinated installation applications
audience: internal
status: draft
area: ops
sinceVersion: 0.30.10
owner: znas
---

# Coordinated installation applications

The receiver can coordinate existing Argo Applications without transferring
resource ownership between them. One selected release, native preparation,
installation head and completion receipt cover the complete group. Controller
success for one member is not installation completion.

## Receiver configuration

`installation.json` format 2 replaces `application` with `applications`, a list
of one to eight `{key, namespace, name}` objects. Keys are unique and at most
48 characters. Applications share one Argo namespace, Git repository,
destination namespace, immutable baseline commit and selected target commit.
The receiving API discovers and binds every actual Application UID, complete
spec, project, rendering configuration and destination. The selected-release
API continues to accept one `expectedCurrentRevision` for this coherent group.
A partially applied group reports each current revision separately and leaves
the shared current revision empty.

Each `imageBindings` entry must name its `application` key. Resource identities
are unique across the combined render. A duplicate resource is refused rather
than assigned to whichever Application was rendered first. Existing format 1
single-Application configuration and stored records retain their original
canonical identity.

Optional `sourceRepositories` entries contain a canonical HTTPS GitHub `.git`
repository and exact allowed directory paths. Dependencies must use full Git
commit hashes. Root and dependency archives are captured independently. Each
dependency collector uses anonymous access, with no product repository token;
a private dependency therefore cannot pass collection. Only an assembled,
bounded native closure can be acknowledged and rendered as a root source.
Source acquisition and every retained dependency remain part of the
preparation's cleanup journal.

A private product root remains supported. Rendering mints a fresh GitHub App
token limited to that root repository and `contents: read`, through the same
native token capability used for its checkout. DSL arguments cannot supply
credentials, installation IDs or replacement repositories. The qualified
Argo CD 2.13.3 renderer inherits the root credential helper for Kustomize child
fetches; this is **not** a promise that the token is never reused during a
dependency request. See Argo's [private remote base contract](https://argo-cd.readthedocs.io/en/release-2.13/user-guide/kustomize/#private-remote-bases)
and its pinned [Kustomize wrapper](https://github.com/argoproj/argo-cd/blob/v2.13.3/util/kustomize/kustomize.go).

The authority boundary is GitHub's repository-scoped, read-only grant, combined
with complete source verification before render: every remote URL must use
`https://github.com`, an operator-allowed repository and directory, an exact
commit and disabled submodules. Other transports, unqualified file reads and
executable plugins refuse. The renderer profile also excludes injected Git
configuration, helper and proxy environment variables. GitHub is the trusted
credential recipient; dependency maintainers cannot receive the credential or
expand its repository permissions through the admitted source inputs. Git can
follow provider redirects, so this does not establish a generic redirect or
credential-isolation policy for arbitrary Git hosts. Another host, renderer
profile or private dependency requires separate qualification.

Native graph and protocol regressions verify anonymous dependency capture,
complete proof before token minting, rejection of caller credential arguments,
and root-only token selection. The app token regression verifies the exact
repository and read permission sent to the mint capability. These fixtures do
not prove real GitHub authentication or redirect transport; the credential
inheritance description above comes from the pinned Argo implementation.

The native image, compatibility, protected-resource, storage and serving-health
requirements still apply. This configuration does not create a catalog,
credentials, renderer, registry permission or GitHub write authority.

## Recipes and recovery

Installation-owned recipes enumerate `installationApplications()` and invoke
source, apply and observe operations with `application: item.key`. The existing
role-only forms remain valid for format 1. Source preparation composes root
capture, source requirements, dependency capture/verify/acknowledge, closed-graph
verification, root acknowledgement and render in that order. The bundled
[preparation recipe](../../../dsl/installation/automations.memql) is the concrete
reference; an installation may select its own installed recipe.

Preparation captures complete revision, rollback and completion source closures.
Replacing the engine does not switch an active selection to a newly installed
recipe. Every effect still rechecks the current original operator, complete
receiving configuration, exact targets, retained sources, signed publications,
immutable image bytes and preservation evidence. PostgreSQL serializes claims
on all Application identities, including single-Application preparations, so a
second installation ID cannot claim a target already in use.

Argo effects have a durable per-Application start before dispatch. A timeout or
process exit retains the attempt and its target claims. Starting the same
selection resumes the saved intent; observing it cannot apply a missing one.
The native passes are bounded, so an installation's DSL can schedule subsequent
passes under its authorized operator context. A timer does not grant authority.

## Rollback

`Preparer.RollbackSelected` accepts only the installation and exact selected plan
IDs. The original operator may also restore the latest successfully completed
release while the installation head has not advanced. Admission reclaims its
head and all Application identities atomically. Any later preparation or plan,
even a cancelled preparation, refuses that historical rollback. Another
installation holding a target also refuses it. The original forward completion
receipt and timestamp remain unchanged; reversal has a separate completion
receipt. The status projection reports whether this request is currently
eligible, but every effect still requires fresh native verification. It requires the original current operator. It records a reversal before
any reverse write and permanently fences further forward effects. Reversals
are derived from exact owned terminal controller operations; pending or foreign
operations refuse.

A process can stop after recording a member start but before sending its Argo
write. The receiver does not infer that the write will never arrive. Under a
shared database effect lock it records a baseline sync intent with the original
Application CAS. Applying that intent fences any delayed forward request. If
the original forward request wins first, only its exact owned terminal state
can admit one separately journaled reversal; the original baseline CAS is then
stale and cannot land. Neither intent is rebound after an uncertain response.

Rollback refuses if the authenticated original session or unchanged migration
digest cannot be proved. It never attempts a database downgrade. Observation
records `rolled_back` only after the exact reverse operations converge and the
baseline workloads, immutable runtime images, original identity, migrations,
storage and protected resources all pass fresh checks. That terminal receipt
releases the group's claims; controller health alone does not.

## Local qualification

Run against a dedicated disposable database with the required extensions, never
an active installation's database. Each worker needs its own database. For
example, after setting `MEMQL_DATABASE_DSN` to that worker's disposable database:

```bash
MEMQL_REQUIRE_DB=1 GOMAXPROCS=2 go test -p 1 -count=1 \
  ./integrations/installation \
  -run 'TestInstallationGroup|TestPreparationWorkflowProtocolRecovery|TestRollbackJournal|TestSelectionEntrypoints'
```

The group protocol regression uses two real PostgreSQL connections, real native
archive/render/publication/image verification and protocol fixtures for external
systems. It covers replacement engine recovery, competing claims, lost writes,
partial group starts, delayed request fencing, migration refusal and terminal
rollback. It does not prove an installed Argo deployment, production catalog
availability or actual serving continuity. Run the separate
[continuity rehearsal](installation-continuity-rehearsal.md) for that evidence.
