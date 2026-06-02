# Gate-client image: acrmemql.azurecr.io/deploy-gate (deployment-v2 #701/#712).
# The deploy-gate-check entrypoint runs the in-cluster deploy gate
# (/readyz + an authenticated service_account-JWT query). Referenced by the Argo
# Rollouts AnalysisTemplate (deploy/rollouts/analysis/deploy-gate.yaml).
#
# Build (from the repo root):
#   docker build -f cmd/deploy-gate-check/Dockerfile -t acrmemql.azurecr.io/deploy-gate:<tag> .
#   # or: az acr build --registry acrmemql --image deploy-gate:<tag> \
#   #        -f cmd/deploy-gate-check/Dockerfile .
# CI emits the resulting @sha256 digest for the release lockfile (releases/).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/deploy-gate-check ./cmd/deploy-gate-check

# Distroless static: ships curl-free; deploy-gate-check does /readyz over Go's
# net/http and the query over gRPC, so no shell/curl is needed.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/deploy-gate-check /usr/local/bin/deploy-gate-check
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/deploy-gate-check"]
