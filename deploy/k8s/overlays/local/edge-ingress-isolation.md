# Local edge HTTP ingress isolation

The local overlay declares `edge-http-from-local-ingress`. Only pods in
`kube-system` with both `app.kubernetes.io/name=traefik` and
`app.kubernetes.io/instance=traefik-kube-system` may reach edge TCP 8085.
Those selectors were read from the running local Traefik Deployment on
2026-11-27 UTC; it does not use host networking. The namespace and pod selectors
are in the same peer so they combine with AND. TCP 50051 and 50062 preserve
existing mesh reachability; egress and Kubernetes node-origin health probes are
unchanged. The policy adds no public route or host port.

This closes an ordinary untrusted pod's direct **edge HTTP** path once the local
policy engine demonstrably enforces it. The native visitor helper still takes
the right-most valid `X-Forwarded-For` entry. Traefik sanitizes untrusted
forwarding headers and appends its observed peer on the measured local path.
A ClusterIP or an accepted NetworkPolicy object alone proves neither boundary.
NetworkPolicies are additive; an additional matching allow policy needs review.

The public API ingress deliberately withholds `/calls/*`; its catch-all goes to
the BFF gRPC listener, not HTTP. The render regression also refuses `/memql` or
other HTTP prefixes which would expose native Calls aliases. The native handler
still validates site ownership and Callkey authority. Its site/owner headers are
pointers, not a private edge credential: **in-cluster BFF HTTP remains a trusted
caller boundary**. This edge-only change does not isolate BFF HTTP from other
pods, change runtime authentication/RBAC, or claim all internal rate-limit bypasses
are impossible. Privileged node traffic, host-network workloads and Kubernetes
port-forward authority are also outside this pod policy's isolation claim.

## Verification after the approved GitOps sync

Do not apply this manifest separately or bypass a blocked GitOps approval. Render
tests prove the declared shape, not live enforcement. After sync, record the
Application revision, rendered policy/Deployment selectors and the current
Traefik/edge pod identities, without collecting secrets. In the exclusive local
acceptance lane:

1. Confirm normal Callkey HTTPS, API health, edge readiness and mesh health still
   work. Check every matching policy in `memql`; none may broaden edge HTTP.
2. Create one disposable probe pod in `memql` with an approved existing local
   image containing a bounded HTTP client. Give it no ingress labels, no service
   account token, no host network and no elevated privileges. From it, try both
   `edge.memql.svc:8085/healthz` and each edge pod IP on TCP 8085 with a short
   timeout. All must be unreachable. A normal ingress request is the positive
   control; distinguish policy denial from a broken/non-listening backend.
   Also try a probe bearing the Traefik pod labels in `memql`: the namespace
   selector must still deny it. Delete only the disposable probes in `finally`.
3. Let the local address's unlock bucket recover, then send sixteen bounded
   invalid-key unlock requests through `https://callkey.memql.localhost`, two
   for each variant below. Use no account key or session cookie. The first ten
   consume the same per-address bucket; subsequent requests must receive the
   native rate refusal regardless of forged values. If unrelated traffic has
   consumed the shared address bucket, report that run as inconclusive and retry
   after recovery. Save only status, fixed refusal code and retry timing.
4. Check public `api.memql.localhost` root and `/memql`/`/_memql` Calls aliases
   cannot reach the native HTTP Calls handler, including forged site/owner and
   forwarding headers. No valid Callkey credential is needed for these route
   checks. Preserve the existing exact-origin acceptance of the intended route.

The eight forwarding variants are: no forwarding headers; one synthetic IPv4
`X-Forwarded-For`; a comma-separated forged chain; duplicate XFF header lines;
an IPv4-mapped IPv6 XFF value; forged `X-Real-IP` plus `CF-Connecting-IP`;
RFC `Forwarded`; and conflicting forwarding headers with an invalid/trailing
XFF entry. Use documentation-range addresses only. Header-integrity proof does
not prove distinct Internet clients get distinct keys when the local host or
another proxy performs SNAT.

Cloud/CDN source preservation, the production trusted-proxy chain, cloud network
policy enforcement and any source-address ban guarantee remain separate
qualification. This local policy does not configure those environments.
