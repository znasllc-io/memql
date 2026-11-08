#!/usr/bin/env python3
"""Offline bootstrap-wrapper fixtures; never invoke a real installed service."""

import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import threading
import unittest

from fixture_process import SUITE_PROCESSES


SCRIPT = Path(__file__).with_name("upgrade-linux.sh")


class UpgradeLinuxTest(unittest.TestCase):
    def setUp(self):
        if os.getuid() == 0:
            self.skipTest("user-service fixtures require a non-root user; root refusal is tested separately")
        self.temporary = tempfile.TemporaryDirectory(prefix="memql-linux-upgrade-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.tools = self.root / "tools"
        self.tools.mkdir()
        self.installed = self.root / "installed with spaces"
        self.installed.mkdir()
        self.binary = self.installed / "memql-computeruse-linux-amd64"
        self.link = self.installed / "memql"
        self.link.symlink_to(self.binary)
        self.variant = "computeruse"
        self.record = self.root / "bootstrap.json"
        self.calls = self.root / "calls.jsonl"
        self.config = self.root / "worker.yaml"
        self.policy = self.root / "policy.yaml"
        self.unit = self.root / "memql-worker.service"
        self.config.write_text("preserve enrolled homes and private credentials")
        self.policy.write_text("preserve all consent and paused connections")
        self.unit.write_text("preserve existing service configuration")
        home = self.root / "home"
        home.mkdir()
        self.env = dict(os.environ, HOME=str(home), PATH=str(self.tools) + os.pathsep + os.environ["PATH"],
                        TEST_SERVICE_EXECUTABLE=str(self.link), TEST_BOOTSTRAP_RECORD=str(self.record),
                        TEST_SYSTEMCTL_CALLS=str(self.calls), MEMQL_UPGRADE_ALLOW_LOOPBACK_HTTP="1")
        self.make_executable(self.tools / "uname", "#!/bin/sh\nmain() { case \"$1\" in -s) echo Linux;; -m) echo x86_64;; esac; }\nmain \"$@\"\n")
        self.make_executable(self.tools / "systemctl", '''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
with open(os.environ["TEST_SYSTEMCTL_CALLS"], "a") as f: f.write(json.dumps(args)+"\\n")
if "--property=ExecStart" in args:
    if args[-1] == "memql-worker.service" and os.environ.get("TEST_MISSING_WORKER"):
        sys.exit(1)
    if args[-1] == "memql-menu.service" and os.environ.get("TEST_BAD_MENU_RESPONSE"):
        sys.stdout.buffer.write(b"\\xff")
        sys.exit(0)
    path = os.environ["TEST_SERVICE_EXECUTABLE"]
    if args[-1] == "memql-menu.service": path = os.environ.get("TEST_MENU_EXECUTABLE", path)
    print("{ path="+path.replace(" ", r"\\x20")+" ; argv[]=memql worker run ; }")
elif "--property=ActiveState" in args:
    print(os.environ.get("TEST_MENU_STATE", "active"))
elif "restart" in args:
    sys.exit(int(os.environ.get("TEST_MENU_RESTART_EXIT", "0")))
else:
    sys.exit(2)
''')
        self.files = {}
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                data = owner.files.get(self.path)
                if data is None:
                    self.send_error(404)
                else:
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(data)

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.env["MEMQL_UPGRADE_TEST_DOWNLOAD_BASE"] = f"http://127.0.0.1:{self.server.server_port}/release"
        self.configure()

    def make_executable(self, path, content):
        path.write_text(content)
        path.chmod(0o755)

    def command_source(self, version, variant, bootstrap=False):
        body = f'''#!/usr/bin/env python3
import json, os, sys
if sys.argv[1:] == ["--version"]:
    print("memql {version} ({variant})")
    sys.exit(0)
'''
        if bootstrap:
            body += '''with open(os.environ["TEST_BOOTSTRAP_RECORD"], "w") as f: json.dump(sys.argv[1:], f)
print(json.dumps({"ok": True, "updated": os.environ.get("TEST_BOOTSTRAP_UPDATED", "true") == "true",
                  "current_version": "1.1.0", "worker_reconnected": True}))
'''
        else:
            body += "sys.exit(2)\n"
        return body

    def configure(self, variant="computeruse", new_version="1.1.0+gnew", link_member=False):
        self.variant = variant
        self.make_executable(self.binary, self.command_source("1.0.0+gold", variant))
        payload = self.command_source(new_version, variant, True).encode()
        if variant == "computeruse":
            asset = "memql-computeruse-linux-amd64"
            manifest = asset + ".sha256"
        else:
            asset = "memql-1.1.0-linux-amd64.tar.gz"
            manifest = "memql-1.1.0-SHA256SUMS"
            archive = io.BytesIO()
            with tarfile.open(fileobj=archive, mode="w:gz") as tar:
                member = tarfile.TarInfo("./memql")
                member.mode = 0o755
                if link_member:
                    member.type = tarfile.SYMTYPE
                    member.linkname = "/bin/sh"
                    tar.addfile(member)
                else:
                    member.size = len(payload)
                    tar.addfile(member, io.BytesIO(payload))
            payload = archive.getvalue()
        self.asset = asset
        self.files = {"/release/" + asset: payload,
                      "/release/" + manifest: (hashlib.sha256(payload).hexdigest() + "  " + asset + "\n").encode()}

    def run_upgrade(self, *args):
        return SUITE_PROCESSES.run(["bash", str(SCRIPT), "--version=1.1.0", *args], env=self.env)

    def result(self, process):
        lines = process.stdout.splitlines()
        self.assertEqual(len(lines), 1, process.stdout + process.stderr)
        return json.loads(lines[0])

    def assert_preserved(self):
        self.assertEqual(self.config.read_text(), "preserve enrolled homes and private credentials")
        self.assertEqual(self.policy.read_text(), "preserve all consent and paused connections")
        self.assertEqual(self.unit.read_text(), "preserve existing service configuration")
        self.assertTrue(self.link.is_symlink())
        self.assertIn("1.0.0+gold", self.binary.read_text())  # wrapper delegates replacement to updater

    def test_verified_matching_variant_uses_existing_path_without_pairing(self):
        for variant in ("computeruse", "headless"):
            with self.subTest(variant=variant):
                self.configure(variant)
                process = self.run_upgrade("--confirm=restart-worker")
                result = self.result(process)
                self.assertEqual(process.returncode, 0, process.stderr + process.stdout)
                self.assertTrue(result["changed"])
                self.assertTrue(result["result"]["menu_restarted"])
                self.assertEqual(json.loads(self.record.read_text()), ["update", "--bootstrap", "--installed-executable=" + str(self.binary.resolve()),
                    "--installed-version=1.0.0+gold", "--version=1.1.0", "--confirm=restart-worker", "--json"])
                calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
                self.assertIn(["--user", "restart", "memql-menu.service"], calls)
                self.assertFalse(any("enable" in call or "daemon-reload" in call for call in calls))
                self.assert_preserved()

    def test_missing_confirmation_does_not_download_or_execute(self):
        process = self.run_upgrade()
        self.assertEqual(process.returncode, 3)
        self.assertFalse(self.result(process)["ok"])
        self.assertFalse(self.record.exists())
        self.assertFalse(self.calls.exists())

    def test_bad_checksum_wrong_version_and_link_archive_never_execute(self):
        for kind in ("checksum", "version", "archive-link"):
            with self.subTest(kind=kind):
                self.configure("headless" if kind == "archive-link" else "computeruse",
                               "1.2.0" if kind == "version" else "1.1.0", kind == "archive-link")
                if kind == "checksum":
                    self.files["/release/" + self.asset] += b"tampered"
                process = self.run_upgrade("--confirm=restart-worker")
                self.assertNotEqual(process.returncode, 0)
                self.assertFalse(self.result(process)["ok"])
                self.assertFalse(self.record.exists())
                self.assert_preserved()

    def test_inactive_menu_stays_inactive(self):
        self.env["TEST_MENU_STATE"] = "inactive"
        process = self.run_upgrade("--confirm=restart-worker")
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        self.assertFalse(self.result(process)["result"]["menu_restarted"])
        self.assertFalse(any("restart" in json.loads(line) for line in self.calls.read_text().splitlines()))

    def test_standalone_script_loads_runtime_from_the_same_tag(self):
        runtime = SCRIPT.parents[3] / "scripts" / "lib" / "capability.sh"
        # Native lanes put temporary files beneath .git/memql-scratch. A
        # standalone download can then have an unrelated checkout runtime
        # exactly three parents above it. That runtime must never be sourced.
        unrelated = self.root / "unrelated-checkout"
        decoy_runtime = unrelated / "scripts" / "lib" / "capability.sh"
        decoy_runtime.parent.mkdir(parents=True)
        decoy_runtime.write_bytes(runtime.read_bytes())
        downloaded = unrelated / ".git" / "memql-scratch" / "fixture" / "upgrade-linux.sh"
        downloaded.parent.mkdir(parents=True)
        downloaded.write_bytes(SCRIPT.read_bytes())
        curl_record = self.root / "curl.json"
        self.env["TEST_CAPABILITY_RUNTIME"] = str(runtime)
        self.env["TEST_CURL_RECORD"] = str(curl_record)
        self.make_executable(self.tools / "curl", '''#!/usr/bin/env python3
import json, os, pathlib, sys
args = sys.argv[1:]
pathlib.Path(os.environ["TEST_CURL_RECORD"]).write_text(json.dumps(args))
pathlib.Path(args[args.index("-o") + 1]).write_bytes(pathlib.Path(os.environ["TEST_CAPABILITY_RUNTIME"]).read_bytes())
''')
        process = SUITE_PROCESSES.run(["bash", str(downloaded), "--version=1.1.0", "--confirm=restart-worker"], env=self.env)
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        self.assertTrue(self.result(process)["ok"])
        self.assertTrue(curl_record.is_file(), "standalone script reused an unrelated ancestor runtime instead of fetching its pinned release")
        args = json.loads(curl_record.read_text())
        self.assertIn("https://raw.githubusercontent.com/znasllc-io/memql/refs/tags/cockpit/v1.1.0/scripts/lib/capability.sh", args)
        self.assertEqual(args[args.index("--proto-redir") + 1], "=https")

    def test_menu_restart_failure_keeps_successful_worker_outcome(self):
        self.env["TEST_MENU_RESTART_EXIT"] = "1"
        process = self.run_upgrade("--confirm=restart-worker")
        result = self.result(process)
        self.assertEqual(process.returncode, 5)
        self.assertTrue(result["changed"])
        self.assertTrue(result["result"]["outcome"]["updated"])
        self.assertIn("systemctl --user restart memql-menu.service", result["error"]["message"])

    def test_repeat_with_current_worker_delegates_noop_to_verified_helper(self):
        self.make_executable(self.binary, self.command_source("1.1.0+gnew", "computeruse", True))
        self.env["TEST_MENU_STATE"] = "inactive"
        self.env["TEST_BOOTSTRAP_UPDATED"] = "false"
        process = self.run_upgrade("--confirm=restart-worker")
        result = self.result(process)
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        self.assertFalse(result["changed"])
        self.assertFalse(result["result"]["updated"])
        arguments = json.loads(self.record.read_text())
        self.assertIn("--bootstrap", arguments)
        self.assertIn("--installed-version=1.1.0+gnew", arguments)

    def test_interrupted_swap_delegates_recovery_to_verified_helper(self):
        self.make_executable(self.binary, self.command_source("1.1.0+gnew", "computeruse", True))
        process = self.run_upgrade("--confirm=restart-worker")
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        self.assertTrue(self.result(process)["result"]["updated"])
        arguments = json.loads(self.record.read_text())
        self.assertIn("--bootstrap", arguments)
        self.assertIn("--installed-version=1.1.0+gnew", arguments)

    def test_missing_service_refuses_before_bootstrap(self):
        self.env["TEST_MISSING_WORKER"] = "yes"
        process = self.run_upgrade("--confirm=restart-worker")
        self.assertEqual(process.returncode, 4)
        self.assertFalse(self.result(process)["changed"])
        self.assertFalse(self.record.exists())

    def test_unexpected_menu_error_retains_completed_update(self):
        self.env["TEST_BAD_MENU_RESPONSE"] = "yes"
        process = self.run_upgrade("--confirm=restart-worker")
        result = self.result(process)
        self.assertEqual(process.returncode, 5)
        self.assertTrue(result["changed"])
        self.assertTrue(result["result"]["outcome"]["updated"])
        self.assertEqual(result["result"]["outcome"]["current_version"], "1.1.0")

    def test_protected_install_refuses_without_sudo_or_mutation(self):
        self.installed.chmod(0o555)
        self.addCleanup(self.installed.chmod, 0o755)
        process = self.run_upgrade("--confirm=restart-worker")
        self.assertEqual(process.returncode, 3)
        self.assertIn("protected installation", self.result(process)["error"]["message"])
        self.assertFalse(self.record.exists())
        self.assert_preserved()


class RootRefusalTest(unittest.TestCase):
    @unittest.skipUnless(os.getuid() == 0, "root-only refusal check")
    def test_root_is_refused(self):
        process = SUITE_PROCESSES.run(["bash", str(SCRIPT), "--version=1.1.0", "--confirm=restart-worker"])
        self.assertNotEqual(process.returncode, 0)
        self.assertIn("without sudo", process.stdout)


if __name__ == "__main__":
    unittest.main()
