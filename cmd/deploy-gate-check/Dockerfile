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
FROM golang:1.26.4@sha256:68cb6d68bed024785b69195b89af7ac7a444f27791435f98647edff595aa0479 AS build
WORKDIR /src
COPY go.mod go.sum ./
# The `wire` tier is a set of NESTED modules the root go.mod `replace`s by
# relative path (memql#3240), and `go mod download` resolves those replaces --
# so their manifests must land in this layer, before `COPY . .`. Add a line per
# module as each tier lands (#3241..#3244).
COPY component/bus/gen/go.mod component/bus/gen/go.sum ./component/bus/gen/
COPY component/grpc/gen/go.mod component/grpc/gen/go.sum ./component/grpc/gen/
COPY component/node/gen/go.mod component/node/gen/go.sum ./component/node/gen/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/deploy-gate-check ./cmd/deploy-gate-check

# Distroless static: ships curl-free; deploy-gate-check does /readyz over Go's
# net/http and the query over gRPC, so no shell/curl is needed.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639
COPY --from=build /out/deploy-gate-check /usr/local/bin/deploy-gate-check
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/deploy-gate-check"]
