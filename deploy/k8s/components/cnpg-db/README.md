# `cnpg-db` — the database, as a component

The seam a deployment composes its database from: a client cluster today, a
memQL Cloud tenant later. It mirrors the way [`dsl-bundle`](../dsl-bundle/kustomization.yaml)
delivers product DSL — one shape, values per overlay.

Epic memql#3842, task memql#3851.

## What it ships

| Resource | Purpose |
|---|---|
| `Cluster` | the Postgres instances |
| `Database` | declarative extensions (`timescaledb`, `vector`) |
| `ObjectStore` | where WAL and base backups land |
| `ScheduledBackup` | the nightly base backup |

Every environment gets **the same four**. What an overlay changes is numbers and
endpoints. That is the parity doctrine applied to the database, and it is the
reason this is a component rather than three near-identical manifests that drift.

## Composing it

```yaml
components:
  - ../../components/cnpg-db
  - ../../components/cnpg-db/presets/mid     # optional; see below

patches:
  - target: { kind: ObjectStore, name: memql-db-backup }
    patch: |
      - op: replace
        path: /spec/configuration/destinationPath
        value: "azure://memql-db-backups/"

images:
  - name: memql-db
    newName: acrmemql.azurecr.io/memql-db
    digest: sha256:...
```

Worked examples live in [`../examples/`](../examples/) and are rendered by
`component_test.go`, so they cannot drift from this page.

### Two things every overlay must supply

**1. `destinationPath`.** It is a *required* field on the CRD, so the component
ships the placeholder `PATCH-ME-IN-THE-OVERLAY`. An overlay that forgets does
not error — it **archives nowhere**, while the Cluster reports Ready and the
pods look healthy. `TestNoOverlayShipsThePlaceholderBackupDestination` refuses
that.

The form differs between a real account and an emulator, and that is a property
of the *emulator*, not of the environment:

| Target | `destinationPath` |
|---|---|
| Azure Blob | `azure://<container>/` |
| Azurite (local) | `http://azurite:10000/<account>/<container>` |

barman rejects the `azure://` form against an emulator as *"malformed"*, and the
only symptom is `ContinuousArchivingFailing` on a healthy-looking Cluster.

**2. The image.** The component names `memql-db` with **no tag**, so an overlay
that forgets its `images:` entry fails to pull rather than silently running some
other Postgres.

> `kustomizeconfig/images.yaml` is what makes that `images:` entry work at all.
> Kustomize's transformer knows the container paths in core workload kinds and
> nothing else; CNPG names its image at `spec/imageName`, which is invisible to
> it. Without that file the entry is a **silent no-op** — the render succeeds
> with an untagged image, and the failure lands at apply or pull time while the
> overlay looks correct in review.
>
> Wherever it is pinned, **the tag must begin with the PostgreSQL major**: CNPG's
> webhook parses it to derive the major version. See
> [`deploy/db-image/README.md`](../../../db-image/README.md).

## Presets

Values only — never divergent manifests.

| Preset | Instances | Resources | Storage (data + WAL) | HA | `max_connections` |
|---|---|---|---|---|---|
| `entry` | 1 | 1 vCPU / 4 GiB | 32 + 16 GiB | off | 200 |
| `mid` | 2 | 2 vCPU / 8 GiB | 128 + 32 GiB | on | 400 |
| `top` | 3 | 4 vCPU / 16 GiB | 256 + 64 GiB | on, 3 zones | 400 |

`TestPresetsMatchTheirDocumentedTiers` asserts every number in that table. A
preset is a promise with a price attached: "mid gives you two instances and
128 GiB" quietly becoming something else is not a rendering bug, it is a
deployment that does not match what a customer bought — and nothing about the
running cluster would say so.

**The HA toggle is two values, and the second is the one that gets forgotten.**
Raising `instances` while leaving the component's single-instance
`enablePDB: false` inherited means a node drain can take the primary and its
only replica together — precisely what the second instance was bought to
prevent. At one instance the opposite is true: a PDB there permits *zero*
disruptions, so a drain blocks forever on a pod nothing can evict, with no error
naming the cause.

**Local uses no preset.** The presets describe what a customer buys, and a
developer laptop is not a tier: `entry` alone would ask for 1 vCPU / 4 GiB and
48 GiB of disk on a machine that also runs the whole mesh. The local overlay
composes the component and supplies its own smaller values.

**`top` sets no storage class.** Premium SSD v2 is LRS/zonal-only, which is
*correct* here — zone redundancy comes from Postgres replication, not from disk
replication, and paying for ZRS disks under a replicated database buys the same
property twice. But the class *name* is cloud-specific, so it belongs to the
overlay.

## Credentials

`bootstrap.initdb.secret` names **`memql-db-app-creds`**, a
`kubernetes.io/basic-auth` Secret. The *name* is the contract, so the component
stays ignorant of where the value came from:

- **local** — `make secrets` writes it (`scripts/k3d/seed-secrets.sh`), from the
  same two variables it builds the DSN from, so the database cannot be created
  with one password and connected to with another;
- **cloud** — ESO syncs it from Key Vault.

An overlay whose cluster identity uses workload identity replaces the
ObjectStore's `azureCredentials` block with `inheritFromAzureAD: true`.

## Related

- [`deploy/cnpg/README.md`](../../../cnpg/README.md) — the operator stack, upgrade
  procedure, and the `enablePDB` trap in full
- [`deploy/db-image/README.md`](../../../db-image/README.md) — the operand image,
  the two-`.so` upgrade choreography, and the tag constraint
- [`docs/internal/ops/timescaledb-license-compliance.md`](../../../../docs/internal/ops/timescaledb-license-compliance.md)
  — why Community edition, and the positioning it obliges
