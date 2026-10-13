"""Exercise the real recovery capability against deterministic registry/GitHub tools."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


HERE = Path(__file__).resolve().parent
SHA = "5" * 40
VERSION = "1.2.3"

FAKE_TOOL = r'''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ["RECOVERY_FIXTURE"])
tool, args = pathlib.Path(sys.argv[0]).name, sys.argv[1:]
with (root / "calls").open("a") as log:
    log.write(json.dumps([tool] + args) + "\n")
data = json.loads((root / "fixture.json").read_text())
scenario = data.get("scenario", "")
def emit(value):
    print(json.dumps(value))
def raw(name):
    sys.stdout.buffer.write((root / name).read_bytes())
if tool == "gh":
    if args[:2] == ["release", "view"]:
        emit(data["release"])
    elif "/commits/" in args[1]:
        print(data["tagSha"])
    elif "/contents/VERSION" in args[1]:
        print(data["versionFile"])
    else:
        sys.exit(99)
elif tool == "skopeo":
    if args[0] == "list-tags":
        if scenario == "unreadable-target":
            print("unauthorized or unavailable", file=sys.stderr)
            sys.exit(1)
        countfile = root / "list-count"
        count = int(countfile.read_text()) + 1 if countfile.exists() else 1
        countfile.write_text(str(count))
        present = scenario in ["same-target", "conflicting-target"] or (scenario == "target-race" and count == 2)
        emit({"Tags": ["1.2.3"] if present else ["0.9.0"]})
    elif args[0] == "copy":
        if args != ["copy", "--all", "--preserve-digests", "docker://acrmemql.azurecr.io/memql-agent@" + data["digest"], "docker://ghcr.io/znasllc-io/memql-agent:1.2.3"]:
            sys.exit(98)
        (root / "copied").touch()
        if scenario == "copy-failed":
            sys.exit(1)
    elif args[0] == "inspect" and "--raw" in args:
        target = args[-1]
        if "--config" in args:
            raw("config.json")
            if scenario == "corrupt-config":
                print(" ")
        else:
            raw("manifest.json")
            wrong_tag = scenario == "wrong-source-tag" and target.endswith(":1.2.3") and "acrmemql" in target
            corrupt_source = scenario == "corrupt-source" and "acrmemql" in target and "@" in target
            wrong_target = "ghcr.io" in target and scenario in ["conflicting-target", "target-race", "wrong-copy-digest"]
            if wrong_tag or corrupt_source or wrong_target:
                print(" ")
    else:
        sys.exit(97)
elif tool == "docker":
    if args[0] == "pull":
        pass
    elif args[0] == "create":
        print("stopped-inspection-container")
    elif args[0] == "cp":
        if scenario == "extract-failed":
            sys.exit(1)
        pathlib.Path(args[-1]).write_text("never execute me")
    elif args[0] == "rm":
        pass
    else:
        sys.exit(96)  # In particular, no docker run/start/build/push/tag.
elif tool == "go":
    if args[:2] != ["version", "-m"]:
        sys.exit(95)
    raw("build-info.txt")
else:
    sys.exit(94)
'''


class RecoveryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for tool in ["gh", "skopeo", "docker", "go"]:
            path = self.root / tool
            path.write_text(FAKE_TOOL)
            path.chmod(0o755)
        self.data = {
            "release": {"tagName": "v" + VERSION, "isDraft": False, "publishedAt": "2026-10-12T00:00:00Z"},
            "tagSha": SHA,
            "versionFile": VERSION,
        }
        self.config = {"os": "linux", "architecture": "amd64", "config": {"Labels": {
            "org.opencontainers.image.source": "https://github.com/znasllc-io/memql",
            "org.opencontainers.image.version": VERSION,
        }}}
        self.info = "engine: go1.26.1\n\tpath\tgithub.com/znasllc-io/memql\n\tbuild\t-tags=agent\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n"
        self.info += '\tbuild\t-ldflags="-s -w -X github.com/znasllc-io/memql/core/buildinfo.release=' + VERSION + ' -X github.com/znasllc-io/memql/core/buildinfo.commit=' + SHA + '"\n'
        self.env = dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ["PATH"], RECOVERY_FIXTURE=str(self.root),
                        GITHUB_ACTIONS="true", GITHUB_REPOSITORY="znasllc-io/memql", GITHUB_REF="refs/heads/main")
        self.env.pop("CAP_PARAMS_STDIN", None)

    def invoke(self, scenario="", flags=None):
        config = json.dumps(self.config).encode()
        (self.root / "config.json").write_bytes(config)
        manifest = json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                               "config": {"digest": "sha256:" + hashlib.sha256(config).hexdigest()}, "layers": []}).encode()
        (self.root / "manifest.json").write_bytes(manifest)
        self.data.update(digest="sha256:" + hashlib.sha256(manifest).hexdigest(), scenario=scenario)
        (self.root / "fixture.json").write_text(json.dumps(self.data))
        (self.root / "build-info.txt").write_text(self.info)
        args = ["--version=" + VERSION, "--sourceSha=" + SHA, "--node=agent", "--digest=" + self.data["digest"]]
        result = subprocess.run(["bash", str(HERE / "recover-engine-image-mirror.sh")] + (args if flags is None else flags),
                                env=self.env, capture_output=True, text=True, timeout=20)
        self.calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()] if (self.root / "calls").exists() else []
        # One JSON envelope, including every refusal. Logs belong on stderr.
        envelope = json.loads(result.stdout)
        self.assertEqual(envelope["ok"], result.returncode == 0, result.stderr)
        self.assertFalse(any(call[:2] in [["docker", forbidden] for forbidden in ["run", "start", "build", "push", "tag"]] for call in self.calls))
        return result, envelope

    def no_copy(self, scenario="", flags=None, reason=None):
        result, envelope = self.invoke(scenario, flags)
        self.assertNotEqual(result.returncode, 0, envelope)
        self.assertFalse((self.root / "copied").exists())
        self.assertFalse(envelope["changed"])
        if reason:
            self.assertIn(reason, envelope["error"]["message"])
        return result

    def test_missing_target_is_copied_by_exact_digest(self):
        result, envelope = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(envelope["changed"])
        self.assertEqual(envelope["result"]["status"], "recovered")
        self.assertEqual(envelope["result"]["digest"], self.data["digest"])
        self.assertEqual(sum(call[0:2] == ["skopeo", "copy"] for call in self.calls), 1)
        self.assertEqual(sum(call[0:2] == ["skopeo", "list-tags"] for call in self.calls), 2)
        self.assertIn(["docker", "rm", "stopped-inspection-container"], self.calls)

    def test_existing_identical_target_is_verified_noop(self):
        result, envelope = self.invoke("same-target")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(envelope["changed"])
        self.assertEqual(envelope["result"]["status"], "already-present")
        self.assertFalse((self.root / "copied").exists())

    def test_registry_refusals_never_copy(self):
        for scenario, reason in [("conflicting-target", "already exists"), ("wrong-source-tag", "ACR release tag"),
                                 ("corrupt-source", "ACR digest response"), ("corrupt-config", "source config"),
                                 ("unreadable-target", "which GHCR tags exist"), ("target-race", "already exists")]:
            with self.subTest(scenario=scenario):
                # Each attempt gets fresh tool state, especially the race's two reads.
                for name in ["calls", "list-count"]:
                    (self.root / name).unlink(missing_ok=True)
                self.no_copy(scenario, reason=reason)

    def test_copy_failure_or_changed_digest_never_reports_success(self):
        for scenario in ["copy-failed", "wrong-copy-digest"]:
            with self.subTest(scenario=scenario):
                result, envelope = self.invoke(scenario)
                self.assertEqual(result.returncode, 5, result.stderr)
                self.assertFalse(envelope["ok"])
                self.assertTrue(envelope["changed"])  # An upload was attempted.

    def test_release_provenance_refusals(self):
        for field, value in [("tagSha", "9" * 40), ("versionFile", "1.2.4"), ("release", {"tagName": "v1.2.3", "isDraft": True, "publishedAt": "now"}),
                             ("release", {"tagName": "v1.2.4", "isDraft": False, "publishedAt": "now"})]:
            with self.subTest(field=field, value=value):
                old = self.data[field]
                self.data[field] = value
                self.no_copy()
                self.data[field] = old

    def test_config_provenance_refusals(self):
        for key, value in [("os", "windows"), ("architecture", "arm64")]:
            with self.subTest(key=key):
                old = self.config[key]
                self.config[key] = value
                self.no_copy(reason="platform or release labels")
                self.config[key] = old
        for key, value in [("source", "https://example.com/other"), ("version", "1.2.4"), ("revision", "9" * 40)]:
            with self.subTest(label=key):
                labels = self.config["config"]["Labels"]
                label = "org.opencontainers.image." + key
                old = dict(labels)
                labels[label] = value
                self.no_copy(reason="platform or release labels")
                self.config["config"]["Labels"] = old

    def test_binary_provenance_refusals(self):
        original = self.info
        for before, after in [(SHA, "9" * 40), ("release=1.2.3", "release=1.2.4"), ("-tags=agent", "-tags=bff"),
                              ("GOOS=linux", "GOOS=windows"), ("GOARCH=amd64", "GOARCH=arm64"),
                              ("\tpath\tgithub.com/znasllc-io/memql", "\tpath\tunrelated"),
                              ("-X github.com/znasllc-io/memql/core/buildinfo.commit=" + SHA, ""),
                              ("-s -w", "-s -w -X github.com/znasllc-io/memql/core/buildinfo.release=1.2.3")]:
            with self.subTest(before=before):
                self.info = original.replace(before, after)
                self.no_copy(reason="engine binary provenance")
        self.info = original + "\tbuild\tGOOS=linux\n"
        self.no_copy(reason="engine binary provenance")

    def test_stopped_container_is_cleaned_after_extraction_failure(self):
        result = self.no_copy("extract-failed", reason="could not extract engine binary")
        self.assertEqual(result.returncode, 5)
        self.assertEqual(self.calls.count(["docker", "rm", "stopped-inspection-container"]), 1)
        scratch = Path(next(call[-1] for call in self.calls if call[:2] == ["docker", "cp"])).parent
        self.assertFalse(scratch.exists(), "failed extraction must remove its scratch directory")

    def test_unhandled_failure_retains_exit_code_and_one_failure_envelope(self):
        mktemp = self.root / "mktemp"
        mktemp.write_text("#!/usr/bin/env bash\nexit 5\n")
        mktemp.chmod(0o755)
        result = self.no_copy(reason="aborted (exit 5)")
        self.assertEqual(result.returncode, 5)
        self.assertEqual(self.calls, [])

    def test_refuses_non_build_server_or_non_main_before_external_calls(self):
        for key, value in [("GITHUB_ACTIONS", "false"), ("GITHUB_REF", "refs/heads/other"), ("GITHUB_REPOSITORY", "other/repo")]:
            with self.subTest(key=key):
                old = self.env[key]
                self.env[key] = value
                result = self.no_copy()
                self.assertEqual(result.returncode, 3)
                self.assertEqual(self.calls, [])
                self.env[key] = old

    def test_bad_parameters_do_not_reach_external_tools(self):
        self.no_copy(flags=["--version=1.2.3;echo injection", "--sourceSha=" + SHA, "--node=agent", "--digest=sha256:" + "0" * 64])
        self.assertEqual(self.calls, [])


class RealBuildInfoTest(unittest.TestCase):
    def test_reads_real_linked_go_build_metadata_without_running_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "go.mod").write_text("module github.com/znasllc-io/memql\n\ngo 1.20\n")
            (root / "core/buildinfo").mkdir(parents=True)
            (root / "core/buildinfo/info.go").write_text('package buildinfo\nvar release = "dev"\nvar commit = "none"\nfunc Values() string { return release + commit }\n')
            (root / "main.go").write_text('package main\nimport "github.com/znasllc-io/memql/core/buildinfo"\nfunc main() { panic(buildinfo.Values()) }\n')
            env = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local", GOOS="linux", GOARCH="amd64", CGO_ENABLED="0")
            flags = "-s -w -X github.com/znasllc-io/memql/core/buildinfo.release=" + VERSION + " -X github.com/znasllc-io/memql/core/buildinfo.commit=" + SHA
            subprocess.run(["go", "build", "-tags=agent", "-ldflags=" + flags, "-o", str(root / "engine"), "."], cwd=root, env=env, check=True, capture_output=True, timeout=60)
            info = subprocess.run(["go", "version", "-m", str(root / "engine")], check=True, capture_output=True, text=True).stdout
            (root / "info").write_text(info)
            result = subprocess.run(["python3", str(HERE / "verify-engine-build-info.py"), "--info", str(root / "info"),
                                     "--version", VERSION, "--source-sha", SHA, "--node", "agent"], capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr + "\n" + info)


if __name__ == "__main__":
    unittest.main()
