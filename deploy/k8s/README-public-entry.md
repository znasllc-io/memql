# Public HTTPS entry (staging)

`public-entry.yaml` (ClusterIssuer + Ingresses) and `copresent.yaml` depend on
two cluster add-ons that are installed **once, out-of-band** (they're upstream
manifests, not part of the kustomize tree):

## 1. ingress-nginx + a pinned static IP

```bash
ING=controller-v1.15.1   # pin a known version
kubectl apply -f "https://raw.githubusercontent.com/kubernetes/ingress-nginx/${ING}/deploy/static/provider/cloud/deploy.yaml"
```

The controller Service gets an Azure LoadBalancer IP. Pin it so it survives a
controller re-create (otherwise DNS breaks). AKS auto-creates a Standard (static)
IP named `kubernetes-<hash>` in the node resource group; bind it as BYO so AKS
stops managing/deleting its lifecycle:

```bash
NODE_RG=$(az aks show -g rg-memql-staging -n aks-memql-staging --query nodeResourceGroup -o tsv)
PIP=$(az network public-ip list -g "$NODE_RG" --query "[?ipAddress=='<LB_IP>'].name" -o tsv)
kubectl annotate svc ingress-nginx-controller -n ingress-nginx \
  "service.beta.kubernetes.io/azure-pip-name=${PIP}" \
  "service.beta.kubernetes.io/azure-load-balancer-resource-group=${NODE_RG}" --overwrite
```

Point the GoDaddy A records `app.staging` + `identity.staging` at the LB IP.

## 2. cert-manager (Let's Encrypt)

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml
kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
```

The `letsencrypt-prod` ClusterIssuer in `public-entry.yaml` then issues the
`app-staging-copresent-tls` + `identity-staging-copresent-tls` certs via HTTP-01
once the A records resolve. (Production-trusted certs even though the cluster is
"staging" -- a Let's Encrypt *staging* issuer would be browser-untrusted and
block the mic/voice secure-context.)

## 3. Internal TLS secrets

`deploy/k8s/tls/gen-internal-ca.sh` creates the `identity-tls` + `memql-ca`
secrets (the self-signed cluster CA for the node mesh -- distinct from the
public Let's Encrypt certs above).
