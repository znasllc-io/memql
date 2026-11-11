# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.4@sha256:08bcb99a38f85a265f9a93d1d3323e2440eec333380f3ea26ee0dec9264bb41c AS probe
ENV CGO_ENABLED=0 GOMAXPROCS=2 GOMEMLIMIT=512MiB
COPY probe.go /probe.go
RUN go build -p=2 -trimpath -ldflags="-s -w -buildid=" -o /probe /probe.go
FROM docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5
ARG PRIVATE_LISTENER
COPY --from=probe /probe /probe
RUN /probe "$PRIVATE_LISTENER" > /proof.json && cat /proof.json
