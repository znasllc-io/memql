# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM docker.io/rancher/mirrored-library-busybox@sha256:101b4afd76732482eff9b95cae5f94bcf295e521fbec4e01b69c5421f3f3f3e5
RUN --mount=type=secret,id=github_read_token,required=true \
    test -z "${MEMQL_GITHUB_READ_TOKEN+x}" && \
    test "$(sha256sum /run/secrets/github_read_token | cut -d' ' -f1)" = "f0d7b2ce25814b757857a0a34b6b72f8d0eedb54b71814b0a6811128c170bc39"
RUN test ! -e /run/secrets/github_read_token && \
    test -z "${MEMQL_GITHUB_READ_TOKEN+x}" && \
    printf 'read-grant-verified\n' > /read-grant-proof.txt
