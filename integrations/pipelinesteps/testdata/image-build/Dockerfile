FROM golang:1.27.1@sha256:512690a5660563b57d37ecc31129e7f136e831db2aed24a1dbeb8ad7380dc0fa AS probe
ENV CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=512MiB
COPY probe.go /probe.go
RUN go build -p=2 -trimpath -ldflags="-s -w -buildid=" -o /probe /probe.go
FROM docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5
ARG PRIVATE_LISTENER
COPY --from=probe /probe /probe
RUN /probe "$PRIVATE_LISTENER" > /proof.json && cat /proof.json
