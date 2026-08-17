---
title: The recorded cluster version in clusters.yaml
audience: public
status: stable
area: operate
sinceVersion: 0.19.0
owner: znas
---

# The Recorded Cluster Version

`clusters.yaml` carries a `version` key per cluster: the release that
cluster is believed to be running. This page is the contract for that
key, because the file is shared -- the VS Code plugin and the memQL
Cockpit both read and write it, and a key one of them does not model is
at risk on the other's next write.

## Why the record exists at all

**No installed cluster can currently state its release honestly.**

- `ServerHello.version` is the literal string `"v1"`. It names the wire
  protocol, not the release, and always has.
- The `VERSION` file has read `0.15.0` at every tag since v0.16.1, and
  the Dockerfile overwrites even that with a build stamp of the form
  `0.15.0-<epoch>` before the binary ships.

memQL engine releases now stamp a real version into the binary and
introduce it on an additive `ServerHello.engine_version` field. That
fixes the problem going forward and **cannot reach backwards**: it
cannot teach an already-installed v0.18.0 to introduce itself, and
v0.18.0 is precisely the cluster an operator most needs to identify.

So the version is *recorded* rather than *observed*. The record is
readable with the cluster switched off, unreachable, or never dialled --
which matters, because the failure that motivated this work was a
session that had just been severed. A version only a live connection
could report would be missing in exactly the moment it was needed.

## The key

```yaml
clusters:
  - name: local
    endpoint: api.memql.localhost:443
    version: v0.18.0
```

| | |
|---|---|
| YAML key | `version` |
| Type | string |
| Plugin model | `ClusterConfig.version?: string` (`clusters/model.ts`) |
| Cockpit model | `Version string` with `yaml:"version,omitempty"` |
| Absent | The cluster's release is unknown. Not an error. |

Every cluster in an operator's file predates this key, so **absent is the
normal case** and must render as unknown rather than as anything else.

## Write semantics: three states, and why

`version` is a plain string, so it takes the same three-state write
semantics as every other string field in this file:

| Value supplied | Effect on disk |
|---|---|
| `undefined` | Leave whatever is on disk alone |
| `""` | Delete the key |
| a string | Write it verbatim |

The distinction between the first two is load-bearing here in a way it is
not for a hand-edited field. The version is learned *opportunistically*
from several sources of differing trustworthiness, and most refresh
attempts learn nothing. "I learned nothing this time" has to be spelled
differently from "this cluster has no version", or a single failed
refresh would erase a version a better source had already established.

This is also why the key is a string rather than anything richer. The
neighbouring `local` flag is a boolean, and booleans in this file follow
a *different* rule -- `false` serialises as ABSENT, because the cockpit
declares that field `omitempty` and drops the key whenever it is false,
so a tool that wrote `local: false` back would make the two churn the
file against each other on every save. A string has no such collapse:
there is no third state to negotiate, and `version: ""` is simply not a
thing either tool writes.

## What may be recorded

**Whatever the cluster said, unmangled.** The record is a report, not a
judgement. A release tag is the common case, but the value may equally be:

- a release tag, with or without the `v` prefix (`v0.18.0`, `0.18.0`)
- a branch name
- a commit sha
- the `0.15.0-<epoch>` build stamp the Dockerfile produces

Anything unparseable is reported as `notComparable` by the comparison
module -- and never as "up to date", which is the failure direction that
reproduces the original incident. That module can only make that call on
a value that reached disk intact, so nothing on the write path may
normalise, coerce, or reject what it is given.

## Record quality: a write may not downgrade

The learners write through this key from five sources, and they do not
agree in trustworthiness. The rule is that a write happens only when the
value **differs** and **does not downgrade record quality**: a value that
is not release-shaped never overwrites one that is.

Without that rule, an install receipt naming `v0.18.0` would be replaced
by a live connection reporting the build stamp `0.15.0-1737072000` -- a
strictly worse record, written by a strictly more recent observation.

### The five sources, most trustworthy first

| Source | What it actually knows |
|---|---|
| `handshake` | `ServerHello.engine_version` -- the release the engine binary was **cut from**, stamped in at link time |
| `receipt` | what the install wizard **checked out**, on this machine, at one moment |
| `argocd` | the `targetRevision` the manifests **ask for**. Legitimately a branch name |
| `deployControl` | the resolved lockfile's version -- the deployment **record**. Absent for a local cluster |
| `live` | `memqlVersion()` over a connection: the same fact as `handshake`, reached through the DSL |

Quality still outranks trust: a branch name from the most trusted source
loses to a real tag from the least. The order above only decides between
values that each name a release.

**`handshake` ranks first because it is the only one that cannot
disagree with the binary serving the connection.** Every other source
describes something adjacent to the running engine and can drift from
it -- the checkout can be moved after the install, the manifests can ask
for something that has not reconciled, the deployment record can outlive
what it deployed. So when the handshake and the install receipt both
name a release and the two differ, the receipt is the stale one.

Two answers from this source are non-answers, and neither needs a rule
of its own. A cluster whose engine predates the field states `""`, which
is dropped before ranking; a binary not cut from a release states `dev`,
which is not release-shaped and so can never land on a recorded release.
Both were true of the entire fleet on the day the source was added,
which is why it changed no cluster's record until one was upgraded.

## Preservation guarantees: the two tools differ

**They do not write this file the same way, and the difference is the
whole reason a shared key needs coordinating.**

**The plugin** read-modify-writes the file as a YAML *document*, not by
serialising a struct over it. Two properties follow, both enforced by
tests in `clustersFile.test.ts`:

- **Comments survive** a plugin write. An operator's comments are part of
  their file.
- **Unmodelled keys survive** a plugin write. A key written by a newer
  cockpit is left untouched.

That matters more for this key than for a hand-edited one, because the
version learners write far more often than any human edit does.

**The cockpit does not.** `cli/config.SaveClusters` is a
`yaml.Marshal` of the whole `ClustersFile` struct, so its next write
rewrites the file from the struct alone. Comments are lost and **any key
it does not model is dropped**.

This is a known limitation rather than a bug to route around, and it is
asserted so it cannot drift silently: `TestClusterLocalFieldRoundTrip`
in the cockpit's `cli/config/clusters_test.go` ends by checking that a
`future_key` written by another tool does **not** survive the round
trip.

The consequence is the rule:

> **Every key in `clusters.yaml` must be modelled on BOTH sides.**
> Adding one to a single tool means the other silently deletes it on its
> next write.

## Coordination is a requirement, not a courtesy

This key goes through the same cross-tool coordination as its
neighbours -- the `token` rename (which fixed a field advertising a
credential class the bff structurally rejects) and the `local` flag.

Given the asymmetry above, that coordination is **load-bearing**. If the
cockpit preserved unmodelled keys, the plugin could ship `version` alone
and nothing would need coordinating at all; because it does not, the
cockpit's matching `Version string` has to land as part of the same
change rather than after it. A recorded version that the operator's next
cockpit command silently erased would be worse than no version at all --
it would be a field that works until it does not, for reasons nothing on
screen could explain.

## Related

- [Upgrade barriers](upgrade-barriers.md) -- when moving between two
  recorded versions is not a retag, and the upgrade is refused with
  instructions instead.
- [Deploy-bundle runbook](deploy-bundle-runbook.md) -- how a release is
  actually deployed once a version has been chosen.
