# memQL monitoring + alerting (memql#1523)

Observability + alerting for the failure classes that were silent on
2026-06-16: a ~50% auth-reject storm and a paused, unpromoted bff
Rollout -- both WARN-log-only, discovered by a user rather than a page.

## What this adds

App-side metrics (emitted by `component/metrics`, served at `GET /metrics`
on the http port 8085 of every node):

| Metric | Type | Meaning |
|--------|------|---------|
| `memql_auth_rejects_total{surface,reason,code}` | counter | Every auth/authz reject. `surface` ∈ grpc/http/node; `reason` ∈ unknown_kid, invalid_token, missing_token, token_revoked, revocation_check_error, wrong_class, missing_binding; `code` ∈ Unauthenticated/PermissionDenied. |
| `memql_jwks_keyset_keys` | gauge | Number of signing keys this process serves (identity) or trusts (verifier). |
| `memql_jwks_keyset_fingerprint` | gauge | Stable numeric fingerprint over the sorted kid set. Identical across coherent replicas; divergence = incoherence. |

Alert rules (prometheus-operator CRDs):

- `prometheusrule-auth.yaml`
  - **MemqlAuthRejectRateHigh** -- total reject rate > 1/s for 5m.
  - **MemqlAuthRejectUnknownKid** -- unknown-kid rejects > 0.05/s for 5m (signing-key skew).
  - **MemqlJWKSIncoherent** -- identity replicas' `memql_jwks_keyset_fingerprint` diverge for 10m.
  - **MemqlJWKSEmpty** -- an identity replica serves 0 keys for 5m.
- `prometheusrule-rollouts.yaml`
  - **MemqlRolloutPausedTooLong** -- a Rollout sits `phase=Paused` (BlueGreenPause / unpromoted) > 15m.
  - **MemqlRolloutDegraded** -- a Rollout is `phase=Degraded` > 5m.

Scrape config:

- `podmonitor.yaml` -- scrapes each memQL node pod's `/metrics` once.
- `servicemonitor-argo-rollouts.yaml` -- OPTIONAL; scrapes the Argo
  Rollouts controller's `rollout_info` metric (only if not already scraped).

## Assumed infrastructure

memQL does **not** ship a monitoring stack. These manifests assume a
**prometheus-operator / kube-prometheus-stack** is already installed in a
`monitoring` namespace -- the SAME assumption the existing deploy-gate SLO
analysis already makes (`deploy/rollouts/analysis/deploy-gate.yaml`
references `http://prometheus-operated.monitoring:9090`). Specifically:

1. The PodMonitor / PrometheusRule / ServiceMonitor CRDs exist.
2. The Prometheus CR's `podMonitorSelector` / `ruleSelector` /
   `serviceMonitorSelector` select these objects. They carry
   `release: kube-prometheus-stack` -- **edit this label to match your
   stack's release name**, or remove it if your operator selects all
   (empty selector).
3. Alertmanager is wired for routing `severity: critical|warning`.
4. For the Rollout alerts, the Argo Rollouts controller
   (`deploy/rollouts/install`, v1.7.2, `argo-rollouts` namespace) is
   scraped (apply `servicemonitor-argo-rollouts.yaml` if not).

If no operator is present, the app-side metrics still emit on `/metrics`
and can be scraped by any Prometheus with a static/relabel scrape job
pointing at the memQL Services on port 8085, path `/metrics`; translate
the alert exprs into a plain `rules.yaml`.

## Apply

```bash
kubectl apply -k deploy/k8s/monitoring
# optional, only if the rollouts controller isn't already scraped:
kubectl apply -f deploy/k8s/monitoring/servicemonitor-argo-rollouts.yaml
```

## Notes

- **`/metrics` is unauthenticated** (like `/healthz` / `/livez` / the JWKS
  feed) so an in-cluster scrape -- which cannot present a bearer token --
  can read it. The exposed data is non-sensitive: aggregate reject counts
  and a non-reversible fingerprint of the (already-public) JWKS kid set.
  Note the `staging-identity` Ingress routes `/` to identity:8085, so
  `/metrics` is reachable on the identity host; block it at the ingress if
  you want it strictly in-cluster (out of scope here -- no deploy-flow
  changes per memql#1523).
- **Scheme.** The PodMonitor scrapes `http` exactly as the k8s readiness
  probe hits `/healthz` over plain HTTP on 8085. If you enable internal
  TLS on 8085, set `scheme: https` + `tlsConfig` on the PodMonitor.
- **TLS / metric port.** All node Deployments name the 8085 container port
  `http`; the PodMonitor keys off that name.
