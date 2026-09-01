# Custom Domains -- Design

- **Date:** 2026-09-01
- **Status:** approved (in-session Q&A with the owner; every fork below
  records the choice that was made and why)
- **Scope:** `dsl/platform/` (the `customDomain` concept +
  queries/mutations/automation), a DNS-verification builtin, an edge
  hostname-alias resolution step (`component/edge`), two capability scripts
  under `scripts/deploy/` (bind/unbind) with action wiring and a small RBAC
  addition in `deploy/k8s/`, and the Domains panel in the OS Deployables
  app (`clients/os/`).
- **The wave this belongs to:** Epic C of three, and the last. It consumes
  exactly two things from Epic B (`site.accountId`, `account.domain` as the
  suggestion source) and nothing from Epic A beyond the site row both
  already share.
- **Follow-ups noted, not built here:** self-serve binding behind a
  request-and-approve queue (the natural v2), registrar-API integrations
  that create the client's DNS records for them, wildcard client domains,
  certificate export.

## Why

Owner's brief, condensed: an account-tied deployable should be servable at
the client's own domain -- this instance ends up hosting websites for
clients. Today any hostname that is not `<slug>.<domain>` is
cluster-owner-only and hand-certified (memql#4224: the front-door
certificate names exact hosts; the DNS-01 wildcard covers only
`*.<domain>`). C replaces the hand-certification with a guided flow: the
UI shows the exact DNS records to create, the platform **verifies** them
and says precisely which record is still wrong, and once the domain points
at the cluster the exact-host ingress and HTTP-01 certificate are
provisioned automatically -- setup guidance plus verification, end to end,
with typed statuses.

## Locked decisions

| # | Decision | Choice (owner-approved) |
|---|---|---|
| D1 | Access | **Cluster owner/admin only** in v1. The flow automates what is already owner territory rather than widening it; it matches the agency workflow (the operator drives setup for the client); certificate issuance and ingress objects are cluster-level resources with real rate limits. Widening is additive; the v2 shape is a request-and-approve queue |
| D2 | Mechanism | **Capability-script reconciliation.** The graph holds the state machine; a scheduled automation verifies; an action dispatch (the existing `component/automations/steps/action_dispatch.go` -> `deploycontrol.ParseCapabilityResult` seam) runs an idempotent script that `kubectl apply`s the objects. The engine deliberately gains NO Kubernetes client -- an in-engine client-go reconciler would be a second way to touch cluster objects beside the GitOps/script path, the exact thing environment-parity review rejects |
| D3 | Concept | `v1:platform:customDomain`, clusterOwner tier (what a cluster serves at the front door is one deployment's fact, the `packState` reasoning): `siteId` (required, `references` site), `hostname` (client FQDN), `accountId` (provenance, prefilled from the site's tie), `token`, `status`, `failureReason` (typed), `lastCheckedAt` / `verifiedAt` / `issuedAt`. Rows survive removal -- the history is the audit |
| D4 | Verification | Two DNS checks, both required before anything issues: the **ownership token** (`TXT _memql-verify.<hostname> = <token>`; storable plaintext, DNS publishes it anyway) and the **pointing check** (CNAME to the cluster's edge host for a subdomain; an A record to the resolved ingress address for an apex -- discovered by resolving the cluster's own edge host, never configured a second time). Never issue before both pass: Let's Encrypt rate limits are real, and a domain that merely points at the cluster must not be claimable by the wrong person |
| D5 | The loop | A scheduled automation walks non-terminal rows; a Go builtin does the lookups. Every miss writes `lastCheckedAt` plus a typed `failureReason` (`dns_token_missing`, `dns_not_pointing`), so the panel always names exactly which record is still wrong. Retries ride the schedule; there is no manual re-check button to hammer |
| D6 | Provisioning | `scripts/deploy/bind-custom-domain.sh` under the capability-script contract (idempotent, `--flag=value` params, one JSON envelope on stdout, honest exit codes; the standing `capability_contract_test.go` gates it automatically): exact-host Ingress + cert-manager HTTP-01 Certificate. `live` only when a follow-up check sees the Certificate Ready. Unbind is the mirror script -> `removing` -> `removed`. The acting node's ServiceAccount gains ingress + certificate write RBAC in `deploy/k8s` base |
| D7 | Parity | The ACME issuer is a VALUE. An overlay declaring none (local k3d) gets the typed refusal `no_acme_issuer` at issuance rather than a pretend success -- same flow shape everywhere, honest about what the target can do |
| D8 | Edge | Resolution gains one alias step: the request `Host` resolves against `site.hostname` OR a **live** `customDomain` row's hostname -> its site. Serving, per-site CSP, runtime config and `apiProxy` are identical on the client origin -- and same-origin API means no CORS story |
| D9 | Surface | A **Domains panel** on the deployable detail in the OS (admin-gated presentation over the server law): the domain list with live statuses, the add flow with `hostname` prefilled from the tied account's `domain` (Epic B's D9 seam), copyable records in the guidance panel, in-surface confirm on remove |
| D10 | Guardrails | `hostname` must not be under the cluster's own domain (slug territory) nor collide with front-door hosts; max domains per site is env-tunable; verification cadence is the schedule |
| D11 | Delivery | Three tasks, **two PRs**: PR 1 = engine (concept/loop/edge alias + scripts/RBAC/action wiring), PR 2 = OS (the Domains panel). Stated on the epic and every task |

## A. What exists today (the ground this builds on)

- memql#4224's rule: the cloud front-door certificate names exact hosts;
  ONE wildcard dnsName fails a whole HTTP-01 order; the DNS-01 wildcard
  (memql#4347) covers `*.<domain>` only. A client's own domain was always
  going to need an exact-host Ingress + Certificate of its own -- C
  automates exactly that pair.
- The action-dispatch seam is live: automations dispatch actions whose
  backends are capability scripts, results parsed by
  `deploycontrol.ParseCapabilityResult`. The script contract and its
  conformance test suite exist (`scripts/lib/capability.sh`,
  `capability_contract_test.go`).
- The engine has **no client-go anywhere** -- D2 keeps it that way.
- The edge resolves `Host` -> `v1:platform:site` by hostname; the alias
  step extends that one lookup.
- HTTP-01 per exact host works precisely because the DNS now points at the
  cluster -- the standard white-label hosting flow.

## B. The flow

1. **Create** (admin, from the site detail): row lands `pending_dns` with a
   minted token; the guidance panel renders the exact records (D4),
   copyable.
2. **Verify**: the scheduled automation + builtin run the two checks; typed
   misses update the row (D5); both pass -> `verifying` becomes `issuing`
   and the bind action dispatches.
3. **Provision**: the script applies Ingress + Certificate; the follow-up
   readiness check flips `issuing` -> `live` (D6). Refusals land as typed
   `failureReason` (`no_acme_issuer`, `issuance_failed`) with the script's
   envelope detail.
4. **Remove**: unbind action -> mirror script -> `removed`; the row stays.

Statuses broadcast, so the panel ticks live as checks run.

## C. Testing

- The DNS builtin against a fake resolver (token present/absent, pointing
  right/wrong, apex vs subdomain) -- pure Go.
- State-machine automation tests; never-issue-before-both-checks proven by
  a test that passes one check and asserts no dispatch.
- The scripts under the standing capability-contract gate; envelope
  round-trip through `ParseCapabilityResult`.
- Edge alias resolution db-gated (live domain resolves; `pending` does
  not; a removed one stops resolving).
- Cluster-e2e to the dispatch boundary -- no live ACME in CI.

## D. Out of scope, and neighbors

Out of scope: self-serve binding (v2 request/approve queue); registrar API
integrations; wildcard client domains; certificate export; local-cluster
ACME.

Neighbors: Epic A (`2026-09-01-packages-and-deployables-design.md`, epic
memql#4794) and Epic B (`2026-09-01-accounts-app-design.md`) in this
directory; the front-door standard (`docs/public/operate/front-door.md`)
gains a section on the custom-domain regime when this ships.
