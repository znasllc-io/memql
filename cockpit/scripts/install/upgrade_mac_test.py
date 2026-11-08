#!/usr/bin/env python3
"""Offline one-time Mac upgrade fixtures; no installed apps or services touched."""

import hashlib
import io
import json
import os
from pathlib import Path
import plistlib
import tarfile
import tempfile
import unittest

from fixture_process import SUITE_PROCESSES

SCRIPT = Path(__file__).with_name("upgrade-mac.sh")
REPO = SCRIPT.parents[3]


class UpgradeMacTest(unittest.TestCase):
    def setUp(self):
        if os.getuid() == 0:
            self.skipTest("native user service fixtures require a non-root user")
        temporary = tempfile.TemporaryDirectory(prefix="memql-mac-upgrade-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.home = self.root / "home with spaces"
        self.app = self.home / "Applications/MemQL.app"
        self.worker = self.app / "Contents/MacOS/MemQL"
        self.worker.parent.mkdir(parents=True)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.plist = self.home / "Library/LaunchAgents/com.znasllc.memql-worker.plist"
        self.plist.parent.mkdir(parents=True)
        self.plist.write_bytes(plistlib.dumps({"Label": "com.znasllc.memql-worker", "ProgramArguments": [str(self.worker), "worker", "run", "--private-fixture=secret"]}))
        self.plist.chmod(0o600)
        self.config = self.home / "workers.yaml"
        self.config.write_text("saved identity + secret + paused homes unchanged")
        self.record = self.root / "helper.json"
        self.calls = self.root / "launchctl.jsonl"
        self.downloads = self.root / "downloads.jsonl"
        self.env = dict(os.environ, HOME=str(self.home), PATH=str(self.tools) + os.pathsep + os.environ["PATH"],
                        TEST_FIXTURE_ROOT=str(self.root), TEST_APP=str(self.app), TEST_RECORD=str(self.record),
                        TEST_CALLS=str(self.calls), TEST_DOWNLOADS=str(self.downloads), TEST_REPO=str(REPO))
        self.executable(self.tools / "uname", '#!/bin/sh\nmain() { case "$1" in -s) echo Darwin;; -m) echo arm64;; esac; }\nmain "$@"\n')
        self.executable(self.tools / "codesign", '#!/bin/sh\nmain() { exit "${TEST_SIGN_EXIT:-0}"; }\nmain "$@"\n')
        self.executable(self.tools / "launchctl", '''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
with open(os.environ["TEST_CALLS"], "a") as f: f.write(json.dumps(args)+"\\n")
if args[0] == "print":
    if os.environ.get("TEST_MENU_ABSENT"): sys.exit(1)
    print("program = " + os.environ.get("TEST_MENU_PROGRAM", os.environ["TEST_APP"] + "/Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit"))
    if not os.environ.get("TEST_MENU_STOPPED"): print("pid = 12345")
    else: print("state = not running")
elif args[0] == "kickstart": sys.exit(int(os.environ.get("TEST_MENU_EXIT", "0")))
else: sys.exit(2)
''')
        self.executable(self.tools / "curl", '''#!/usr/bin/env python3
import json, os, pathlib, shutil, sys
args = sys.argv[1:]
url = next(arg for arg in args if arg.startswith("https://"))
dest = args[args.index("-o")+1]
with open(os.environ["TEST_DOWNLOADS"], "a") as f: f.write(json.dumps(args)+"\\n")
name = url.rsplit("/",1)[-1]
if name == "capability.sh": source = pathlib.Path(os.environ["TEST_REPO"]) / "scripts/lib/capability.sh"
elif name == "lib.sh": source = pathlib.Path(os.environ["TEST_REPO"]) / "cockpit/scripts/install/lib.sh"
else: source = pathlib.Path(os.environ["TEST_FIXTURE_ROOT"]) / name
shutil.copyfile(source, dest)
''')
        self.configure()

    def executable(self, path, body):
        path.write_text(body)
        path.chmod(0o755)

    def configure(self, version="0.22.1+g982248465766.dirty", variant="computeruse"):
        self.executable(self.worker, f'#!/bin/sh\nmain() {{ echo "memql {version} ({variant})"; }}\nmain "$@"\n')
        helper = '''#!/usr/bin/env python3
import json, os, sys
with open(os.environ["TEST_RECORD"], "w") as f: json.dump(sys.argv[1:], f)
print(json.dumps({"ok": os.environ.get("TEST_HELPER_OK", "1") == "1", "updated": os.environ.get("TEST_UPDATED", "1") == "1", "current_version": "0.23.0", "worker_reconnected": True, "phase": "complete", "error": os.environ.get("TEST_HELPER_ERROR", "")}))
sys.exit(int(os.environ.get("TEST_HELPER_EXIT", "0")))
'''
        archive = self.root / "memql-app-darwin-arm64.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            files = {"MemQL.app/Contents/MacOS/MemQL": helper,
                     "scripts/macos/install-app-files.sh": "fixture",
                     "scripts/macos/activate-app.sh": "fixture",
                     "scripts/macos/install-menubar.sh": "fixture",
                     "scripts/lib/capability.sh": "fixture"}
            for name, content in files.items():
                data = content.encode()
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o755 if name.endswith("/MemQL") else 0o644
                tar.addfile(info, io.BytesIO(data))
        self.checksum = self.root / (archive.name + ".sha256")
        self.checksum.write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + "  " + archive.name + "\n")

    def run_upgrade(self, *args, script=SCRIPT):
        result = SUITE_PROCESSES.run(["bash", str(script), *(args or ("--version=0.23.0", "--confirm=restart-worker"))], env=self.env)
        try:
            body = json.loads(result.stdout)
        except ValueError:
            self.fail(f"not one JSON envelope: {result.returncode}: {result.stdout!r} {result.stderr!r}")
        self.assertNotIn("secret", result.stdout + result.stderr)
        return result, body

    def test_passes_exact_install_and_version_without_touching_saved_files(self):
        before = {path: path.read_bytes() for path in (self.config, self.plist, self.worker)}
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(body["ok"])
        self.assertTrue(body["changed"])
        self.assertTrue(body["result"]["menu_restarted"])
        args = json.loads(self.record.read_text())
        self.assertEqual(args, ["update", "--bootstrap", "--installed-executable=" + str(self.worker.resolve()),
                               "--installed-version=0.22.1+g982248465766.dirty", "--version=0.23.0", "--confirm=restart-worker", "--json"])
        for path, original in before.items(): self.assertEqual(path.read_bytes(), original)

    def test_paused_or_absent_menu_stays_stopped(self):
        for setting in ("TEST_MENU_STOPPED", "TEST_MENU_ABSENT"):
            self.env[setting] = "1"
            self.env["TEST_UPDATED"] = "0"
            result, body = self.run_upgrade()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse(body["changed"])
            self.assertFalse(body["result"]["menu_restarted"])
            self.assertNotIn("kickstart", self.calls.read_text())
            del self.env[setting]

    def test_repeated_same_version_can_refresh_running_menu(self):
        self.configure(version="0.23.0+grelease")
        self.env["TEST_UPDATED"] = "0"
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(body["changed"])
        self.assertFalse(body["result"]["update"]["updated"])
        self.assertIn("--installed-version=0.23.0+grelease", self.record.read_text())

    def test_checksum_and_signature_failure_never_run_helper(self):
        self.checksum.write_text("0" * 64 + "  memql-app-darwin-arm64.tar.gz\n")
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 3)
        self.assertFalse(body["changed"])
        self.assertFalse(self.record.exists())
        self.configure()
        self.env["TEST_SIGN_EXIT"] = "1"
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 3)
        self.assertFalse(self.record.exists())

    def test_rejects_other_variant_and_unsafe_service(self):
        self.configure(variant="headless")
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 3)
        self.assertFalse(self.downloads.exists())
        self.configure()
        self.plist.chmod(0o666)
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 3)
        self.assertFalse(self.record.exists())

    def test_helper_error_preserves_partial_updated_result(self):
        self.env.update(TEST_HELPER_OK="0", TEST_HELPER_EXIT="1", TEST_HELPER_ERROR="worker needs recovery")
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 5)
        self.assertFalse(body["ok"])
        self.assertTrue(body["changed"])
        self.assertTrue(body["result"]["update"]["updated"])
        self.assertEqual(body["result"]["update"]["error"], "worker needs recovery")
        self.assertFalse(self.calls.exists())

    def test_helper_nonzero_cannot_be_success(self):
        self.env["TEST_HELPER_EXIT"] = "1"
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 5)
        self.assertFalse(body["ok"])

    def test_menu_failure_retains_worker_success(self):
        self.env["TEST_MENU_EXIT"] = "1"
        result, body = self.run_upgrade()
        self.assertEqual(result.returncode, 5)
        self.assertTrue(body["changed"])
        self.assertTrue(body["result"]["update"]["updated"])

    def test_copied_script_fetches_pinned_runtime_and_verifier(self):
        copied = self.root / "upgrade-mac.sh"
        copied.write_bytes(SCRIPT.read_bytes())
        result, body = self.run_upgrade(script=copied)
        self.assertEqual(result.returncode, 0, result.stderr)
        requests = [json.loads(line) for line in self.downloads.read_text().splitlines()]
        self.assertEqual(len(requests), 4)
        for args in requests:
            self.assertIn("--proto-redir", args)
            self.assertTrue(any("cockpit/v0.23.0/" in arg for arg in args))

    def test_standalone_inside_checkout_ignores_ancestor_and_sibling_helpers(self):
        checkout = self.root / "checkout"
        copied = checkout / ".git/memql-scratch/fixture/upgrade-mac.sh"
        copied.parent.mkdir(parents=True)
        copied.write_bytes(SCRIPT.read_bytes())
        ancestor_runtime = checkout / "scripts/lib/capability.sh"
        ancestor_runtime.parent.mkdir(parents=True)
        ancestor_runtime.write_bytes((REPO / "scripts/lib/capability.sh").read_bytes())
        (copied.parent / "lib.sh").write_bytes(SCRIPT.with_name("lib.sh").read_bytes())
        result, body = self.run_upgrade(script=copied)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(body["ok"])
        requests = [json.loads(line) for line in self.downloads.read_text().splitlines()]
        urls = [next(arg for arg in args if arg.startswith("https://")) for args in requests]
        self.assertEqual(len(urls), 4)
        self.assertIn("/cockpit/v0.23.0/scripts/lib/capability.sh", urls[0])
        self.assertIn("/cockpit/v0.23.0/cockpit/scripts/install/lib.sh", urls[1])

    def test_bad_flags_return_capability_error(self):
        result, body = self.run_upgrade("--version=latest")
        self.assertEqual(result.returncode, 2)
        self.assertFalse(body["ok"])
        self.assertFalse(self.downloads.exists())


if __name__ == "__main__":
    unittest.main()
