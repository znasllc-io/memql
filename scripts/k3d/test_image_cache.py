import copy
import importlib.util
import pathlib
import subprocess
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("image_cache", pathlib.Path(__file__).with_name("image_cache.py"))
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


def identity(number):
    return "sha256:" + f"{number:064x}"


def image(number, node_type="bff", cluster="fixture", **status):
    return {"status": {"id": identity(number), "repoTags": [], "repoDigests": [], "pinned": False, **status},
            "info": {"imageSpec": {"created": f"2026-01-{number:02d}T00:00:00Z", "config": {
                "Labels": {cache.CLUSTER_LABEL: cluster, cache.TYPE_LABEL: node_type}}}}}


class Runtime:
    def __init__(self, images):
        self.images = {item["status"]["id"]: copy.deepcopy(item) for item in images}
        self.used = []
        self.wanted = set()
        self.removed = []
        self.inventory_calls = 0
        self.on_inventory = lambda: None
        self.on_inspect = lambda image_id: None
        self.node_names = ["k3d-fixture-server-0"]

    def nodes(self):
        return self.node_names

    def inventory(self, node):
        self.inventory_calls += 1
        self.on_inventory()
        return [copy.deepcopy(item["status"]) for item in self.images.values()]

    def containers(self, node):
        return copy.deepcopy(self.used)

    def desired(self):
        return self.wanted

    def inspect(self, node, image_id):
        self.on_inspect(image_id)
        return copy.deepcopy(self.images[image_id])

    def remove(self, node, image_id):
        self.removed.append(image_id)
        del self.images[image_id]


class ImageCacheTest(unittest.TestCase):
    def run_cleanup(self, runtime, apply=True):
        return cache.clean(runtime, ["k3d-fixture-server-0"], "fixture", 2, apply)

    def test_preserves_current_recovery_desired_and_unowned_images(self):
        runtime = Runtime([image(n) for n in range(1, 10)])
        runtime.images[identity(1)]["status"]["repoTags"] = ["memql-bff:recovery"]
        runtime.images[identity(2)]["status"]["repoDigests"] = ["memql-bff@" + identity(2)]
        runtime.images[identity(3)]["status"]["pinned"] = True
        runtime.used = [{"imageRef": "containerd://" + identity(4)}]
        runtime.wanted = {identity(5)}
        runtime.images[identity(6)]["info"]["imageSpec"]["config"]["Labels"] = {}
        # Only 7 expires: 8 + 9 are the retained two builds. Older protected images
        # do not become disposable merely because they are outside that pair.
        result = self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [identity(7)])
        self.assertEqual(len(result["removed"]), 1)

    def test_retention_is_separate_per_node_type_and_cluster(self):
        runtime = Runtime([image(1), image(2), image(3), image(4, "agent"), image(5, "agent"),
                           image(6, "agent"), image(7, cluster="another")])
        self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [identity(1), identity(4)])
        self.assertIn(identity(7), runtime.images)

    def test_dry_run_never_deletes(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        result = self.run_cleanup(runtime, apply=False)
        self.assertEqual(len(result["candidates"]), 1)
        self.assertEqual(runtime.removed, [])

    def test_retag_after_plan_protects_image(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        def retag():
            if runtime.inventory_calls == 2:
                runtime.images[identity(1)]["status"]["repoTags"] = ["memql-bff:keep"]
        runtime.on_inventory = retag
        result = self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [])
        self.assertEqual(len(result["retained"]), 1)

    def test_new_container_after_plan_protects_image(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        def create_container():
            if runtime.inventory_calls == 2:
                runtime.used = [{"imageRef": identity(1)}]
        runtime.on_inventory = create_container
        self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [])

    def test_final_pin_or_ownership_change_protects_image(self):
        for change in ("pin", "owner"):
            with self.subTest(change=change):
                runtime = Runtime([image(n) for n in range(1, 4)])
                def protect(image_id):
                    if runtime.inventory_calls > 1 and image_id == identity(1):
                        if change == "pin":
                            runtime.images[image_id]["status"]["pinned"] = True
                        else:
                            runtime.images[image_id]["info"]["imageSpec"]["config"]["Labels"] = {}
                runtime.on_inspect = protect
                self.run_cleanup(runtime)
                self.assertEqual(runtime.removed, [])

    def test_unresolved_container_stops_before_removal(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        runtime.used = [{"imageRef": identity(99)}]
        with self.assertRaisesRegex(ValueError, "cannot be resolved"):
            self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [])

    def test_read_failure_or_node_change_stops_before_removal(self):
        for failure in ("read", "nodes"):
            with self.subTest(failure=failure):
                runtime = Runtime([image(n) for n in range(1, 4)])
                if failure == "nodes":
                    runtime.node_names.append("k3d-fixture-agent-0")
                else:
                    def failed_read():
                        if runtime.inventory_calls > 1:
                            raise OSError("inventory unavailable")
                    runtime.on_inventory = failed_read
                with self.assertRaises((ValueError, OSError)):
                    self.run_cleanup(runtime)
                self.assertEqual(runtime.removed, [])

    def test_invalid_owned_metadata_stops_before_removal(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        runtime.images[identity(2)]["info"]["imageSpec"]["created"] = "unknown"
        with self.assertRaises(ValueError):
            self.run_cleanup(runtime)
        self.assertEqual(runtime.removed, [])

    def test_requires_two_retained_builds(self):
        with self.assertRaisesRegex(ValueError, "current and previous"):
            cache.expired_images([], 1)

    def test_oci_nanoseconds_parse_and_keep_precise_order(self):
        builds = []
        for number, fraction in enumerate(("1", "1234567", "12345678", "123456789"), 1):
            item = image(5 - number)
            item["info"]["imageSpec"]["created"] = "2026-10-23T16:04:59." + fraction + "+00:00"
            builds.append(cache.owned_image(item, "fixture"))
        self.assertEqual(cache.expired_images(builds, 2), {identity(4), identity(3)})
        equivalent = image(5)
        equivalent["info"]["imageSpec"]["created"] = "2026-10-23T09:04:59.123456789-07:00"
        self.assertEqual(cache.owned_image(equivalent, "fixture")["created"], builds[-1]["created"])

    def test_collects_controller_init_and_ephemeral_references(self):
        tree = {"items": [{"spec": {"template": {"spec": {
            "containers": [{"image": "current"}], "initContainers": [{"image": "initializer"}],
            "ephemeralContainers": [{"image": "debugger"}]}}}}]}
        self.assertEqual(cache.requested_images(tree), {"current", "initializer", "debugger"})

    def test_host_images_keep_cluster_references_after_host_tag_moves(self):
        runtime = Runtime([image(n) for n in range(1, 7)])
        runtime.node_names = [cache.HOST_STORE]
        cluster = Runtime([image(n) for n in range(1, 7)])
        cluster.used = [{"imageRef": identity(1)}]
        cluster.wanted = {identity(2)}
        cluster.images[identity(3)]["status"]["repoTags"] = ["memql-bff:recovery"]
        # Exercise the real cross-store protection, not a canned protected set.
        adapter = cache.Runtime("fixture")
        adapter.cluster_nodes = cluster.nodes
        adapter.inventory, adapter.containers, adapter.desired = cluster.inventory, cluster.containers, cluster.desired
        runtime.external_protected = adapter.external_protected
        cache.clean(runtime, [cache.HOST_STORE], "fixture", 2, True)
        self.assertEqual(runtime.removed, [identity(4)])

    def test_host_reference_is_refreshed_after_plan(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        runtime.node_names = [cache.HOST_STORE]
        runtime.external_protected = lambda node: {identity(1)} if runtime.inventory_calls > 1 else set()
        result = cache.clean(runtime, [cache.HOST_STORE], "fixture", 2, True)
        self.assertEqual(runtime.removed, [])
        self.assertEqual(len(result["retained"]), 1)

    def test_all_store_plans_must_succeed_before_any_removal(self):
        runtime = Runtime([image(n) for n in range(1, 4)])
        real_inventory = runtime.inventory
        def inventory(node):
            if node == cache.HOST_STORE:
                raise OSError("host Docker unavailable")
            return real_inventory(node)
        runtime.inventory = inventory
        with self.assertRaises(OSError):
            cache.clean(runtime, ["k3d-fixture-server-0", cache.HOST_STORE], "fixture", 2, True)
        self.assertEqual(runtime.removed, [])

    def test_docker_adapter_lists_all_containers_and_deletes_only_exact_id(self):
        container_id = "f" * 64
        docker_image = {"Id": identity(1), "Created": "2026-01-01T00:00:00Z",
                        "RepoTags": ["memql-bff:local"], "RepoDigests": [],
                        "Config": {"Labels": {cache.CLUSTER_LABEL: "fixture", cache.TYPE_LABEL: "bff"}}}
        runtime = cache.Runtime("fixture")
        responses = [subprocess.CompletedProcess([], 0, identity(1) + "\n"),
                     subprocess.CompletedProcess([], 0, container_id + "\n"),
                     subprocess.CompletedProcess([], 0, "")]
        with mock.patch.object(cache.subprocess, "run", side_effect=responses) as run, \
             mock.patch.object(cache, "read_json", side_effect=[[docker_image], [{"Id": container_id, "Image": identity(1)}]]) as read:
            self.assertEqual(runtime.inventory(cache.HOST_STORE)[0]["repoTags"], ["memql-bff:local"])
            self.assertEqual(runtime.containers(cache.HOST_STORE), [{"imageRef": identity(1)}])
            runtime.remove(cache.HOST_STORE, identity(2))
        self.assertEqual(run.call_args_list[0].args[0], ["docker", "image", "ls", "--all", "--quiet", "--no-trunc"])
        self.assertEqual(run.call_args_list[1].args[0], ["docker", "ps", "--all", "--quiet", "--no-trunc"])
        self.assertEqual(run.call_args_list[2].args[0], ["docker", "image", "rm", "--no-prune", identity(2)])
        self.assertEqual(read.call_args_list[1].args[0], ["docker", "container", "inspect", container_id])
        self.assertEqual(cache.owned_image(runtime.docker_detail(docker_image), "fixture")["id"], identity(1))

    def test_docker_inspection_cannot_silently_drop_an_inventory_item(self):
        with mock.patch.object(cache.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, identity(1) + "\n")), \
             mock.patch.object(cache, "read_json", return_value=[]):
            with self.assertRaisesRegex(ValueError, "inventory changed"):
                cache.Runtime.docker_objects("image")

    def test_unowned_docker_index_without_creation_metadata_is_retained(self):
        index = {"Id": identity(1), "RepoTags": [], "RepoDigests": []}
        self.assertIsNone(cache.owned_image(cache.Runtime.docker_detail(index), "fixture"))
        index["Config"] = {"Labels": {cache.CLUSTER_LABEL: "fixture", cache.TYPE_LABEL: "bff"}}
        with self.assertRaisesRegex(ValueError, "creation time"):
            cache.owned_image(cache.Runtime.docker_detail(index), "fixture")


if __name__ == "__main__":
    unittest.main()
