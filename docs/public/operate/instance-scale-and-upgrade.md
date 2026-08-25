---
title: Scaling and upgrading an instance without interrupting users
audience: public
status: stable
area: operate
sinceVersion: 0.20.0
owner: platform
---

# Scaling and upgrading an instance

**Audience:** an operator who runs a MemQL instance and wants to change its
size, or move it to a new engine version, without their users noticing.
**Issues:** memql#4466, memql#4467.

Related: [lifecycle-runbook.md](lifecycle-runbook.md) (in-cluster node drain) ·
[upgrade-barriers.md](upgrade-barriers.md) (local clusters) ·
[deploy-bundle-runbook.md](deploy-bundle-runbook.md) ·
[environment-parity.md](environment-parity.md)

## The one thing to understand first

"Scaling" is two different levers that people say with one word, and confusing
them is how an instance ends up unable to take a Kubernetes patch.

| | What it buys | What it costs |
|---|---|---|
| **Nodes** | capacity | **money** -- a node is a VM billed by the hour whether or not anything runs on it |
| **Replicas** | **availability** | nothing, if the nodes already have room |

A default instance runs three nodes and one replica of each mesh service. That
one replica is the problem, and not for the reason people expect: a single
replica does **not** make version rolls unsafe, but it makes **node maintenance
impossible**.

Every mesh Deployment declares a PodDisruptionBudget with `minAvailable: 1`. At
one replica that arithmetic leaves nothing to spare:

```
$ kubectl get pdb -n memql
NAME        MIN AVAILABLE   ALLOWED DISRUPTIONS
agent       1               0
cognition   1               0
identity    1               0
```

`ALLOWED DISRUPTIONS: 0` means `kubectl drain` will wait forever rather than
evict the pod -- correctly, because evicting it would take the service to zero.
Every AKS node image upgrade drains nodes. So an instance at one replica cannot
be patched without downtime, and the way you discover this is a drain that
hangs during a maintenance window.

**Raising mesh replicas to 2 is free on a default instance.** The nodes are
already paid for:

```
mesh nodes:  2 x 1900m allocatable = 3800m CPU   in use ~1200m
each mesh service requests 200m -- doubling seven of them adds ~1200m
```

So the first thing to do on any instance you intend to keep is raise replicas,
not nodes.

## Scaling

Both axes go through one capability, `deploy.azureScale`, which is reachable
three ways -- by hand, through the `scaleInstance` DSL action, or from the
VS Code Deployments panel. All three run the same script.

### Replicas (free, do this first)

```bash
scripts/deploy/azure-scale.sh \
  --subscriptionId=<sub> --resourceGroup=<rg> --clusterName=<aks> \
  --replicas=2
```

Verify the budgets actually changed -- this is the check that matters, not the
pod count:

```bash
kubectl get pdb -n memql        # ALLOWED DISRUPTIONS must now be 1
```

### Nodes (this is the one that costs money)

```bash
scripts/deploy/azure-scale.sh \
  --subscriptionId=<sub> --resourceGroup=<rg> --clusterName=<aks> \
  --nodePool=mesh --nodeCount=3
```

At the reference size (`Standard_D2as_v4`, East US, pay-as-you-go) each
additional node is roughly **$66/month**. The script applies numbers; it does
not decide them.

**Order is handled for you, and it is not symmetric.** Scaling up runs nodes
first, then replicas -- new pods need somewhere to land. Scaling down runs
replicas first, then nodes, because shrinking the cluster under pods that are
still scheduled leaves them Pending on a cluster with nowhere to put them. The
script reorders regardless of the order the flags were given.

### Before you scale up, check quota

A default instance uses 6 vCPUs and a new subscription typically allows 10, so
there is room for exactly **two** more `D2as_v4` nodes before Azure refuses:

```bash
az vm list-usage --location <region> -o table | grep -E "Total Regional|DASv4"
```

A quota increase is a support request with a lead time. Discovering the ceiling
during an incident is worse than reading it now.

## Upgrading the engine version

There are two kinds of upgrade and only one of them is inherently safe.

### Application version rolls -- already zero-downtime

Every mesh Deployment ships `maxSurge: 1, maxUnavailable: 0`. Kubernetes starts
the replacement pod, waits for it to pass its readiness probe, and only then
terminates the old one. **There is never a moment with zero ready pods**, even
at one replica -- provided a node has room for the surge pod, which is the same
headroom the scale section measured.

The roll itself is a digest change in the instance overlay, reconciled by
ArgoCD:

```bash
scripts/deploy/pin-overlay-digests.sh --overlayPath=<path> --digests='{...}'
scripts/deploy/argo-sync.sh --app=memql --revision=<branch>
```

Watch it, and watch the right thing -- readiness, not Argo's own status:

```bash
kubectl rollout status deploy/bff -n memql --timeout=5m
```

To prove it rather than assume it, poll an endpoint across the roll and count
failures:

```bash
while true; do
  curl -s -o /dev/null -w "%{http_code}\n" https://api.<domain>/healthz
  sleep 0.5
done
```

Any non-200 during a roll is a defect worth filing, not a normal cost.

**Rolling back** is the same mechanism in reverse -- the previous digests are in
git history, and `scripts/deploy/revert-overlay.sh` restores them.

### Node and Kubernetes upgrades -- NOT safe at one replica

An AKS node image or Kubernetes version upgrade cordons and drains each node in
turn. Drain respects PodDisruptionBudgets. So:

1. **Raise mesh replicas to 2 first** (free -- see above). Confirm
   `ALLOWED DISRUPTIONS: 1` on every PDB before starting.
2. Upgrade the node pool.
3. Optionally return to 1 replica afterwards, accepting that the next
   maintenance needs step 1 again.

Skipping step 1 does not fail loudly. The drain simply waits, the upgrade sits
at "in progress" for as long as you let it, and the cluster is in a half-upgraded
state the whole time.

## What is NOT zero-downtime today, stated plainly

**The database.** A default instance runs **one** CNPG instance. A Postgres
restart, a failover, or draining the node it sits on is a write outage for its
duration. No amount of care in the mesh tier changes this, because there is one
copy of the data path.

Making it highly available is a supported change -- a second CNPG instance with
automatic failover -- and it costs **one additional node (~$66/month)**, because
anti-affinity places the replica elsewhere. Until that is done, plan database
work into a window and say so, rather than describing the instance as
zero-downtime.

**Single-replica services during node maintenance**, as covered above.

## Verifying, and the trap in verifying

`Healthy` from ArgoCD is not evidence that an upgrade worked. Argo reports the
health of the resources it last managed to apply; it is entirely possible for an
Application to report `Healthy` while being `OutOfSync`, pinned to a revision
that no longer exists, and reconciling nothing. That exact combination went
unnoticed on a live instance for its whole life (memql#4463).

Check the things that cannot be faked:

```bash
kubectl get pods -n memql -o wide            # every pod Running and READY n/n
kubectl get pdb -n memql                     # ALLOWED DISRUPTIONS as expected
kubectl get application memql -n argocd \
  -o jsonpath='{.status.sync.status}{"\n"}'  # Synced, not merely Healthy
```

## Where these run from

The scale and upgrade capabilities run on the **cockpit/runner surface --
outside the target cluster**. That is structural, not stylistic: an instance
must not drive the roll that replaces its own pods, because the pod executing
the automation is one of the pods being replaced. A local instance (`make up`)
or CI is the runner; for a fleet, any instance other than the target.
