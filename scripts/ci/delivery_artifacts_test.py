#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("delivery_artifacts", Path(__file__).with_name("delivery_artifacts.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class ArtifactFixtureTest(unittest.TestCase):
    def test_artifact_cache_must_match_requested_platform(self):
        for cached_arch, want_pull in (("amd64", True), ("arm64", False)):
            with self.subTest(cached_arch=cached_arch), tempfile.TemporaryDirectory() as directory:
                run = fixture.Fixture(Path(directory), directory)
                calls = []

                def docker(*args, **_):
                    calls.append(args)
                    return 0, json.dumps([{"Os": "linux", "Architecture": cached_arch}])

                run.docker = docker
                ref = "example/image@sha256:" + "a" * 64
                run.image(ref, "linux/arm64")
                self.assertEqual(any(c[0] == "pull" for c in calls), want_pull)
                if want_pull:
                    self.assertEqual(calls[-1], ("pull", "--platform=linux/arm64", ref))

    def test_readiness_retries_provider_startup_disconnect(self):
        with tempfile.TemporaryDirectory() as directory:
            run = fixture.Fixture(Path(directory), directory)
            replies = [fixture.http.client.RemoteDisconnected("starting"),
                       fixture.urllib.error.HTTPError("http://127.0.0.1", 403, "auth required", {}, None)]
            with patch.object(fixture.urllib.request, "urlopen", side_effect=replies) as probe, \
                    patch.object(fixture.time, "sleep"):
                run.wait_http(10000, "/deliveryfixture")
            self.assertEqual(probe.call_count, 2)

    def write_image(self, path, corrupt=False, duplicate=False):
        blobs = {}

        def blob(body, kind):
            digest = hashlib.sha256(body).hexdigest()
            blobs["blobs/sha256/" + digest] = body
            return {"digest": "sha256:" + digest, "size": len(body), "mediaType": "application/vnd.oci." + kind}

        config = blob(b'{"os":"linux","architecture":"arm64"}', "image.config.v1+json")
        layer = blob(b"real archive content", "image.layer.v1.tar+gzip")
        manifest = blob(json.dumps({"schemaVersion": 2, "config": config, "layers": [layer]}).encode(), "image.manifest.v1+json")
        index = blob(json.dumps({"schemaVersion": 2, "manifests": [manifest]}).encode(), "image.index.v1+json")
        blobs["index.json"] = json.dumps({"schemaVersion": 2, "manifests": [index]}).encode()
        blobs["oci-layout"] = b'{"imageLayoutVersion":"1.0.0"}'
        if corrupt:
            blobs["blobs/sha256/" + layer["digest"][7:]] = b"bad archive content"
        with tarfile.open(path, "w") as archive:
            for name, body in blobs.items():
                member = tarfile.TarInfo(name)
                member.size = len(body)
                archive.addfile(member, io.BytesIO(body))
            if duplicate:
                member = tarfile.TarInfo("index.json")
                archive.addfile(member, io.BytesIO())
        return manifest, layer

    def test_platform_projection_preserves_image_and_layer_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source.tar", Path(directory) / "projected.tar"
            manifest, layer = self.write_image(source)
            self.assertEqual(fixture.export_platform(source, target), manifest["digest"])
            with tarfile.open(target) as archive:
                index = json.load(archive.extractfile("index.json"))
                self.assertEqual(index["manifests"][0]["digest"], manifest["digest"])
                self.assertEqual(index["manifests"][0]["platform"], {"os": "linux", "architecture": "arm64"})
                self.assertEqual(archive.extractfile("blobs/sha256/" + layer["digest"][7:]).read(), b"real archive content")
                self.assertEqual(len(archive.getmembers()), 5)
            # Fixed metadata/order makes the exact same image's archive reproducible.
            again = Path(directory) / "again.tar"
            fixture.export_platform(source, again)
            self.assertEqual(fixture.digest_file(target), fixture.digest_file(again))

    def test_corrupt_content_and_ambiguous_archive_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source.tar", Path(directory) / "target.tar"
            for option in ("corrupt", "duplicate"):
                self.write_image(source, **{option: True})
                with self.assertRaises(ValueError):
                    fixture.export_platform(source, target)

    def test_skipped_or_absent_real_tests_cannot_pass(self):
        passed = json.dumps({"Action": "pass", "Test": "TestReal"})
        self.assertEqual(fixture.verify_events(passed, ["TestReal"]), ["TestReal"])
        for raw in ("", json.dumps({"Action": "skip", "Test": "TestReal"}),
                    passed + "\n" + json.dumps({"Action": "skip", "Test": "TestReal/provider"})):
            with self.assertRaises(ValueError):
                fixture.verify_events(raw, ["TestReal"])

    def test_cleanup_refuses_another_invocations_container(self):
        with tempfile.TemporaryDirectory() as directory:
            run = fixture.Fixture(Path(directory), directory)
            run.containers = ["owned-name"]
            calls = []

            def docker(*args, **_):
                calls.append(args)
                return 0, json.dumps([{"Config": {"Labels": {fixture.LABEL: "someone-else"}}}])

            run.docker = docker
            with self.assertRaises(RuntimeError):
                run.cleanup()
            self.assertFalse(run.report["cleanup"]["confirmed"])
            self.assertEqual(calls, [("inspect", "owned-name")])

    def test_cleanup_removes_owned_resources_after_unknown_create_reply(self):
        with tempfile.TemporaryDirectory() as directory:
            run = fixture.Fixture(Path(directory), directory)
            run.containers = ["owned-name"]
            calls = []

            def docker(*args, **_):
                calls.append(args)
                if args[0] == "inspect":
                    return 0, json.dumps([{"Config": {"Labels": {fixture.LABEL: run.token}}}])
                return 0, ""

            run.docker = docker
            run.cleanup()
            self.assertTrue(run.report["cleanup"]["confirmed"])
            self.assertIn(("rm", "-f", "-v", "owned-name"), calls)
            self.assertEqual(calls[-1][0], "ps")

    def test_inspection_failure_is_not_disappearance(self):
        with tempfile.TemporaryDirectory() as directory:
            run = fixture.Fixture(Path(directory), directory)
            run.containers = ["owned-name"]

            def docker(*args, **_):
                return (1, "inspect failed") if args[0] == "inspect" else (0, "container-id")

            run.docker = docker
            with self.assertRaises(RuntimeError):
                run.cleanup()
            self.assertFalse(run.report["cleanup"]["confirmed"])


if __name__ == "__main__":
    unittest.main()
