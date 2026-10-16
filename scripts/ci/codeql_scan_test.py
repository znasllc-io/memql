from pathlib import Path
import hashlib
import importlib.util
import io
import json
import platform
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock
import urllib.error

sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("codeqlscan", ROOT / "scripts/ci/codeql-scan.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def sarif(results=None, successful=True):
    return {"version": "2.1.0", "runs": [{
        "results": results or [], "invocations": [{"executionSuccessful": successful}],
    }]}


class FakeResponse:
    """An urlopen answer that hands out chunks, then raises or ends."""

    def __init__(self, chunks, failure=None):
        self.chunks = list(chunks)
        self.failure = failure

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False

    def read(self, size=-1):
        if self.chunks:
            return self.chunks.pop(0)
        if self.failure:
            raise self.failure
        return b""


class BundleDownload(unittest.TestCase):
    """The bundle is several hundred megabytes, fetched three times a run."""

    content = b"codeql-bundle" * 4096

    def install(self, answers):
        calls, archives = [], []

        def urlopen(url, timeout):
            calls.append(url)
            answer = answers[min(len(calls), len(answers)) - 1]
            if isinstance(answer, Exception):
                raise answer
            return answer

        def extract(archive, destination):
            archives.append(archive.read_bytes())

        digest = hashlib.sha256(self.content).hexdigest()
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(module.urllib.request, "urlopen", urlopen), \
                mock.patch.object(module.platform, "system", lambda: "Linux"), \
                mock.patch.object(module, "BUNDLES", {platform.machine(): ("linux64", digest)}), \
                mock.patch.object(module, "extract_bundle", extract), \
                mock.patch("time.sleep"):
            try:
                module.install_bundle(Path(directory))
                return calls, archives, None
            except Exception as exc:  # the caller judges which failures are right
                return calls, archives, exc

    # Run 6 of epic #6011: both bundle downloads stalled past the 60-second
    # read timeout at once, and two analyses failed a 5-hour run.
    def test_a_stalled_read_is_fetched_again(self):
        stalled = FakeResponse([self.content[:100]], TimeoutError("The read operation timed out"))
        calls, archives, failure = self.install([stalled, FakeResponse([self.content])])
        self.assertIsNone(failure, "a stalled read was not fetched again")
        self.assertEqual(len(calls), 2)
        self.assertEqual(archives, [self.content])

    def test_a_download_that_keeps_failing_stops_after_three_attempts(self):
        calls, _, failure = self.install([TimeoutError("The read operation timed out")])
        self.assertIsInstance(failure, RuntimeError)
        self.assertIn("3 attempts", str(failure))
        self.assertEqual(len(calls), 3)

    def test_a_wrong_digest_is_never_fetched_again(self):
        calls, _, failure = self.install([FakeResponse([b"not the pinned bundle"])])
        self.assertIsInstance(failure, RuntimeError)
        self.assertIn("SHA-256", str(failure))
        self.assertEqual(len(calls), 1)

    def test_a_missing_release_is_never_fetched_again(self):
        missing = urllib.error.HTTPError("https://github.com/", 404, "Not Found", None, None)
        calls, _, failure = self.install([missing])
        self.assertIsInstance(failure, RuntimeError)
        self.assertEqual(len(calls), 1)


class Contract(unittest.TestCase):
    def test_report_requires_completed_analysis(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "report.sarif"
            invalid = [{}, {"version": "2.1.0", "runs": []},
                       {"version": "2.1.0", "runs": [{"results": []}]},
                       sarif(successful=False)]
            for report in invalid:
                path.write_text(json.dumps(report))
                with self.assertRaises(RuntimeError):
                    module.validate_sarif(path)
            path.write_text(json.dumps(sarif([{"ruleId": "example"}])))
            self.assertEqual(module.validate_sarif(path), 1)

    def test_archive_cannot_create_files_outside_owned_directory(self):
        for attack in ("parent", "absolute", "link", "chain", "device", "duplicate"):
            with self.subTest(attack=attack), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                archive = root / "bundle.tar"
                destination = root / "out"
                destination.mkdir()
                with tarfile.open(archive, "w") as bundle:
                    entries = []
                    if attack in ("parent", "absolute"):
                        entries.append(tarfile.TarInfo("../escaped" if attack == "parent" else str(root / "escaped")))
                    elif attack in ("link", "chain"):
                        first = tarfile.TarInfo("first")
                        first.type = tarfile.SYMTYPE
                        first.linkname = "second" if attack == "chain" else "../escaped"
                        entries.append(first)
                        if attack == "chain":
                            second = tarfile.TarInfo("second")
                            second.type, second.linkname = tarfile.SYMTYPE, "../escaped"
                            entries.append(second)
                    elif attack == "device":
                        device = tarfile.TarInfo("device")
                        device.type = tarfile.CHRTYPE
                        entries.append(device)
                    else:
                        entries = [tarfile.TarInfo("same"), tarfile.TarInfo("same")]
                    for entry in entries:
                        bundle.addfile(entry, io.BytesIO(b""))
                with self.assertRaises(RuntimeError):
                    module.extract_bundle(archive, destination)
                self.assertFalse((root / "escaped").exists())

    def test_archive_preserves_executable_and_internal_links(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            archive = root / "bundle.tar"
            destination = root / "out"
            destination.mkdir()
            with tarfile.open(archive, "w") as bundle:
                for name, kind in (("sym", tarfile.SYMTYPE), ("hard", tarfile.LNKTYPE)):
                    link = tarfile.TarInfo("codeql/" + name)
                    link.type = kind
                    link.linkname = "cli" if kind == tarfile.SYMTYPE else "codeql/cli"
                    bundle.addfile(link)
                entry = tarfile.TarInfo("codeql/cli")
                entry.mode, entry.size = 0o4755, 3
                bundle.addfile(entry, io.BytesIO(b"cli"))
            module.extract_bundle(archive, destination)
            for name in ("cli", "sym", "hard"):
                self.assertEqual((destination / "codeql" / name).read_bytes(), b"cli")
            self.assertEqual((destination / "codeql/cli").stat().st_mode & 0o7777, 0o755)

    def test_cli_runs_declared_suite_and_rejects_stale_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake, calls = root / "codeql", root / "calls"
            suite = root / "qlpacks/codeql/python-queries/1.0.0/codeql-suites/python-code-quality.qls"
            suite.parent.mkdir(parents=True)
            suite.write_text("- queries: .\n")
            fake.write_text('''#!/usr/bin/env python3
import sys,json,pathlib
with open(''' + repr(str(calls)) + ''','a') as out: out.write(json.dumps(sys.argv[1:])+'\\n')
if sys.argv[1]=='version': print('{"version":"2.27.1"}')
elif sys.argv[1:3]==['database','create']:
 import yaml
 config=next(a.split('=',1)[1] for a in sys.argv if a.startswith('--codescanning-config='))
 obj=yaml.safe_load(pathlib.Path(config).read_text())
 assert obj['queries']==[{'uses':'security-extended'}]
 assert obj['packs']==['codeql/python-queries@1.0.0:codeql-suites/python-code-quality.qls']
 assert any(f['exclude']['id']=='go/bad-redirect-check' for f in obj['query-filters'])
elif sys.argv[1:3]==['database','analyze']:
 out=next(a.split('=',1)[1] for a in sys.argv if a.startswith('--output='))
 pathlib.Path(out).write_text(json.dumps({'version':'2.1.0','runs':[{'results':[],'invocations':[{'executionSuccessful':True}]}]}))
''')
            fake.chmod(0o755)
            command = ["python3", str(ROOT / "scripts/ci/codeql-scan.py"), "--language=python",
                       "--codeql=" + str(fake), "--output-dir=" + str(root / "out")]
            result = subprocess.run(command, capture_output=True, text=True, cwd=ROOT)
            self.assertEqual(result.returncode, 0, result.stderr)
            summary = json.loads((root / "out/summary.json").read_text())
            self.assertEqual(summary["status"], "analyzed")
            self.assertEqual(summary["querySuites"], ["security-extended", "python-code-quality.qls"])
            self.assertEqual(len(calls.read_text().splitlines()), 3)
            result = subprocess.run(command, capture_output=True, text=True, cwd=ROOT)
            self.assertNotEqual(result.returncode, 0)

    def test_missing_or_ambiguous_quality_suite_is_not_a_security_only_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for language in ("go", "javascript-typescript", "python"):
                with self.subTest(language=language):
                    with self.assertRaises(RuntimeError):
                        module.quality_suite(root / "codeql", language)
                    pack = "javascript" if language == "javascript-typescript" else language
                    suite = root / f"qlpacks/codeql/{pack}-queries/1.0.0/codeql-suites/{pack}-code-quality.qls"
                    suite.parent.mkdir(parents=True)
                    suite.write_text("- queries: .\n")
                    self.assertEqual(module.quality_suite(root / "codeql", language), suite.resolve())
                    second = root / f"qlpacks/codeql/{pack}-queries/2.0.0/codeql-suites/{pack}-code-quality.qls"
                    second.parent.mkdir(parents=True)
                    second.write_text("- queries: .\n")
                    with self.assertRaises(RuntimeError):
                        module.quality_suite(root / "codeql", language)


if __name__ == "__main__":
    unittest.main()
