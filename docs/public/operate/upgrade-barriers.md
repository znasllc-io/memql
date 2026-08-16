---
title: Upgrade barriers
audience: public
status: stable
area: operate
sinceVersion: 0.19.0
owner: znas
---

# Upgrade Barriers

Moving a local cluster to another release is normally a **retag**: the plugin
moves the pinned checkout, reconciles the local overlay from it, and every other
install step verifies itself and skips. That is the whole reason one button can
honestly offer it.

Between some pairs of releases it is not a retag. This page lists those pairs,
says what each one actually changes, and gives the procedure that replaces the
button.

**The plugin refuses rather than warns.** When a move crosses a barrier, the
confirmation dialog becomes a refusal and the button does not run. A warning an
operator can click past is not a safeguard for a change that can leave a cluster
running with an empty graph and no error anywhere.

## The barriers

| Moving past | What changes |
|---|---|
| `v0.18.0` | The local cluster's database changes from an in-overlay Postgres Deployment to a CloudNativePG cluster, and the cluster gains an operator stack (cert-manager + CloudNativePG). |

The machine-readable copy is `editors/vscode/src/version/barriers.ts`, and it is
the one the plugin reads. This table is prose for a human; that file is the
table.

---

## Moving past v0.18.0

### What changes

At `v0.18.0` the local overlay listed `postgres.yaml` — a plain
`Deployment/postgres` running `timescale/timescaledb:2.19.1-pg16`, with a
`PersistentVolumeClaim/postgres-data` holding the data and credentials in a
`memql-local-db-creds` Secret.

After it, the overlay composes `deploy/k8s/components/cnpg-db` instead: a
CloudNativePG `Cluster/memql-db` with its own storage, its own operand image,
and its credentials in `memql-db-app-creds`. `scripts/k3d/up.sh` gained
`install_operator_stack`, which registers cert-manager and CloudNativePG as
ArgoCD Applications and waits for them, because the CNPG manifests are custom
resources that nothing serves until the operator is running.

See [database-platform.md](database-platform.md) for what the CloudNativePG
platform is and why the cluster runs it.

### Why the checkout move cannot carry it

Two different things break, and the second is the dangerous one.

**The operator stack has to exist first.** The overlay after v0.18.0 contains
`Cluster` and `Database` resources, which are CloudNativePG CRDs. Applying them
to a cluster with no CNPG operator fails on kinds the API server does not
recognise — a loud, obvious failure, and not the problem.

**The data does not follow.** The old `postgres-data` volume belongs to a
Deployment the new overlay does not contain, and the new CNPG cluster
initialises an empty database of its own. Nothing in the reconcile reads the old
volume, and nothing reports that it did not: the pods come up, the engine
migrates a fresh schema, health checks pass, and the operator gets a working
cluster containing none of their graph. That is a silent outcome, which is why
this is a refusal and not a warning.

WARNING: the old PVC is not deleted by the move, so the data is still on the
machine until something removes it. `make down PURGE=1` and the plugin's
uninstall graph both remove it. Take the dump below **before** either.

### Procedure A — discard the data and re-install (the usual choice)

A local cluster is a development cluster. If nothing in it needs to survive,
this is the shortest correct path and the one to prefer.

- [ ] Uninstall the cluster (the plugin's **Uninstall** action, behind its
      removal preview; or `make down PURGE=1` in the checkout).
- [ ] Install again at the new tag (the plugin's **Create deployment**; or
      `make up` in a checkout moved to the new tag).

The install graph registers the operator stack and provisions the CNPG cluster
as part of a normal first install, so there is nothing barrier-specific to do.

### Procedure B — carry the data across

Run this **while the old cluster is still up**. Every name below is read off the
manifests on each side, but confirm the dump is non-empty before you tear
anything down.

- [ ] Dump the old database:

      kubectl exec -n memql deploy/postgres -- \
        sh -c 'pg_dump -U "$POSTGRES_USER" -d memql --no-owner --no-privileges' \
        > memql-pre-cnpg.sql

- [ ] Check the dump. It should be tens of megabytes at minimum for a cluster
      that has been used, and end in `PostgreSQL database dump complete`.

- [ ] Uninstall, then install at the new tag, exactly as in Procedure A. The new
      cluster comes up with an empty `memql` database owned by `memql`.

- [ ] Restore into the CloudNativePG cluster:

      kubectl exec -i -n memql memql-db-1 -- \
        psql -U memql -d memql < memql-pre-cnpg.sql

- [ ] Restart the engine pods so they re-read a database that changed underneath
      them: `kubectl rollout restart -n memql deploy`.

Notes that matter:

- **`--no-owner --no-privileges`** is not optional. The old Deployment's role
  name comes from `memql-local-db-creds` and the CNPG database is owned by
  `memql`; a dump carrying the old ownership fails to restore against the new
  role.
- **TimescaleDB and pgvector extensions** are created by the engine's own
  migrations on first boot against the new cluster, so the dump does not need to
  carry them. A dump that does will report the extensions already exist; that is
  benign.
- **The operand image is not the old image.** The new cluster runs the operand
  built in `deploy/db-image/`, not `timescale/timescaledb:2.19.1-pg16`. Restoring
  a logical dump is what makes that a non-issue — a filesystem-level copy of the
  old PVC is not a supported move and is not offered here.

### Moving back before v0.18.0

The plugin refuses this direction too, and it is the worse one: it puts a plain
Postgres Deployment in front of data that lives in a CloudNativePG cluster, and
the CNPG `Cluster` resource is left behind by an overlay that no longer contains
it. There is no supported downgrade procedure. If a release after v0.18.0 has to
be backed out of, do it by re-installing at the older tag from empty, restoring a
dump you took beforehand.

---

## Adding a barrier

A barrier entry is written by whoever ships the change, in the release that
ships it — not from a plan.

1. Add an entry to `UPGRADE_BARRIERS` in
   `editors/vscode/src/version/barriers.ts`. `afterVersion` names the last
   release **before** the barrier, so an entry can be written before the release
   carrying the change has a number.
2. Add a section to this page and point the entry's `docHref` at it. The tests
   assert the file exists; they cannot assert it says anything useful, so this
   is the part that needs a person.
3. Keep the table ordered oldest-first. A move can cross two barriers, and the
   operator performs them in order.

`editors/vscode/test/barriers.test.ts` holds the newest entry at or below
`DEFAULT_STACK_TAG` (`editors/vscode/src/install/stackPin.ts`). An entry newer
than the installer's reviewed pin describes a crossing nobody can make yet,
which means the refusal never fires and the entry looks correct for exactly as
long as it is wrong.

## Related

- [The recorded cluster version](cluster-version-record.md) — where the `from`
  side of the comparison comes from, and why it is recorded rather than observed
- [Database platform (CloudNativePG)](database-platform.md) — the platform the
  v0.18.0 barrier moves onto
- [Reproduce staging locally](reproduce-staging-locally.md) — the local k3d
  runbook, including backup and restore drills on the CNPG side
