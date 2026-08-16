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

The learners write through this key from four sources, and they do not
agree in trustworthiness. The rule is that a write happens only when the
value **differs** and **does not downgrade record quality**: a value that
is not release-shaped never overwrites one that is.

Without that rule, an install receipt naming `v0.18.0` would be replaced
by a live connection reporting the build stamp `0.15.0-1737072000` -- a
strictly worse record, written by a strictly more recent observation.

## Preservation guarantees

Both tools read-modify-write this file as a YAML *document*, not by
serialising a struct over it. Two properties follow, and both are
enforced by tests:

- **Comments survive.** An operator's comments are part of their file.
- **Unmodelled keys survive.** A key written by a newer version of the
  other tool is left untouched.

The version learners write far more often than any human edit does, so a
write path that stripped either would destroy an operator's file quickly
rather than slowly.

## Coordination history

This key follows the same cross-tool coordination as its neighbours: the
`token` rename (which fixed a field advertising a credential class the
bff structurally rejects) and the `local` flag both went through it. Add
a key to one tool's model and the other's next write is the hazard --
which is why the cockpit's `Version string` lands as part of the same
change rather than after it.

## Related

- [Upgrade barriers](upgrade-barriers.md) -- when moving between two
  recorded versions is not a retag, and the upgrade is refused with
  instructions instead.
- [Deploy-bundle runbook](deploy-bundle-runbook.md) -- how a release is
  actually deployed once a version has been chosen.
