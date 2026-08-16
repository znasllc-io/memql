# The memQL Cloud control-plane DSL bundle, as a data-only image
# (epic memql#3852, task memql#3853).
#
# WHAT A BUNDLE IMAGE IS. Not a program. The `dsl-bundle` kustomize component
# (deploy/k8s/components/dsl-bundle) runs this image as an INIT-CONTAINER whose
# whole job is `cp -a /bundle/. /var/lib/memql/dsl/` into a shared volume; the
# engine container then reads that path as MEMQL_DSL_PATH and mounts every
# domain it finds. So the image needs a filesystem and a shell to copy with, and
# nothing else -- no Go toolchain, no engine binary, no memQL code of any kind.
#
# THAT IS THE POINT. A product-agnostic engine image plus this image is a
# running memQL Cloud control plane. Swap this image for another product's
# bundle and the same engine runs that product instead, with no rebuild. It is
# the mechanism the platform-consolidation epic's acceptance bar names ("a
# second product boots a full stack with ZERO engine-repo edits"), and memQL
# Cloud goes through it rather than around it so we find out when it breaks.
#
# BUILD (from the repository root, so the context contains dsl/):
#
#   docker build -f deploy/fleet/Dockerfile -t memql-fleet-bundle:<version> deploy/fleet
#
# The build server owns this for staging and production, exactly as it owns the
# engine images: a release is {engine version, bundle digest, client digest}
# pinned in one overlay. Do not hand-build and push a release bundle from an
# operator machine.
FROM busybox:1.37-uclibc

# Alpine or busybox rather than scratch: the init-container's command is `sh -c
# "cp -a ..."`, so the image has to carry a shell. `scratch` would make an image
# whose contents are correct and which cannot copy them anywhere.
COPY dsl/ /bundle/

# Non-root by number. There is no user database in this image to name one from,
# and the only thing that ever runs here is a copy out of a world-readable tree.
USER 65534:65534

# Never executed -- the component overrides the command with its copy. Present
# so that `docker run` on this image says what it is instead of dropping into a
# shell and implying it is something you can use.
CMD ["/bin/sh", "-c", "echo 'memQL Cloud fleet DSL bundle: data only. Mount /bundle at MEMQL_DSL_PATH.' && ls -R /bundle"]
