#!/usr/bin/env python3
"""Offline release discovery and installer integrity regressions.

curl is a closed fixture: an unknown URL fails instead of reaching a network.
Full installer cases use a temporary HOME/BIN_DIR and inert fixture binaries.
"""

import base64
import hashlib
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT_INSTALL = HERE.parent.parent / "install.sh"
API = "https://api.github.com/repos/znasllc-io/memql/releases?per_page=100"
RAW = "https://raw.githubusercontent.com/znasllc-io/memql/main/cockpit/scripts/install"


def release(tag, **flags):
    return dict(tag_name=tag, draft=False, prerelease=False, **flags)


class ReleaseDiscoveryTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="memql-release-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.pages = {}
        self.fixture = self.root / "responses.json"
        self.calls = self.root / "calls.jsonl"
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        HOME=str(self.root / "home"), BIN_DIR=str(self.root / "installed"),
                        MEMQL_TEST_RELEASE_RESPONSES=str(self.fixture),
                        MEMQL_TEST_RELEASE_CALLS=str(self.calls),
                        MEMQL_INSTALL_RELEASE_API=API)
        for key in ("MEMQL_COCKPIT_VERSION", "MEMQL_INSTALL_RAW_BASE", "MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP"):
            self.env.pop(key, None)
        Path(self.env["HOME"]).mkdir()
        curl = self.bin / "curl"
        curl.write_text("#!" + sys.executable + "\n" + '''
import base64,json,os,sys
from pathlib import Path
args=sys.argv[1:]
url=next((arg for arg in args if arg.startswith(("https://","http://","file://"))), "")
with open(os.environ["MEMQL_TEST_RELEASE_CALLS"], "a") as log:
    log.write(json.dumps({"url":url,"args":args})+"\\n")
entry=json.loads(Path(os.environ["MEMQL_TEST_RELEASE_RESPONSES"]).read_text()).get(url)
if entry is None: sys.exit(22)
if entry.get("exit"): sys.exit(entry["exit"])
body=base64.b64decode(entry["body64"]) if "body64" in entry else entry.get("body", "").encode()
if "--dump-header" in args:
    Path(args[args.index("--dump-header")+1]).write_text(entry.get("headers", ""))
if "-o" in args:
    Path(args[args.index("-o")+1]).write_bytes(body)
else: sys.stdout.buffer.write(body)
''')
        curl.chmod(0o755)

    def response(self, url, body, headers="", exit_code=0):
        if isinstance(body, bytes):
            entry = {"body64": base64.b64encode(body).decode()}
        else:
            entry = {"body": body if isinstance(body, str) else json.dumps(body)}
        self.pages[url] = dict(entry, headers=headers, exit=exit_code)

    def run_command(self, command, input_text=None):
        self.fixture.write_text(json.dumps(self.pages))
        return subprocess.run(command, input=input_text, env=self.env, cwd=self.root,
                              text=True, capture_output=True, timeout=20)

    def resolve(self):
        return self.run_command([sys.executable, str(HERE / "release-version.py"),
                                 "--api-url", self.env["MEMQL_INSTALL_RELEASE_API"]])

    def requested(self):
        return [json.loads(line) for line in self.calls.read_text().splitlines()] if self.calls.exists() else []

    def test_engine_page_cannot_hide_cockpit_and_order_cannot_downgrade(self):
        second = API + "&page=2"
        third = API + "&page=3"
        self.response(API, [release("v99.0." + str(i)) for i in range(100)],
                      f'HTTP/2 200\r\nLink: <{second}>; rel="next", <{third}>; rel="last"\r\n')
        self.response(second, [release("cockpit/v0.9.9"), release("cockpit/v0.21.0")],
                      f'Link: <{third}>; rel="next"\r\n')
        self.response(third, [release("cockpit/v0.20.5"), release("cockpit/v1.0.0-rc.1")])
        result = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "cockpit/v0.21.0")
        self.assertEqual([row["url"] for row in self.requested()], [API, second, third])

    def test_only_explicitly_published_stable_cockpit_tags_qualify(self):
        rows = [release("v99.0.0"), release("cockpit/v99.0.0"), release("cockpit/v88.0.0"),
                {"tag_name": "cockpit/v77.0.0"}, release("cockpit/v01.0.0"),
                release("cockpit/v0.20.1+build"), release("cockpit/v0.20.1")]
        rows[1]["draft"] = True
        rows[2]["prerelease"] = True
        self.response(API, rows)
        result = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "cockpit/v0.20.1")

    def test_no_release_is_an_honest_publication_failure(self):
        self.response(API, [release("v0.31.6")])
        result = self.resolve()
        self.assertEqual(result.returncode, 4)
        self.assertEqual(result.stdout, "")
        self.assertIn("No stable Cockpit release has been published", result.stderr)
        self.assertNotIn("select a version explicitly", result.stderr)
        self.assertNotIn("Traceback", result.stderr)

    def test_api_error_invalid_json_and_non_list_are_not_absence(self):
        for body, code in [("rate limit", 22), ("not JSON", 0), ({"message": "rate limited"}, 0), ([None], 0)]:
            with self.subTest(body=body):
                self.response(API, body, exit_code=code)
                result = self.resolve()
                self.assertEqual(result.returncode, 4)
                self.assertEqual(result.stdout, "")
                self.assertNotIn("Traceback", result.stderr)
                self.assertNotIn("No stable Cockpit", result.stderr)

    def test_failed_later_page_never_returns_an_older_release(self):
        second = API + "&page=2"
        self.response(API, [release("cockpit/v0.19.0")], f'Link: <{second}>; rel="next"\r\n')
        self.response(second, "", exit_code=28)
        result = self.resolve()
        self.assertEqual(result.returncode, 4)
        self.assertEqual(result.stdout, "")

    def test_next_link_cannot_escape_catalog_or_loop(self):
        for target in ("https://elsewhere.example/releases?page=2", API.replace("releases?", "issues?"), API,
                       "https://elsewhere.example/repositories/1239190708/releases?page=2",
                       "https://api.github.com/repositories/1239190708/issues?page=2",
                       "https://api.github.com/repositories/1239190708/releases/latest?page=2",
                       "https://api.github.com/repositories/memql/releases?page=2"):
            with self.subTest(target=target):
                self.calls.unlink(missing_ok=True)
                self.response(API, [release("cockpit/v0.19.0")], f'Link: <{target}>; rel="next"\r\n')
                result = self.resolve()
                self.assertEqual(result.returncode, 4)
                self.assertEqual(len(self.requested()), 1)

    # The header shape api.github.com sent for this catalog on 2026-10-30: a
    # request on /repos/<owner>/<name> is paged on the repository's numeric path.
    # Every install failed on it once the catalog passed 100 releases.
    def test_next_link_may_use_github_numeric_repository_path(self):
        numeric = "https://api.github.com/repositories/1239190708/releases?per_page=100"
        second, third = numeric + "&page=2", numeric + "&page=3"
        self.response(API, [release("v99.0." + str(i)) for i in range(100)],
                      f'HTTP/2 200\r\nLink: <{second}>; rel="next", <{third}>; rel="last"\r\n')
        self.response(second, [release("cockpit/v0.21.1")],
                      f'HTTP/2 200\r\nLink: <{numeric}&page=1>; rel="prev", <{third}>; rel="next", '
                      f'<{third}>; rel="last", <{numeric}&page=1>; rel="first"\r\n')
        self.response(third, [release("cockpit/v0.20.5")],
                      f'HTTP/2 200\r\nLink: <{second}>; rel="prev", <{numeric}&page=1>; rel="first"\r\n')
        result = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "cockpit/v0.21.1")
        self.assertEqual([row["url"] for row in self.requested()], [API, second, third])

    def test_numeric_next_link_cannot_change_repository(self):
        numeric = "https://api.github.com/repositories/1239190708/releases?per_page=100"
        other = "https://api.github.com/repositories/42/releases?per_page=100&page=3"
        self.response(API, [release("cockpit/v0.19.0")], f'Link: <{numeric}&page=2>; rel="next"\r\n')
        self.response(numeric + "&page=2", [release("cockpit/v0.21.1")], f'Link: <{other}>; rel="next"\r\n')
        self.response(other, [release("cockpit/v9.9.9")])
        result = self.resolve()
        self.assertEqual(result.returncode, 4)
        self.assertEqual(result.stdout, "")
        self.assertEqual(len(self.requested()), 2)

    def test_redirect_response_cannot_invent_a_next_page(self):
        self.response(API, [release("cockpit/v0.20.1")],
                      f'HTTP/2 302\r\nLink: <{API}&page=2>; rel="next"\r\n\r\nHTTP/2 200\r\n')
        result = self.resolve()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.requested()), 1)

    def test_extended_and_unquoted_next_links_are_followed_and_malformed_refuses(self):
        second = API + "&page=2"
        self.response(second, [release("cockpit/v0.21.0")])
        for link in (f'<{second}>; rel="next"; type="application/json"',
                     f'<{second}>; type="application/json"; rel=next'):
            self.response(API, [release("cockpit/v0.20.1")], "Link: " + link + "\r\n")
            result = self.resolve()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), "cockpit/v0.21.0")
        self.response(API, [release("cockpit/v0.20.1")], f'Link: <{second}>; rel="next\r\n')
        result = self.resolve()
        self.assertEqual(result.returncode, 4)
        self.assertEqual(result.stdout, "")

    def test_shared_selector_serves_library_and_piped_posix_entry(self):
        self.response(API, [release("cockpit/v0.20.1")])
        result = self.run_command(["bash", "-c", 'source "$1/lib.sh"; resolve_latest_version', "fixture", str(HERE)])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "0.20.1")
        self.response(RAW + "/release-version.py", (HERE / "release-version.py").read_bytes())
        poison = self.root / "scripts" / "install" / "release-version.py"
        poison.parent.mkdir(parents=True)
        poison.write_text('raise SystemExit("UNTRUSTED CURRENT DIRECTORY HELPER")\n')
        (self.root / "sh").write_text("unrelated current-directory file\n")
        source = ROOT_INSTALL.read_text().removesuffix('main "$@"\n')
        result = self.run_command(["sh", "-s"], source + "\nresolve_version\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "cockpit/v0.20.1")
        fetched = [row for row in self.requested() if row["url"].endswith("release-version.py")]
        self.assertEqual(len(fetched), 1)
        self.assertFalse(Path(fetched[0]["args"][fetched[0]["args"].index("-o") + 1]).exists())

    def test_piped_fleet_drivers_load_same_helper_and_choose_platform_variant(self):
        self.response(RAW + "/lib.sh", (HERE / "lib.sh").read_bytes())
        self.response(RAW + "/release-version.py", (HERE / "release-version.py").read_bytes())
        self.response(API, [release("cockpit/v0.20.1")])
        temporary = self.root / "temporary"
        temporary.mkdir()
        self.env["TMPDIR"] = str(temporary)
        (temporary / "release-version.py").write_text('raise SystemExit("UNTRUSTED TEMPORARY DIRECTORY HELPER")\n')
        # Match the library's required-function check, so the old cwd lookup
        # would actually execute this poison instead of merely rejecting it.
        (self.root / "lib.sh").write_text((HERE / "lib.sh").read_text() +
                                        '\necho "UNTRUSTED CURRENT DIRECTORY LIBRARY" >&2; exit 97\n')
        (self.root / "release-version.py").write_text('raise SystemExit("UNTRUSTED CURRENT DIRECTORY HELPER")\n')
        for system, arch, os_name, arch_name, driver in [
            ("Darwin", "arm64", "darwin", "arm64", "install-mac.sh"),
            ("Darwin", "x86_64", "darwin", "amd64", "install-mac.sh"),
            ("Linux", "aarch64", "linux", "arm64", "install-linux.sh"),
            ("Linux", "x86_64", "linux", "amd64", "install-linux.sh"),
        ]:
            uname = self.bin / "uname"
            uname.write_text(f'#!/bin/sh\ncase "$1" in -s) echo {system};; -m) echo {arch};; esac\n')
            uname.chmod(0o755)
            for variant in ("headless", "computeruse"):
                with self.subTest(system=system, arch=arch, variant=variant):
                    self.calls.unlink(missing_ok=True)
                    args = ["bash", "-s", "--", "--token", "mql_wkr_fixture", "--cluster", "https://api.example.test",
                            "--user-local", "--no-service", "--no-menu", "--verbose"]
                    if variant == "computeruse": args.append("--computeruse")
                    result = self.run_command(args, (HERE / driver).read_text())
                    self.assertEqual(result.returncode, 4, result.stderr + result.stdout)
                    names = [row["url"] for row in self.requested()]
                    infix = "computeruse-" if variant == "computeruse" else ""
                    expected = f"https://github.com/znasllc-io/memql/releases/download/cockpit/v0.20.1/memql-{infix}{os_name}-{arch_name}"
                    self.assertIn(expected, names)
                    self.assertIn(RAW + "/release-version.py", names)
                    self.assertIn(RAW + "/lib.sh", names)
                    self.assertNotIn("UNTRUSTED", result.stdout + result.stderr)
                    self.assertFalse((Path(self.env["HOME"]) / ".memql" / "workers.yaml").exists())
                    for row in self.requested():
                        if row["url"].endswith(("/lib.sh", "/release-version.py")):
                            self.assertFalse(Path(row["args"][row["args"].index("-o") + 1]).exists())

    def test_explicit_loopback_fixture_has_no_redirect_permission(self):
        self.env["MEMQL_INSTALL_RELEASE_API"] = "http://127.0.0.1:9999/releases"
        self.response(self.env["MEMQL_INSTALL_RELEASE_API"], [release("cockpit/v0.20.1")])
        denied = self.resolve()
        self.assertEqual(denied.returncode, 4)
        self.assertEqual(self.requested(), [])
        self.env["MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP"] = "1"
        accepted = self.resolve()
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        args = self.requested()[0]["args"]
        self.assertEqual(args[args.index("--proto") + 1], "=http")
        self.assertEqual(args[args.index("--max-redirs") + 1], "0")
        raw = "http://127.0.0.1:9999/raw"
        self.env["MEMQL_INSTALL_RAW_BASE"] = raw
        self.response(raw + "/lib.sh", (HERE / "lib.sh").read_bytes())
        self.response(raw + "/release-version.py", (HERE / "release-version.py").read_bytes())
        result = self.run_command(["bash", "-s", "--", "--token", "mql_wkr_fixture", "--cluster",
                                   "https://api.example.test", "--user-local", "--no-service", "--no-menu"],
                                  (HERE / "install-mac.sh").read_text())
        self.assertEqual(result.returncode, 4, result.stderr + result.stdout)
        helper_fetch = next(row for row in self.requested() if row["url"] == raw + "/release-version.py")
        args = helper_fetch["args"]
        self.assertEqual(args[args.index("--proto") + 1], "=http")
        self.assertEqual(args[args.index("--max-redirs") + 1], "0")
        self.assertFalse(Path(args[args.index("-o") + 1]).exists())

    def test_standalone_requires_valid_archive_checksum_before_installing(self):
        uname = self.bin / "uname"
        uname.write_text('#!/bin/sh\ncase "$1" in -s) echo Linux;; -m) echo x86_64;; esac\n')
        uname.chmod(0o755)
        self.env["MEMQL_COCKPIT_VERSION"] = "v0.20.1"
        archive = io.BytesIO()
        binary = b"#!/bin/sh\nexit 1\n"
        with tarfile.open(fileobj=archive, mode="w:gz") as tar:
            info = tarfile.TarInfo("./memql")  # The member name produced by make dist.
            info.size = len(binary)
            info.mode = 0o755
            tar.addfile(info, io.BytesIO(binary))
        content = archive.getvalue()
        base = "https://github.com/znasllc-io/memql/releases/download/cockpit/v0.20.1/"
        asset = "memql-0.20.1-linux-amd64.tar.gz"
        sums = base + "memql-0.20.1-SHA256SUMS"
        self.response(base + asset, content)
        for manifest, exit_code in [("", 22), ("0" * 64 + "  " + asset + "\n", 0)]:
            self.response(sums, manifest, exit_code=exit_code)
            result = self.run_command(["sh", str(ROOT_INSTALL)])
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse((Path(self.env["BIN_DIR"]) / "memql").exists())
            self.assertNotIn(API, [row["url"] for row in self.requested()])
        self.response(sums, hashlib.sha256(content).hexdigest() + "  " + asset + "\n")
        result = self.run_command(["sh", str(ROOT_INSTALL)])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((Path(self.env["BIN_DIR"]) / "memql").read_bytes(), binary)

    def test_standalone_never_falls_back_to_an_unchecked_raw_binary(self):
        uname = self.bin / "uname"
        uname.write_text('#!/bin/sh\ncase "$1" in -s) echo Darwin;; -m) echo arm64;; esac\n')
        uname.chmod(0o755)
        self.env["MEMQL_COCKPIT_VERSION"] = "v0.20.1"
        self.response("https://github.com/znasllc-io/memql/releases/download/cockpit/v0.20.1/memql-darwin-arm64",
                      b"#!/bin/sh\nexit 0\n")
        result = self.run_command(["sh", str(ROOT_INSTALL)])
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(Path(self.env["BIN_DIR"]).exists())
        self.assertTrue(all(row["url"].endswith(".tar.gz") for row in self.requested()))


if __name__ == "__main__":
    unittest.main()
