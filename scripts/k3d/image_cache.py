#!/usr/bin/env python3
"""Retire only explicitly owned local build images beyond a caller's retention.

Never prune Docker, volumes or unlabelled images. The caller holds the shared
host build lock for the entire operation. Every removal is by immutable CRI ID
after a fresh reference/ownership check; inventory failures stop the operation.
"""

import argparse
import collections
import datetime
import json
import pathlib
import re
import subprocess
import sys

CLUSTER_LABEL = "io.memql.local-build.cluster"
TYPE_LABEL = "io.memql.local-build.node-type"
IMAGE_ID = re.compile(r"sha256:[0-9a-f]{64}\Z")
NAME = re.compile(r"[a-z0-9][a-z0-9-]{0,62}\Z")
CREATED = re.compile(r"(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})\Z")


def read_json(args):
    result = subprocess.run(args, check=True, capture_output=True, text=True, timeout=30)
    return json.loads(result.stdout)


def normalized(reference):
    for prefix in ("containerd://", "docker://", "docker-pullable://"):
        if reference.startswith(prefix):
            return reference[len(prefix):]
    return reference


def requested_images(value):
    found = set()
    if isinstance(value, dict):
        for key, child in value.items():
            if key in ("containers", "initContainers", "ephemeralContainers") and isinstance(child, list):
                found.update(item["image"] for item in child if isinstance(item, dict) and isinstance(item.get("image"), str))
            found.update(requested_images(child))
    elif isinstance(value, list):
        for child in value:
            found.update(requested_images(child))
    return found


def protected_images(images, containers, desired):
    aliases = collections.defaultdict(set)
    protected = set()
    for item in images:
        image_id = item["id"]
        if not IMAGE_ID.fullmatch(image_id):
            raise ValueError("Image inventory returned an invalid identity")
        for ref in [image_id] + (item.get("repoTags") or []) + (item.get("repoDigests") or []):
            aliases[normalized(ref)].add(image_id)
        if item.get("repoTags") or item.get("repoDigests") or item.get("pinned"):
            protected.add(image_id)
    for container in containers:
        ref = normalized(container["imageRef"])
        if ref not in aliases:
            raise ValueError("A container image cannot be resolved; cleanup is stopped")
        protected.update(aliases[ref])
    for ref in desired:
        protected.update(aliases.get(normalized(ref), ()))
    return protected


def owned_image(detail, cluster):
    status = detail["status"]
    image_id = status["id"]
    if not IMAGE_ID.fullmatch(image_id):
        raise ValueError("Image details returned an invalid identity")
    spec = detail.get("info", {}).get("imageSpec", {})
    labels = spec.get("config", {}).get("Labels") or {}
    if labels.get(CLUSTER_LABEL) != cluster:
        return None
    node_type = labels.get(TYPE_LABEL, "")
    if not NAME.fullmatch(node_type):
        raise ValueError("Owned image has no valid node type; cleanup is stopped")
    stamp = CREATED.fullmatch(spec["created"])
    if stamp is None:
        raise ValueError("Owned image creation time must be RFC3339 with a timezone")
    # Python 3.9 cannot parse OCI's nanosecond fractions. Parse whole seconds
    # separately and retain the full fraction for deterministic newest-first
    # ordering rather than rounding distinct builds onto one microsecond.
    whole = datetime.datetime.fromisoformat(stamp[1] + stamp[3].replace("Z", "+00:00"))
    created = (whole, int((stamp[2] or "0").ljust(9, "0")))
    return {"id": image_id, "type": node_type, "created": created}


def expired_images(owned, keep):
    if keep < 2:
        raise ValueError("Keep at least the current and previous build")
    groups = collections.defaultdict(list)
    for item in owned:
        groups[item["type"]].append(item)
    expired = set()
    for builds in groups.values():
        builds.sort(key=lambda item: (item["created"], item["id"]), reverse=True)
        expired.update(item["id"] for item in builds[keep:])
    return expired


class Runtime:
    def __init__(self, cluster):
        self.cluster = cluster

    def nodes(self):
        output = subprocess.run(["docker", "ps", "--all", "--filter", "label=k3d.cluster=" + self.cluster,
            "--format", '{{.Names}} {{.State}} {{.Label "k3d.role"}}'],
            check=True, capture_output=True, text=True, timeout=30).stdout
        nodes = []
        for line in output.splitlines():
            name, state, role = line.split()
            if role not in ("server", "agent"):
                continue
            if state != "running" or not name.startswith("k3d-" + self.cluster + "-"):
                raise ValueError("A cluster node is unavailable; cleanup is stopped")
            nodes.append(name)
        if not nodes:
            raise ValueError("No cluster nodes found; cleanup is stopped")
        return sorted(nodes)

    def inventory(self, node):
        return read_json(["docker", "exec", node, "crictl", "images", "-o", "json"])["images"]

    def containers(self, node):
        return read_json(["docker", "exec", node, "crictl", "ps", "-a", "-o", "json"])["containers"]

    def desired(self):
        return requested_images(read_json(["kubectl", "--context", "k3d-" + self.cluster, "get",
            "pods,deployments,replicasets,statefulsets,daemonsets,jobs,cronjobs", "-A", "-o", "json"]))

    def inspect(self, node, image_id):
        return read_json(["docker", "exec", node, "crictl", "inspecti", "-o", "json", image_id])

    def remove(self, node, image_id):
        subprocess.run(["docker", "exec", node, "crictl", "rmi", image_id], check=True,
                       timeout=30, stdout=subprocess.DEVNULL)


def snapshot(runtime, node, cluster, keep, metadata):
    images = runtime.inventory(node)
    protected = protected_images(images, runtime.containers(node), runtime.desired())
    owned = []
    for image in images:
        key = (node, image["id"])
        if key not in metadata:
            detail = runtime.inspect(node, image["id"])
            if detail["status"]["id"] != image["id"]:
                raise ValueError("Image identity changed during cleanup")
            metadata[key] = owned_image(detail, cluster)
        item = metadata[key]
        if item is not None:
            owned.append(item)
    return expired_images(owned, keep) - protected


def clean(runtime, nodes, cluster, keep, apply=False, result=None):
    if result is None:
        result = {"cluster": cluster, "keepPerType": keep, "candidates": [], "removed": [], "retained": []}
    metadata = {}  # Immutable config metadata only; references are always fresh.
    # Complete every node's plan before changing any node.
    plans = {node: sorted(snapshot(runtime, node, cluster, keep, metadata)) for node in nodes}
    for node, candidates in plans.items():
        for image_id in candidates:
            entry = {"node": node, "image": image_id}
            result["candidates"].append(entry)
            if not apply:
                continue
            # A retagged image, new container, changed controller or retention
            # inventory must protect the image even after planning succeeded.
            if runtime.nodes() != sorted(nodes):
                raise ValueError("Cluster nodes changed during cleanup")
            if image_id not in snapshot(runtime, node, cluster, keep, metadata):
                result["retained"].append(entry)
                continue
            final = runtime.inspect(node, image_id)
            status = final["status"]
            owner = owned_image(final, cluster)
            if owner is None or owner["id"] != image_id or status.get("pinned") or status.get("repoTags") or status.get("repoDigests"):
                result["retained"].append(entry)
                continue
            runtime.remove(node, image_id)
            if any(image["id"] == image_id for image in runtime.inventory(node)):
                raise ValueError("The runtime did not confirm image removal")
            result["removed"].append(entry)
    return result


class Arguments(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError(message)


def main():
    parser = Arguments(description=__doc__)
    parser.add_argument("--cluster", required=True)
    parser.add_argument("--keep-per-type", type=int, default=2,
                        help="Number of newest owned builds to retain per node type (minimum 2)")
    parser.add_argument("--apply", choices=("true", "false"), default="false",
                        help="Remove reviewed candidates; default only reports the plan")
    result = {"candidates": [], "removed": [], "retained": []}
    code = 0
    message = None
    try:
        args = parser.parse_args()
        if not NAME.fullmatch(args.cluster) or not 2 <= args.keep_per_type <= 100:
            raise ValueError("Use a valid cluster name and retain between 2 and 100 builds per node type")
    except ValueError as error:
        code, message = 2, str(error)
    if not code:
        result.update(cluster=args.cluster, keepPerType=args.keep_per_type)
        try:
            # The same lock is held by Fleet CI, local builds and cleanup. A
            # child adopts only the checked inherited descriptor; a standalone
            # invocation acquires it without deleting any interrupted record.
            sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "lib"))
            from host_build_guard import host_build_guard
            with host_build_guard():
                runtime = Runtime(args.cluster)
                clean(runtime, runtime.nodes(), args.cluster, args.keep_per_type,
                      args.apply == "true", result)
        except Exception as error:
            code, message = 5, str(error)
    if message:
        print("Local image cleanup stopped: " + message, file=sys.stderr)
    print(json.dumps({"ok": code == 0, "capability": "k3d.cleanOwnedImages", "changed": bool(result["removed"]),
                      "result": result, "error": {"code": code, "message": message} if code else None}))
    return code


if __name__ == "__main__":
    sys.exit(main())
