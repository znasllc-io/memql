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

Discovery inventories are compared in full within one observation, with only the
top-level `APIResourceList.resources` order normalized. On the installed
Kubernetes cluster, Argo, CNPG and Cilium discovery returned identical entries in
different orders; comparing their raw arrays refused preparation as changed
discovery. Added, removed or duplicated resources, changed scopes, verbs, kinds,
versions and extra fields between two reads of one observation still refuse it.
`TestReceiverDiscoveryResourceOrderIsStableAcrossReadsAndRecovery` and
`TestReceiverDiscoveryStillRefusesResourceContractChanges` cover that boundary.
What a plan saves across observations is narrower; see
[What a saved plan binds](#what-a-saved-plan-binds).
For an explicit live read-only check, set
`MEMQL_INSTALLATION_DISCOVERY_TEST_KUBECONFIG` to an absolute operator kubeconfig
path and run `TestReceivingDiscoveryThroughInstalledKubernetes`. It reads only
API discovery, never namespaced objects or credentials, logs the contract a plan
would save, and does not qualify installation application or completion.

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

Native source collectors carry the trusted `manual` event, bound into their
request identity before dispatch. This lets the installed queue apply its
operator-maintenance priority while retaining its quota, dedicated build pool
and suspended-job admission. The event and priority are not client inputs.
An absent event is refused when priority selection is configured; it does not
fall back to an unqueued job. The real executor/Workbench hop and job-builder
regression, `TestSourceCaptureUsesWorkbenchHopAndFreshReplicaRecovery`, checks
the manual class and suspended admission on both initial dispatch and a fresh
replica's recovery. That test does not claim live installation completion.

An unfinished preparation can be cancelled by its original operator from
**Cluster > Updates**. `releaseCancelInstallationPreparation` runs one bounded
pass of the existing installed retirement workflow: stop the scoped producer,
fence its artifact writes, release its references, confirm retirement of the
complete inventory and remove each retired file's Library entries before
releasing the installation reservation. A removal needs the journal's own
retirement receipt for that file, and an unconfirmed one keeps its page, so a
cancelled preparation leaves no Library entry pointing at a retired file.
Preparations cancelled before that step left those entries behind; the
pipeline artifact sweep removes them, and it never retires a preparation's
files ([Retention](../../public/operate/pipelines-substrate.md#retention)).

The Library store refuses to retire a file it keeps
(`pipelinesteps.ErrArtifactRetained`): one its owner filed, labelled, edited,
gave another version or trained into a knowledge domain, one whose Library
history names another owner or source, or one whose entry is not ready. The
last includes a capture whose upload the cancellation interrupted before its
entry was marked ready, which nobody chose to keep. The store also refuses a
file an active durable reference pins; a capture's only one, its own source
reference, is released before its first page. Cleanup records the refusal on
the page entry as `retained`, leaves the file's bytes and Library entries, and
moves on, so a kept capture does not hold the installation reservation. A kept
file already has a committed, verified object, which no late upload can
replace, so it needs no tombstone. Until 2026-11-08 the refusal read as an
unconfirmed retirement, and the preparation stayed `retiring` and held the head
for good.

A recorded answer is final. The pipeline sweep skips a kept file and asks again
on its next visit; this cleanup never asks again, so the file stays in its
owner's Library even after they stop keeping it. The page entry goes when the
page advances; afterwards only the file's upload journal row, still not
retired, under the preparation's source run shows what was left. If two passes
ask either side of the owner unfiling the file, a retirement receipt outranks
the kept answer while the page still holds the file, and the file still leaves
the Library. If the kept answer has already moved the page on, the late receipt
is refused: the file is retired with its Library entries left, which the
pipeline sweep removes, or, when that retirement did not finish, it stays
`retiring` and nothing finishes it. That needs two passes of one preparation at
once and the owner's change between their store calls; closing it needs cleanup
passes serialized per preparation, which is not done. A later preparation runs
under its own source run, so its fence and inventory never include an earlier
preparation's files.
`TestPreparationRetirementLeavesAKeptCaptureAndReleasesItsHead` and
`TestRetirementWorkflowInstalledPassLeavesKeptCapturesOnFreshHost` cover a kept
capture, including a lost reply asked again on another connection, and
`TestPreparationRetirementReceiptOutranksARacingKeptAnswer` covers the two
passes while the page holds the file; their Library is a fixture. The real
store's refusal of a labelled or versioned file is
`TestPipelineLibraryDB/TestPipelineRetentionLibraryDB`, in `app`.

Use **Resume cancellation** when cleanup remains pending. An uncertain reply must
be reconciled from the saved preparation; it cannot authorize another capture.
The cleanup entry does not depend on optional release choices or GitHub reads.
It refuses promoted plans and other operators, including a different owner.
Legacy captures retain their absent event and original receipt identity for
cleanup. Once cleanup starts, its recipe is bound to the executing engine's
build commit, so any engine change refuses resumption rather than changing a
partially completed cleanup. A preparation left `retiring` across an upgrade
therefore keeps the installation reservation, and no product action releases
it. `TestSelectedPreparationCancellationRecoversLegacyScopeWithoutReleasingAnotherHead`
covers interrupted cleanup, a fresh host, ownership and a successor reservation.
`TestRetirementWorkflowInstalledPassPurgesRetiredCapturesOnFreshHost` covers a
lost Library removal reply repeated on a fresh host, and
`TestArtifactRetentionPurgesARetiredPreparationsCapturesAcrossReplicas` the
sweep's removal for an earlier cancellation.

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

### What a saved plan binds

Preparation saves a configuration invariant that Apply, completion, Restore and
a restoration's progress check each compare with a fresh observation
(`receiver_reads.go`, `receiving-configuration-invariant-v2`). It binds what each
input means, so routine cluster activity no longer strands a started update:

| Input | Bound | Checked fresh at every call instead |
|---|---|---|
| ConfigMaps and Secrets the receiver reads | UID and an HMAC of their type, immutability and data, keyed from the catalog credential; for Argo's renderer ConfigMap (`argocd-cm`) also its `app.kubernetes.io/part-of` label, without which Argo stops reading it | Other labels, annotations and `resourceVersion` are not bound |
| Repo-server Deployment | UID, selector and pod template, without the `kubectl.kubernetes.io/restartedAt` annotation | Its Pods, ReplicaSets and EndpointSlices: every endpoint ready and addressing a Pod of the Deployment's current revision (the Pod's ReplicaSet, read by name, has the Deployment's pod template apart from `pod-template-hash`, and the Pod carries that hash), with the template's containers, init containers, volumes and pinned runtime image, the template's service account, security context, host namespaces, host aliases, DNS and runtime class, and no ephemeral container; one per wanted replica |
| Repo-server TLS Secret | UID and its trust anchors (name and public key of `ca.crt`, else of the top of the `tls.crt` chain) | The Secret's leaf and the certificate the repo-server serves both chain to those anchors and name the Service |
| Repo-server Service, Argo projects | UID and spec | Status and metadata are not bound |
| Kubernetes discovery | Major and minor version; whether each resource the receiver reads is served, with its kind, scope and the verbs it uses | Patch build fields, `/api` addresses and every other group, version or resource are not bound. The APIs the plan's own render applies are resolved from fresh discovery by resource evidence at every call and compared through `ResourceDiffDigest` |

Before this, the invariant bound every `resourceVersion`, each repo-server Pod,
ReplicaSet and EndpointSlice, the exact serving leaf and the whole discovery
surface (`-v1`). On a healthy cluster a repo-server restart or reschedule, a
readiness flap, an AKS patch, any CRD installed or upgraded, a cert-manager
renewal and any annotation write to a watched object each refused every started
update's Apply, completion and Restore.

A changed value still refuses: a rotated password or token, edited ConfigMap
data, another CA, a different repo-server template, image or Deployment, a
serving Pod of an older revision, with another identity or host context or with
a debug container attached, a Kubernetes minor upgrade, or a removed or rescoped
resource the receiver reads. Kubernetes cannot remove an ephemeral container
from its Pod, so after debugging the repo-server, replace its Pod (delete it,
or `kubectl rollout restart`) before the update continues. Only the
`kubectl.kubernetes.io/restartedAt` restart marker is ignored: other restart
mechanisms, such as Stakater Reloader's
annotation or environment strategies, change the bound template and still
refuse a started update.

Discovery is still read in full within each observation, for the renderer's API
list, so an API group whose aggregated service does not answer, or discovery
beyond the receiver's bounds (128 groups, 128 group versions, 256 reads), still
refuses that call until it is resolved. That refusal is transient and predates
this change; it does not strand a saved plan.

The HMAC exists because the digest is saved with the plan and a ConfigMap or
Secret may hold a guessable credential. The package's Secret fingerprint
(`sensitiveDigest`) is unkeyed, so it is not reused; the key is derived from the
catalog credential the receiver reads at every call and never persists. The key
is only as strong as that credential: the reference installation's is a minted
PAT, while a short or guessable token there would weaken the HMAC. Rotating it
re-keys every digest and refuses, as any rotation does.

A self-signed renderer certificate (no CA) is its own trust anchor, so **a
self-signed leaf whose key rotates on renewal is a new trust anchor and still
refuses**: every update started before that renewal can no longer be verified
and must be closed. cert-manager rotates the key at each renewal by default
(`privateKey.rotationPolicy: Always` since 1.18). The reference installation's
renderer certificate (memql-znas `deploy/installation/znas-production/renderer.yaml`)
is issued that way today, so its renewals, about every 75 days, still strand the
updates started before them until that instance changes. A self-signed leaf
keeps its anchor only when cert-manager keeps its key
(`privateKey.rotationPolicy: Never`), and even then, between a renewal and the
repo-server restart Argo 2.13 needs to load the new leaf, its old leaf no longer
verifies, so renders refuse until that restart. A leaf issued by a long-lived CA
avoids both. `TestReceiverSelfSignedRendererAnchorIsItsKey` covers the kept key,
the old leaf before the restart, and the rotated key.

Plans prepared by engines up to 0.31.28 saved the `-v1` form. The receiver still
computes it from the same reads, so such a plan, including the update that
installs this engine, is compared with `-v1`'s strictness: it completes or
restores when nothing `-v1` binds changed, and otherwise refuses as before and
is closed by its operator ([Closing without completion proof](#closing-without-completion-proof),
which does not consult the invariant). The live checks are this engine's for
every plan, so the renderer's TLS handshake trusts the Secret's anchors and not
the leaf `-v1` pinned. A plan saved in `-v2` cannot be verified by an engine that
predates it, so an update that installs an older engine (a downgrade below this
release) cannot complete and must be closed.

A candidate whose render would change the receiver's own configuration ConfigMap
(`memql-installation`, rendered by the `memql` Application from the instance
repository's `deploy/k8s/instance/installation/installation.json`) is refused at
preparation, and again before an older engine's plan first starts. The
preparation's reservation still holds the installation, so the reason says to
cancel this preparation, deploy the change by hand (sync the Application that
renders the ConfigMap to a revision that declares it), then prepare the update
again. Every step after such a sync would compare against configuration the plan
itself rewrote, so it could never complete. Changing only that ConfigMap's labels
or annotations is allowed. A candidate that changes another input the receiver
reads (a rendered Secret, AppProject or repository Secret, or the
`MEMQL_INSTALLATION_CONFIG_NAME` a rendered environment ConfigMap selects) is not
refused at preparation; the invariant then refuses every later step and the plan
is closed. That strands but never bypasses a check.

Regression tests: `receiver_churn_test.go` (restart, current revision, Pod
identity, ephemeral containers, content, keyed digest, TLS anchors, legacy golden
digests, the preparation refusal), `receiver_discovery_test.go` (the discovery
contract and the render profile that makes it safe),
`receiver_own_configuration_db_test.go` (the start-fence re-check) and
`TestInstallationUpdateSurvivesReceiverChurnThroughCompletionAndRestore`
(`receiver_churn_protocol_test.go`), which prepares, applies, completes and
restores a two-Application group through the real journal with a repo-server
restart, certificate renewal, Kubernetes patch, new CRD and annotation writes
between every call, and refuses Apply for another CA or a rendered API no longer
served. None of these is a live cluster; production proof is a Cluster > Updates
update and restoration that each record their receipts.

This change makes no Kubernetes request engine 0.31.28 does not: the same paths
are read with the same verbs (the golden `-v1` digests prove the read set), so
installations need no new RBAC.

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
evidence. A rotated PAT changes the catalog Secret's bound content and refuses a
saved plan ([What a saved plan binds](#what-a-saved-plan-binds)).

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

### Application status and retained change history

Some installed Application CRDs have no status subresource. Their routine
controller status updates advance `metadata.generation` even when the complete
spec is unchanged. Comparing that generation in the long-lived configuration
digest refused preparation after a healthy Argo refresh. Removing the generation
check alone would hide a configuration edit followed by a restoration.

New preparations therefore capture a name-filtered Application list's global
resource version and retain a history cursor with each original intent in
`installation_preparations.application_continuity`. An object's own resource
version may predate history compaction even when freshly read, so it cannot
replace the list boundary. A call-owned guard checks the cursor every ten
seconds while preparation or pre-apply verification is running. It is joined
before the call returns or dispatches an owned Application write; no detached
controller or timer acquires installation authority.

An unchanged object version refreshes the global boundary without a watch.
Otherwise a bounded watch must account for every intervening event through the
freshly read object. The original UID, complete spec, intent marker and idle
operation must remain unchanged. Status-driven generation advances are accepted;
a real edit, including one restored before the next read, permanently invalidates
that preparation. Missing, expired or malformed history refuses and does not
advance the cursor. Concurrent replicas compare and persist cursors under the
existing original-operator, installation-head and workflow locks. A slower
successful read cannot erase another replica's invalidation. Recovery starts
from the retained cursor, never from a newly invented baseline.

The final patch needs a fresh native proof for the exact observed object version
and tests UID, resource version, generation and complete spec atomically. Cursor
JSON is not such a proof. Already owned effects reconcile through their original
intent marker. Existing intent-format-1 records retain their strict generation
fences, and nothing rebinds an uncertain reversal. If a paused preparation loses
required Kubernetes history, cancel it and prepare again. An uncertain started
effect must instead be reconciled from its saved intent. The additive cursor
migration cannot be reversed while retained history exists.

Reversals are history-backed too (2026-11-09). A reversal used to be planned
from a plain read with the strict generation fence and was never planned again,
so one Argo status write between planning and its member's patch refused that
patch on every retry. `argocd.PlanRollback` and the baseline read now use the
name-filtered list, so every new reversal is an intent-format-2 intent bound to a
watch boundary. No cursor is persisted for it: every proof
(`argocd.BoundaryProof`) rereads the whole window from the reversal's own
planning boundary, so an edit seen once is seen by every later check while
Kubernetes retains that history. With that history gone, the proof still holds
while the Application's generation has not moved since planning
(`unchangedSincePlanning`): every spec change advances the generation, so only
metadata can have changed, which is what the format-1 fence accepted.
`argocd.Admit` proves a member's reversal before its durable reverse start, so a
real edit by another writer refuses while the restoration can still be
withdrawn (below), and again for the exact object right before the patch. A
format-1 reversal is admitted against its strict fence the same way, before its
start. Status-only generation advances pass; a spec, marker, operation or
identity change still refuses.

A resumed restoration does not depend on that history surviving
(`rollbackWorkflowScope.replan`). A member whose saved reversal can no longer be
admitted, and of which nothing can still reach the cluster, is planned again
with `PlanRollback` after every check of the pass and stored in
`replanned_intents`, which supersedes the member's planned and any reconciled
intent (`rollbackRecord.application`). It qualifies when the member has no
reverse start and its history-backed intent's history is gone with its
generation moved (`ErrHistoryUnavailable`) or its format-1 intent fails the
strict fence; or when the member has started but its format-1 intent is
provably dead (`argocd.Superseded`: the Application still holds the intent's
exact baseline at a later generation, so no request formed for it can pass its
fence, and none landed, or the Application would carry its marker). A started
history-backed member is never planned again, because the version its in-flight
request tested is not retained, so one whose patch never landed and that is
resumed only after its history is gone and its generation moved stays held.
History that shows another writer's edit refuses as before. A reversal reserved
but never begun is planned again and replaced whole at reservation
(`replaceUnstartedRollback`). Reversals stored by engines up to 0.31.28 keep
their format-1 strict fence until one of those conditions replaces them.
Classifying an untouched baseline for a fence (`PlanUnappliedRollback`) still
compares the generation strictly with the forward intent's, so a status write
since the forward intent was planned leads to `PlanRollback` instead.

Restoration adds no Kubernetes verb or resource the forward history does not
already use: the name-filtered `list` and `watch` of each configured Application
(`limit=2`; `watch=true`, `resourceVersion`, `allowWatchBookmarks`,
`timeoutSeconds=1`) and its `get`, under the Role rule below.

Installers need `get`, `list` and `watch` on only their configured Application
names. Keep `resourceNames` on that Role rule: each list/watch supplies the exact
`metadata.name` field selector. Unfiltered collections and unrelated Application
names remain unauthorized. Patch permissions, credentials and subjects do not
change. Activate the scoped read permissions before deploying an engine that
uses history-backed preparation.

Regression coverage is in
[`continuity_test.go`](../../../integrations/argocd/continuity_test.go),
[`application_continuity_db_test.go`](../../../integrations/installation/application_continuity_db_test.go)
and the existing group recovery/rollback protocol suite. The PostgreSQL cases
cover replica recovery, sticky invalidation, cancellation, unavailable-history
retry and joined lifecycle cancellation. The opt-in
`TestApplicationHistoryThroughInstalledKubernetes` observes a real status-driven
generation change using an explicitly selected operator connection and named
Application. It performs no writes and does not qualify installer RBAC or live
installation completion; those still require their own checks.

## Rollback

`Preparer.RollbackSelected` accepts only the installation and exact selected plan
IDs. The original operator may also restore the latest successfully completed
release while the installation head has not advanced. Any later preparation or
plan, even a cancelled preparation, refuses that historical rollback. Another
installation holding a target also refuses it. The original forward completion
receipt and timestamp remain unchanged; reversal has a separate completion
receipt. The status projection reports whether this request is currently
eligible, but every effect still requires fresh native verification. It requires the original current operator. It records a reversal before
any reverse write and permanently fences further forward effects. Reversals
are derived from exact owned terminal controller operations; pending or foreign
operations refuse.

Restoring a completed update runs every refusable check before it takes the
installation back (2026-11-09). Admission used to reclaim the head and mark the
plan `reverting` first; a check that then refused (the configuration
invariant, preservation digests, changed migrations, the plan's own finished
operation) left a plan that close, cancellation and completion all refuse, so
the installation stayed held. Now the rollback recipe runs against the
completed attempt (`rollbackWorkflowScope.qualifying`): configuration and its
invariant, publications, compatibility, retained sources (reopened through
`reopenCompletedCapture`, fenced by a free head, an unchanged epoch and continued
eligibility instead of an active one), renders, resources and images, storage
and protected resources, migrations and the planned reversal of every member.
Only `installationRollbackReserve` takes the head, reclaims every Application
and stores the reversal, in one transaction that rechecks eligibility
(`reserveCompletedRollback`). A refusal before that leaves the attempt exactly
as it was.

A restoration that has reserved its reversal but recorded no reverse start for
any member (`restorationUnwritten`) has written nothing to Argo: every reverse
Apply first commits that member's start (`beginRollbackApplication`), under the
plan's effect lock and after every check its apply makes. A group records one
per member, and a single-Application reversal records its one member (`""`) the
same way (`rollbackRecord.perApplyStarts`). Before 2026-11-09 a
single-Application reversal's only start was the reversal's own
(`beginRollback`), which the Begin step commits ahead of those checks, so a
refusal there stranded a restoration that had written nothing. One whose intent
an engine up to 0.31.28 may have written (format 1 and not replanned) still
counts that reversal start as a possible write.

When any check refuses an unwritten restoration, `RollbackSelected` returns it
to `complete` under the plan's effect lock (`withdrawRefusedRestoration`): the
reserved reversal is deleted, the head and claims are released, and
`withdrawn_restorations` records who, when and the reversal's identity. A
withdrawal that fails is logged and the attempt is read again, so the reply
reports the state it is in. Its operator may also end such a restoration with
**Stop restoration** in Cluster > Updates (`releaseCloseInstallation`;
`selectionResult` sets `canClose` for it), which records their reason the same
way (`withdrawRestoration`). An attempt left `reverting` with nothing written by
an earlier engine is ended by either path. A restoration with any recorded
reverse start may have reached the cluster and is neither withdrawn nor
closable; its operator resumes it.

Across requests: a refusal cannot tell which request reserved the reversal, so
it also withdraws another request's reservation that has not begun, whose next
step then refuses because the attempt is complete again. A Restore that reserves
while another request's reservation has not begun replaces it rather than
refusing (`reserveCompletedRollback`). A preparation started while a Restore
qualifies (minutes, for the source and render checks) pre-empts the Restore at
reservation. An engine up to 0.31.28 that resumes a history-backed reversal takes
the member's start and then cannot prove the reversal; the restoration then waits
for a current engine to resume it.

Argo keeps only the last operation in `status.operationState`, so one later
sync of an Application (a hand sync of a manual-sync Application, or self-heal)
used to strand both completion and restoration, which counted success only from
the plan's own operation there. Every read that sees an intent's own success now
records it durably (`owned_successes`, per intent digest, with the member,
revision and source it synced): completion's controller read, and for a
reversal the rollback recipe's first read (the start of **Resume
restoration**), its apply's check of a member already applied, and its
observation. Both directions' reads pass the records to `argocd.Observe` and
`PlanRollback` (`argocd.WithRecordedSuccess`, carried in the call's context
because the receiving configuration reader observes intents too). A record, or
for a forward member of an update that already completed its completion
receipt, counts only while the Application still holds the plan's marker and
spec, nothing is pending or running, Argo compares the plan's revision with the
plan's source, and the later sync Succeeded with a result naming that same
revision and source; completion still needs Synced and Healthy. A later sync to
another revision or spec still refuses.

A forward plan whose own success was replaced before any read observed it has no
record and keeps refusing until its operator closes it. A restoration in that
state cannot be closed, because its writes reached the cluster, so a started
reversal member may instead be proved from Argo's `status.history`
(`argocd.WithDeploymentHistory`, added by `recordedSuccessContext`): the newest
entry by `id` must name the reversal's revision and source and be `deployedAt`
no earlier than the reversal's start (`installation_revision_rollbacks.started_at`,
compared to the second, which is all Argo records). Argo appends an entry only
for a sync that succeeded. The proof is read again at every check and never
recorded (`observedSuccess`), and forward intents never use it. It compares
Argo's clock with PostgreSQL's: an Argo controller clock running behind the
database's by more than the start's sub-second part leaves the proof
unavailable, which fails closed.

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

## Closing without completion proof

`Preparer.CloseSelected` (`releaseCloseInstallation`) ends an applying plan whose
evidence can no longer match, typically a saved protected-resource digest no
engine can reproduce, or a `-v1` configuration invariant that routine cluster
activity has moved past ([What a saved plan binds](#what-a-saved-plan-binds)).
Only the original operator may close it, with a reason.
Under the plan's effect lock it reads every started member's intent, and every
started reversal, and refuses while one still has its own Argo operation
queued, running or terminating. It does not require a member to sit at its
intent, because a hand deploy may have moved it on. The `closed` attempt keeps
its observations and any reversal, has no completion receipt and is never
rollback-eligible; the close releases the head and every member's claim.
A `reverting` attempt (restoring a completed update) closes differently: only
while its restoration has recorded no reverse start, and then back to the
verified `complete` state with the reason recorded (see Rollback above). The
design and its residual window are in
[installation revision effects](../planning/installation-revision-effects.md#closing-without-completion-proof).

## Local qualification

Run against a dedicated disposable database with the required extensions, never
an active installation's database. Each worker needs its own database. For
example, after setting `MEMQL_DATABASE_DSN` to that worker's disposable database:

```bash
MEMQL_REQUIRE_DB=1 GOMAXPROCS=2 go test -p 1 -count=1 \
  ./integrations/installation \
  -run 'TestInstallationGroup|TestPreparationWorkflowProtocolRecovery|TestRollbackJournal|TestInstallationClose|TestInstallationRestoration|TestInstallationCompletion|TestInstallationUpdateSurvivesReceiverChurn|TestSelectionEntrypoints'
```

`restore_lifecycle_db_test.go` drives a two-Application installation through
the installed preparation, revision, completion and rollback recipes for the
stranding cases above: a restoration refused by a check before and after its
reservation, a stranded restoration closed, a written one refused, completion and
restoration after a later sync of the same and of another revision, and a status
write or another writer's spec edit between a reversal's planning and its patch.
`integrations/argocd` covers the recorded-success conditions
(`revision_recorded_test.go`) and history-backed reversals
(`rollback_history_test.go`).

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
