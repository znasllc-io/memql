#!/usr/bin/env python3
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location("delivery_artifacts", Path(__file__).with_name("delivery_artifacts.py"))
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)

GO_DOWNLOAD = (
    "go: downloading go1.27.3 (linux/amd64)\n"
    "go: download go1.27.3: golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64: "
    'Get "https://proxy.golang.org/golang.org/toolchain/@v/v0.0.1-go1.27.3.linux-amd64.zip": '
)
GO_DNS_FAILURE = GO_DOWNLOAD + "dial tcp: lookup proxy.golang.org on 127.0.0.53:53: server misbehaving\n"


class ArtifactFixtureTest(unittest.TestCase):
    def test_only_exact_transient_toolchain_downloads_are_retried(self):
        for cause in (
                "dial tcp: lookup proxy.golang.org on 127.0.0.53:53: server misbehaving",
                "dial tcp: lookup proxy.golang.org: i/o timeout",
                "dial tcp 142.250.190.17:443: i/o timeout",
                "read tcp 127.0.0.1:1234->142.250.190.17:443: read: connection reset by peer",
                "net/http: TLS handshake timeout", "EOF"):
            with self.subTest(cause=cause):
                self.assertTrue(fixture.transient_go_toolchain_download(GO_DOWNLOAD + cause + "\n"))
        for raw in (
                GO_DOWNLOAD + "dial tcp: lookup proxy.golang.org: no such host\n",
                GO_DOWNLOAD + "x509: certificate signed by unknown authority\n",
                GO_DOWNLOAD + "403 Forbidden\n",
                GO_DNS_FAILURE.replace("https://proxy.golang.org/", "https://other-proxy.example/"),
                GO_DNS_FAILURE.replace("linux-amd64.zip", "darwin-arm64.zip"),
                GO_DNS_FAILURE.replace("go: downloading go1.27.3", "go: downloading go1.27.2"),
                GO_DNS_FAILURE + "FAIL\t./app\n",
                GO_DNS_FAILURE + "checksum mismatch\n",
                "go: invalid GOTOOLCHAIN value\n", "go: toolchain not available\n",
                "verifying golang.org/toolchain: checksum mismatch\n",
                "# example/package\nfile.go:10: undefined: missing\nFAIL\texample/package [build failed]\n",
                "--- FAIL: TestDNS\n    dial tcp: lookup proxy.golang.org: server misbehaving\n",
                GO_DNS_FAILURE.splitlines()[1] + "\n"):
            with self.subTest(raw=raw):
                self.assertFalse(fixture.transient_go_toolchain_download(raw))

    def test_go_download_attempts_share_budgets_and_retain_all_evidence(self):
        success = "go version go1.27.3 linux/amd64\n"
        # Model elapsed command time and backoff without network or real waits.
        # Each response is (duration, exit status, diagnostic).
        for name, remaining, responses, error, waits, sleeps in (
                ("transient then success", 1800, [(14.5, 1, GO_DNS_FAILURE), (1, 0, success)], None, [600, 583.5], [2]),
                ("three transient failures", 1800, [(1, 1, GO_DNS_FAILURE)] * 3, RuntimeError, [600, 597, 592], [2, 4]),
                ("prerequisite timeout clipped", 1800, [(590, 1, GO_DNS_FAILURE), (9, 1, GO_DNS_FAILURE)], fixture.subprocess.TimeoutExpired, [600, 8], [2]),
                ("overall timeout clipped", 20, [(14.5, 1, GO_DNS_FAILURE), (4, 1, GO_DNS_FAILURE)], fixture.subprocess.TimeoutExpired, [20, 3.5], [2]),
                ("no budget for backoff", 1800, [(599, 1, GO_DNS_FAILURE)], RuntimeError, [600], []),
                ("interrupted process", 1800, [(1, -15, GO_DNS_FAILURE)], RuntimeError, [600], []),
                ("permanent failure", 1800, [(1, 1, "verifying golang.org/toolchain: checksum mismatch\n")], RuntimeError, [600], []),
                ("configuration failure", 1800, [(1, 1, "go: invalid GOTOOLCHAIN value\n")], RuntimeError, [600], []),
                ("permanent after transient", 1800, [(1, 1, GO_DNS_FAILURE), (1, 1, "go: toolchain not available\n")], RuntimeError, [600, 597], [2])):
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                run = fixture.Fixture(Path(directory), directory)
                now = [100.0]
                run.deadline = now[0] + remaining
                commands, actual_waits, actual_sleeps = [], [], []

                def popen(argv, **kwargs):
                    self.assertEqual(argv, ["go", "version"])
                    self.assertEqual(kwargs["stderr"], fixture.subprocess.STDOUT)
                    duration, code, raw = responses[len(commands)]
                    commands.append(argv)
                    process = Mock(pid=123, returncode=code)
                    communicated = False

                    def communicate(timeout=None):
                        nonlocal communicated
                        if not communicated:
                            communicated = True
                            actual_waits.append(timeout)
                            now[0] += min(duration, timeout)
                            if duration > timeout:
                                raise fixture.subprocess.TimeoutExpired(argv, timeout)
                        return raw.encode(), None

                    process.communicate.side_effect = communicate
                    return process

                def sleep(seconds):
                    actual_sleeps.append(seconds)
                    now[0] += seconds

                with patch.object(fixture.time, "monotonic", side_effect=lambda: now[0]), \
                        patch.object(fixture.time, "sleep", side_effect=sleep), \
                        patch.object(fixture.subprocess, "Popen", side_effect=popen), \
                        patch.object(fixture.os, "killpg") as kill:
                    if error:
                        with self.assertRaises(error):
                            run.go_prerequisite()
                    else:
                        self.assertEqual(run.go_prerequisite(), success)
                self.assertEqual(actual_waits, waits)
                self.assertEqual(actual_sleeps, sleeps)
                self.assertEqual(len(commands), len(responses))
                self.assertLessEqual(now[0] - 100, min(600, remaining))
                self.assertEqual(run.deadline, 100 + remaining)
                expected_logs = ["go-version.log"] + [f"go-version-attempt-{n}.log" for n in range(2, len(responses) + 1)]
                self.assertEqual(sorted(p.name for p in run.output.iterdir()), sorted(expected_logs))
                for log, (_, _, diagnostic) in zip(expected_logs, responses):
                    self.assertEqual((run.output / log).read_text(), diagnostic)
                self.assertEqual(kill.call_count, int(error is fixture.subprocess.TimeoutExpired))
                self.assertEqual(run.report["tests"], [])
                self.assertEqual(run.containers, [])

    def test_ordinary_test_commands_are_never_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            run = fixture.Fixture(Path(directory), directory)
            process = Mock(returncode=1)
            process.communicate.return_value = GO_DNS_FAILURE.encode(), None
            with patch.object(fixture.subprocess, "Popen", return_value=process) as popen, \
                    patch.object(fixture.time, "sleep") as sleep:
                with self.assertRaises(RuntimeError):
                    run.run(["go", "test", "./app"], log="app.log")
            popen.assert_called_once()
            sleep.assert_not_called()
            self.assertEqual((run.output / "app.log").read_text(), GO_DNS_FAILURE)

    def test_go_prerequisite_budget_and_failure_evidence(self):
        # The mock models elapsed setup time without sleeping for minutes or
        # downloading a toolchain. Only success reaches the next prerequisite.
        for name, remaining, duration, code, expected, allowance in (
                ("slow setup", 1800, 121, 0, StopIteration, 600),
                ("remaining budget", 150, 121, 0, StopIteration, 150),
                ("setup error", 1800, 1, 17, RuntimeError, 600),
                ("setup timeout", 1800, 601, 0, fixture.subprocess.TimeoutExpired, 600),
                ("overall timeout", 90, 121, 0, fixture.subprocess.TimeoutExpired, 90)):
            with self.subTest(case=name), tempfile.TemporaryDirectory() as directory:
                run = fixture.Fixture(Path(directory), directory)
                run.deadline = 100 + remaining
                diagnostic = b"go: provisioning selected toolchain\n"
                go = Mock(pid=123, returncode=code)
                waits = []

                def communicate(timeout=None):
                    waits.append(timeout)
                    if len(waits) == 1 and duration > timeout:
                        raise fixture.subprocess.TimeoutExpired(["go", "version"], timeout)
                    return diagnostic, None

                go.communicate.side_effect = communicate
                commands = []

                def popen(argv, **kwargs):
                    commands.append(argv)
                    if argv[0] == "git":
                        raw = b"a" * 40 + b"\n" if argv[1] == "rev-parse" else b""
                        process = Mock(returncode=0)
                        process.communicate.return_value = raw, None
                        return process
                    self.assertEqual(argv, ["go", "version"])
                    self.assertEqual(kwargs["stderr"], fixture.subprocess.STDOUT)
                    return go

                with patch.object(fixture.time, "monotonic", return_value=100), \
                        patch.object(fixture.subprocess, "Popen", side_effect=popen), \
                        patch.object(fixture.os, "killpg") as kill, \
                        patch.object(run, "docker", side_effect=StopIteration("next prerequisite")) as docker:
                    with self.assertRaises(expected):
                        run.qualify(None)
                self.assertEqual(waits[0], allowance)
                self.assertEqual((run.output / "go-version.log").read_bytes(), diagnostic)
                self.assertEqual(commands, [["git", "rev-parse", "HEAD"],
                                            ["git", "status", "--porcelain"], ["go", "version"]])
                self.assertEqual(run.containers, [])
                self.assertEqual(run.report["tests"], [])
                if expected is StopIteration:
                    docker.assert_called_once_with("version", "--format", "{{.Client.Version}} / {{.Server.Version}}")
                else:
                    docker.assert_not_called()
                if expected is fixture.subprocess.TimeoutExpired:
                    self.assertEqual(waits, [allowance, 5])
                    kill.assert_called_once_with(go.pid, fixture.signal.SIGTERM)
                else:
                    self.assertEqual(waits, [allowance])
                    kill.assert_not_called()

    def test_exhausted_overall_budget_refuses_go_before_startup(self):
        for now in (200, 201):
            with self.subTest(now=now), tempfile.TemporaryDirectory() as directory:
                run = fixture.Fixture(Path(directory), directory)
                run.deadline = 200
                processes = []
                for raw in (b"a" * 40 + b"\n", b""):
                    process = Mock(returncode=0)
                    process.communicate.return_value = raw, None
                    processes.append(process)
                with patch.object(fixture.time, "monotonic", side_effect=[100, 199, now, now]), \
                        patch.object(fixture.subprocess, "Popen", side_effect=processes) as popen, \
                        patch.object(run, "docker") as docker:
                    with self.assertRaisesRegex(RuntimeError, "overall deadline exhausted"):
                        run.qualify(None)
                self.assertEqual([call.args[0] for call in popen.call_args_list],
                                 [["git", "rev-parse", "HEAD"], ["git", "status", "--porcelain"]])
                docker.assert_not_called()
                self.assertIn("overall deadline exhausted", (run.output / "go-version.log").read_text())
                self.assertEqual(run.containers, [])
                self.assertEqual(run.report["tests"], [])

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

    def write_image(self, path, corrupt=False, duplicate=False, missing_platforms=(), selected_platform=None, missing_selected=False, config_arch="arm64"):
        blobs = {}

        def blob(body, kind):
            digest = hashlib.sha256(body).hexdigest()
            blobs["blobs/sha256/" + digest] = body
            return {"digest": "sha256:" + digest, "size": len(body), "mediaType": "application/vnd.oci." + kind}

        config = blob(json.dumps({"os": "linux", "architecture": config_arch}).encode(), "image.config.v1+json")
        layer = blob(b"real archive content", "image.layer.v1.tar+gzip")
        manifest = blob(json.dumps({"schemaVersion": 2, "config": config, "layers": [layer]}).encode(), "image.manifest.v1+json")
        if selected_platform is not None:
            manifest["platform"] = selected_platform
        manifests = [manifest]
        for n, platform in enumerate(missing_platforms):
            absent = {"digest": "sha256:" + f"{n + 1:064x}", "size": 123,
                      "mediaType": "application/vnd.oci.image.manifest.v1+json"}
            if platform is not None:
                absent["platform"] = platform
            manifests.append(absent)
        index = blob(json.dumps({"schemaVersion": 2, "manifests": manifests}).encode(), "image.index.v1+json")
        blobs["index.json"] = json.dumps({"schemaVersion": 2, "manifests": [index]}).encode()
        blobs["oci-layout"] = b'{"imageLayoutVersion":"1.0.0"}'
        if corrupt:
            blobs["blobs/sha256/" + layer["digest"][7:]] = b"bad archive content"
        if missing_selected:
            del blobs["blobs/sha256/" + manifest["digest"][7:]]
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

    def test_uncached_other_platforms_do_not_break_selected_platform(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source.tar", Path(directory) / "target.tar"
            manifest, _ = self.write_image(source, missing_platforms=[
                {"os": "linux", "architecture": "amd64"},
                {"os": "unknown", "architecture": "unknown"},
                {"os": "windows", "architecture": "arm64"},
            ], selected_platform={"os": "linux", "architecture": "arm64"})
            self.assertEqual(fixture.export_platform(source, target), manifest["digest"])
            with tarfile.open(target) as archive:
                self.assertEqual(len(archive.getmembers()), 5)

    def test_missing_selected_or_unclassified_manifest_still_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source.tar", Path(directory) / "target.tar"
            for params in ({"missing_selected": True}, {"missing_platforms": [None]},
                           {"missing_platforms": [{"os": "linux", "architecture": "arm64"}]}):
                with self.subTest(params=params):
                    self.write_image(source, **params)
                    with self.assertRaisesRegex(ValueError, "missing OCI blob"):
                        fixture.export_platform(source, target)
                    self.assertFalse(target.exists())

    def test_selected_platform_requires_matching_config_and_valid_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            source, target = Path(directory) / "source.tar", Path(directory) / "target.tar"
            for platform, arch in (({"os": "linux", "architecture": "arm64"}, "amd64"),
                                   ({"os": "linux"}, "arm64"), ([], "arm64")):
                with self.subTest(platform=platform, arch=arch):
                    self.write_image(source, selected_platform=platform, config_arch=arch)
                    with self.assertRaisesRegex(ValueError, "platform"):
                        fixture.export_platform(source, target)
                    self.assertFalse(target.exists())

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
