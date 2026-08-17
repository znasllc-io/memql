# The operator credential (`MEMQL_OPERATOR_KEY`)

`Authorization: Operator <key>` admits a gRPC stream as a **synthetic cluster
owner** -- `role: owner`, subject `cluster:operator`, and per-row authz checks
then hit the `IsClusterOwner()` bypass. It exists so out-of-band tooling can
talk to a cluster **before any user has been provisioned**, which is the one
moment no other credential can cover.

It is the highest-privilege credential in the system. Treat it accordingly.

Interceptor: `component/grpc/operator_stream_interceptor.go`, wired on all
three interceptor paths in `app/transport.go`.

---

## Two secrets, two jobs

| | `MEMQL_MASTER_KEY` | `MEMQL_OPERATOR_KEY` |
|---|---|---|
| Job | **Decrypts** DSL-resolved secrets at rest | **Authenticates** operator tooling to the cluster |
| Used by | `component/secret`, `secretResolve` | `component/grpc/operator_stream_interceptor.go` |
| Needed by | any node that opens the envelope | any node that should accept operator streams |
| If unset | the node cannot decrypt its config | operator streams are refused (safe) |

**These were one value until memql#3519.** The interceptor read
`MEMQL_MASTER_KEY`, justified by: *"it is already the operator credential of
the cluster (anyone who can read /var/lib/memql secrets from the host can
produce it), so layering authentication on top of it adds no new secret to
rotate."*

That reasoning is about **host filesystem access**, and it stopped describing
where the value actually lived:

- the envelope's sealing CLI **defaulted to `--sync-shell=true`** and appended
  `export MEMQL_MASTER_KEY=...` to `~/.bashrc` and `~/.zshrc`, preserving each
  file's existing permission bits -- so on a typical `0644` dotfile the key was
  readable by every local account, and travelled into dotfile backups, sync
  tools and screen shares.
- ESO delivers it from Key Vault into the `memql-secrets` k8s Secret on
  **staging and production** (`deploy/external-secrets/externalsecret-memql.yaml`),
  so the operator path is live there, not only on a laptop.

Composed, those two facts meant a value in a world-readable dotfile was a
cluster-owner bearer token against production, over the network. The split
costs one more secret to rotate. That is exactly the cost the old comment
declined to pay -- but the trade had been priced against nodes, not laptops.

The repo's own agent-safety classifier already scored a write to `~/.bashrc` as
`TierHigh`, *"near-certain persistence / privilege-escalation signal"*
(`component/safety/rules_path.go`). The installer was doing it by default with
the highest-value credential in the system. `--sync-shell` now defaults to
**false**, so the first-party tool no longer trips its own rule.

---

## Generating and seeding

```bash
openssl rand -hex 32
```

The envelope's sealing CLI deliberately did **not** mint this key. It owned the envelope;
coupling the two again -- even only by generating both in one place -- is how
they became one value the first time.

**Local.** Export it in the shell you run operator tooling from, or keep it in
a password manager and paste per session. Do not put it in a dotfile.

**Staging / production.** It rides the same path as the other cluster secrets:

```bash
az keyvault secret set \
  --vault-name kv-memql-staging \
  --name memql-operator-key \
  --value "$(openssl rand -hex 32)"
```

ESO then reconciles it into `memql-secrets` as `MEMQL_OPERATOR_KEY`.

> **ORDERING.** Create the Key Vault entry **before** the ExternalSecret that
> references it reconciles. ESO fails the whole ExternalSecret when a
> `remoteRef` cannot be resolved, so a missing entry does not degrade to "one
> key absent" -- it can stall the sync for every key in that object.

---

## What "no fallback" means for an upgrade

There is deliberately no fallback to `MEMQL_MASTER_KEY`. A fallback would keep
the master key working as an authentication credential, which is the entire
defect.

So on the first deploy carrying this change, **a cluster that has not been
given `MEMQL_OPERATOR_KEY` refuses every operator stream.** The failure mode is:

```
operator auth: rejected -- MEMQL_OPERATOR_KEY not configured on this node
```

and the client sees `Unauthenticated`. That is `scripts/secrets health`,
`make secrets-seed` and `scripts/cluster/rolling-drain` failing to
authenticate -- **not** a data-path outage and not an open door. The
interceptor fails closed; ordinary user and service-account traffic is
untouched because it never used this scheme.

Seed the key and the tooling works again. There is no window in which the old
credential still functions, which is the point.

A configured value shorter than 32 characters is refused exactly like an unset
one. That is a floor on operator error rather than a cryptographic parameter:
`MEMQL_OPERATOR_KEY=test` set in a hurry would otherwise be a cluster-owner
credential on a production ingress.

---

## Rotation

Rotating the operator key is now **cheap and independent** -- it invalidates
operator tooling sessions and nothing else. This is the main practical dividend
of the split; previously the same act re-sealed the envelope.

1. Generate a new value (`openssl rand -hex 32`).
2. Update the Key Vault entry (`memql-operator-key`). ESO reconciles on its
   refresh interval; force it with `kubectl annotate externalsecret memql-secrets
   force-sync=$(date +%s) -n memql --overwrite`.
3. Restart or wait out the pods so the new env value is in process. The
   interceptor reads the variable per request, but the pod's environment is
   fixed at start, so a rollout is what actually swaps it.
4. Update whatever the operator exports locally.

Because there is no key ring here, step 3 is a hard cutover: between the Key
Vault update and the rollout completing, tooling using the new value is
rejected and tooling using the old value still works on un-restarted pods. Do
it deliberately rather than mid-incident.

### Rotating the MASTER key (the expensive one)

Worth stating because the two used to be conflated. `MEMQL_MASTER_KEY`
**decrypts and never authenticates**; rotating it means re-encrypting every
secret sealed under it.

> **This procedure changed with epic memql#3958.** It used to be "re-seal the
> genesis envelope and move `memql-genesis-b64` and `memql-master-key` in Key
> Vault together, because the envelope and the key that opens it are one unit".
> There is no envelope any more -- config has one delivery path, the
> `memql-secrets` Secret -- so the coupled move and the "a node holding the new
> envelope and the old key cannot boot" hazard are both gone with it.

What remains under the key is `v1:platform:globalSecret` rows
(`component/secret`'s `Encrypt` / `Decrypt`). Rotating it is therefore a data
migration rather than a config change:

1. Re-encrypt every affected row under the new key.
2. Update `memql-master-key` in Key Vault.
3. Roll the nodes that decrypt.

A node holding the new key cannot read rows sealed under the old one, so
sequence it as "re-encrypt, then swap, then restart".

---

## Disabling the operator path

Leave `MEMQL_OPERATOR_KEY` unset. The interceptor still parses the header and
always rejects, so the scheme is inert. This is the supported way to run a
cluster with no operator credential at all -- appropriate anywhere the
bootstrap-before-first-user problem does not apply.
