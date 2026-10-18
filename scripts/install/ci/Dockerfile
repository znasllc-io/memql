# syntax=docker/dockerfile:1
# Local installer fixture only. Never published or deployed to a cloud cluster.
# The inner daemon owns all k3d containers, networks and images for one case.
FROM docker.io/library/docker:29.8.2-dind@sha256:1e08cdb63405ca788aea94ef35b792d1e299607c667d33d7bcbcae1fd2611ced AS daemon
FROM ghcr.io/znasllc-io/memql-toolchain@sha256:a62b53491fbd9cfd4bbfdf9612bfb1776d9fc9d42aab7ceceac3a0a18a17a8bb
USER root
RUN apt-get update && apt-get install -y --no-install-recommends \
      sudo iptables iproute2 libnss3-tools util-linux pigz xz-utils && \
    rm -rf /var/lib/apt/lists/* && \
    groupadd --system docker && usermod -aG docker memql && \
    printf 'memql ALL=(ALL) NOPASSWD: ALL\n' > /etc/sudoers.d/memql-installer-fixture && \
    chmod 0440 /etc/sudoers.d/memql-installer-fixture && \
    mkdir -p /evidence && chown memql:memql /evidence && chmod 0700 /evidence && \
    touch /run/memql-install-fixture
COPY --from=daemon /usr/local/bin/dockerd /usr/local/bin/dockerd
COPY --from=daemon /usr/local/bin/ctr /usr/local/bin/ctr
COPY --from=daemon /usr/local/bin/containerd-shim-runc-v2 /usr/local/bin/containerd-shim-runc-v2
COPY --from=daemon /usr/local/bin/runc /usr/local/bin/runc
COPY --from=daemon /usr/local/bin/docker-init /usr/local/bin/docker-init
COPY --from=daemon /usr/local/bin/docker-proxy /usr/local/bin/docker-proxy
COPY --from=daemon /usr/local/bin/dind /usr/local/bin/dind
COPY entrypoint.sh /usr/local/bin/memql-install-fixture
ENTRYPOINT ["/usr/local/bin/memql-install-fixture"]
