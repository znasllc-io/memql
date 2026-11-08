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

The serving node retains its existing private Library client during transport
startup and binds the native receiver only after the cluster phase installs its
authenticated Workbench router. The transport phase precedes that router; binding
there leaves installation selection unavailable even when Workbench replicas are
healthy. Missing storage, projected cluster identity or the remote router still
refuses receiver setup, with no local execution fallback. The startup ordering
regression is `TestTheClusterPhaseWiresInstallationAfterWorkbench`; receiver
admission checks remain in `TestInstallationPreparerRefusesMissingNativeConfigurationAndIdentity`.

`installation.json` format 2 replaces `application` with `applications`, a list
of one to eight `{key, namespace, name}` objects. Keys are unique and at most
48 characters. Applications share one Argo namespace, Git repository,
destination namespace, immutable baseline commit and selected target commit.
The receiving API discovers and binds every actual Application UID, complete
spec, project, rendering configuration and destination. The selected-release
API continues to accept one `expectedCurrentRevision` for this coherent group.
A partially applied group reports each current revision separately and leaves
the shared current revision empty.

Discovery inventories bind the full resource contracts, with only the top-level
`APIResourceList.resources` order normalized. On the installed Kubernetes cluster,
Argo, CNPG and Cilium discovery returned identical entries in different orders;
comparing their raw arrays refused preparation as changed discovery. The same
canonical inventory feeds observation and recovery digests without modifying the
captured response. Added, removed or duplicated resources, changed scopes, verbs,
kinds, versions and extra fields still invalidate observation. Other arrays and
the existing object UID/resourceVersion and Secret rotation fences remain exact.
`TestReceiverDiscoveryResourceOrderIsStableAcrossReadsAndRecovery` and
`TestReceiverDiscoveryStillRefusesResourceContractChanges` cover that boundary.
For an explicit live read-only check, set
`MEMQL_INSTALLATION_DISCOVERY_TEST_KUBECONFIG` to an absolute operator kubeconfig
path and run `TestReceivingDiscoveryThroughInstalledKubernetes`. It reads only
API discovery, never namespaced objects or credentials, and does not qualify
installation application or completion.

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

### Catalog and continuity credentials

A personal access token belongs to the Identity HTTP surface. The BFF's gRPC
verifier intentionally rejects PATs, and Identity's general gRPC surface is
operator-only. Pointing a PAT at either general query endpoint cannot qualify an
installation. Do not widen those interceptors, borrow an operator identity or
store an expiring interactive access token to make a rehearsal pass.

For a retained PAT, configure both `catalog.endpoint` and `health.endpoint` as
`https://identity.<installation-domain>/release/installation-read`. This exact
HTTPS path selects the bounded Identity reader; an origin with no path retains
the existing JWT/SDK gRPC transport. There is no transport or identity fallback
after a refusal. Custom certificate roots remain explicit receiver inputs.

The Identity handler freshly verifies the PAT and its active owning user on
every request. It replaces the ambient bootstrap actor with that verified user
and credential, then applies the developer/admin/owner gate. Its closed request
schema permits only signed published-catalog get/list, native build/migration
health and the authenticated user's own row. It accepts no arbitrary query,
principal, credential operation, publication or installation write. It mints no
token and adds no Kubernetes or object-store authority to Identity. The existing
PAT activity/last-use accounting still runs during authentication.

The in-process result adapter must handle both the shaped `currentUser` query
and the typed node sets returned by the release builtins. A builtin's result has
no graph bundle; treating that absence as missing evidence makes authenticated
catalog and health reads return 503. Decode each builtin payload as an object,
retain the handler's exactly-one-row check, and refuse malformed payloads. Do
not replace this boundary test with an executor that already returns flat rows.

Catalog signatures and exact candidate/digest checks are shared with the SDK
reader. The continuity probe checks the nonce, signed engine commit, completed
migrations and original user/credential across installation. It now measures the
configured Identity serving surface when using a PAT; the receiver's separate
signed image bindings, controller readiness, storage preservation and complete
Application-group checks remain mandatory. TLS verification, bounded bodies and
redirect refusal prevent transport changes from moving the bearer or substituting
evidence. Secret UID/resourceVersion fences remain unchanged.

Regression coverage includes `TestIdentityReaderVerifiesSignedCatalogAcrossReplicasAndRevocation`,
`TestIdentityReaderRefusesAuthorityAndProtocolConfusionBeforeQueries`,
`TestIdentityReaderTransportNeverForwardsBearerOrTrustsMalformedReplies`,
`TestInstallationPATReplacesAmbientSystemAuthorityAndRechecksRevocation`,
`TestInstallationContinuityUsesIdentityTLSAndOriginalPAT`, and
`TestIdentityInstallationReadsThroughNativeEngines`. The latter mounts the real
route on two independent engines, reads a signed fixture from the shared ledger,
and verifies catalog get/list, current-user, and native health. Run it with an
immutable build-info commit stamp to exercise successful health; an unstamped
test binary must refuse that claim. These local tests do
not certify a production installation. Install the supporting engine first,
retain the approved credential, update the two endpoint values, and require a
fresh native preparation/apply/completion receipt before claiming readiness.


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


## Owner-push installation proposals

`MEMQL_RELEASE_INSTALLATION_OVERLAY` may explicitly select
`landingMode: "owner_push"`. Omission retains the pull-request path. GitHub must
report pull requests disabled and confirm the repository's stable identity and
default branch. Preparation captures signed release pins on a deterministic
branch. `releaseLandInstallationProposal` confirms that exact commit with the
connected owner's GitHub user grant. The native fast-forward refuses changed
bases and reconciles lost replies without rebasing or repeating a different
change. The separate `landed_commit` receipt makes the revision selectable;
observation cannot land it and landing cannot apply it.

This does not configure a receiving installation. Activation still requires
`MEMQL_INSTALLATION_CONFIG_NAME`, the named `installation.json`, authenticated
catalog and serving probes, registry access, a digest-pinned TLS renderer,
source capture, scoped Kubernetes permissions, and immutable healthy baseline
revisions for every application. An Application tracking `main` must be deliberately
pinned to its currently verified commit before activation. Preserve its UID,
resource ownership, manual sync and prune protections. Do not advertise deployment
readiness from a saved proposal or from a healthy Argo status alone.
