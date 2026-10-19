import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import yaml

SPEC = importlib.util.spec_from_file_location("scorecard_scan", Path(__file__).with_name("scorecard-scan.py"))
SCAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SCAN)
SHA = "a" * 40


def report():
    return {"repo": {"name": "github.com/acme/widget", "commit": SHA},
            "scorecard": {"version": "v5.5.0"},
            "checks": [{"name": name, "score": 10, "reason": "passed"} for name in sorted(SCAN.CHECKS)]}


class ScorecardTests(unittest.TestCase):
    def test_policy_preserves_the_retiring_actions_enabled_checks(self):
        raw = SCAN.POLICY.read_bytes()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), SCAN.POLICY_SHA256)
        policies = yaml.safe_load(raw)["policies"]
        self.assertEqual(set(policies), SCAN.CHECKS)
        self.assertEqual({k for k, v in policies.items() if v["mode"] == "disabled"}, {"Contributors", "Signed-Releases"})

    def test_full_audit_and_findings_are_not_an_execution_failure(self):
        data = report()
        data["checks"][0]["score"] = 0
        data["checks"][1].update(score=-1, reason="no applicable releases")
        self.assertEqual(SCAN.validate_results(data, "acme/widget", SHA), [data["checks"][1]["name"]])

    def test_historical_commit_subset_is_not_full_coverage(self):
        data = report()
        data["checks"] = [c for c in data["checks"] if c["name"] != "Branch-Protection"]
        with self.assertRaisesRegex(ValueError, "complete policy check set"):
            SCAN.validate_results(data, "acme/widget", SHA)

    def test_wrong_source_duplicate_checks_version_and_auth_failure_refuse(self):
        variants = []
        data = report(); data["repo"]["commit"] = "b" * 40; variants.append(data)
        data = report(); data["repo"]["name"] = "github.com/other/repo"; variants.append(data)
        data = report(); data["checks"][-1] = copy.deepcopy(data["checks"][0]); variants.append(data)
        data = report(); data["scorecard"]["version"] = "v0.0.0"; variants.append(data)
        data = report(); data["checks"][0]["reason"] = "internal error: Resource not accessible by integration"; variants.append(data)
        data = report(); data["checks"][0]["score"] = True; variants.append(data)
        data = report(); data["checks"][0] = None; variants.append(data)
        for data in variants:
            with self.subTest(data=data), self.assertRaises(ValueError):
                SCAN.validate_results(data, "acme/widget", SHA)

    def test_bounded_process_captures_both_pipes_and_failure(self):
        code, out, err = SCAN.run_bounded([sys.executable, "-c", "import sys; print('out'); print('err',file=sys.stderr); sys.exit(7)"], os.environ)
        self.assertEqual((code, out, err), (7, b"out\n", b"err\n"))

    def test_bounded_process_stops_oversize_and_timeout(self):
        for command, kwargs, message in [
            ("import sys; sys.stdout.write('x'*100000)", {"maximum": 100}, "byte bound"),
            ("import sys; sys.stderr.write('x'*100000)", {"maximum": 100}, "byte bound"),
            ("import time; time.sleep(30)", {"timeout": 0.1}, "execution bound"),
        ]:
            with self.subTest(command=command), self.assertRaisesRegex(ValueError, message):
                SCAN.run_bounded([sys.executable, "-c", command], os.environ, **kwargs)

    def test_head_change_after_scan_prevents_success_receipt(self):
        secret = "fixture-read-only-token"
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA, "MEMQL_GITHUB_READ_TOKEN": secret}), \
                 mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                 mock.patch.object(SCAN, "head", side_effect=[SHA, "b" * 40]), \
                 mock.patch.object(SCAN, "run_bounded", return_value=(0, json.dumps(report()).encode(), secret.encode())):
                self.assertEqual(SCAN.main(), 1)
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(summary["status"], "failed")
            self.assertIn("HEAD changed", summary["error"])
            self.assertEqual((output / "json.log").read_text(), "[redacted]")
            self.assertFalse((output / "results.sarif").exists())

    def test_sarif_identity_and_findings(self):
        data = {"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "Scorecard", "semanticVersion": "v5.5.0"}}, "results": [{"ruleId": "test"}]}]}
        self.assertEqual(SCAN.validate_sarif(data), 1)
        data["runs"][0]["tool"]["driver"]["name"] = "other"
        with self.assertRaisesRegex(ValueError, "different analyzer"):
            SCAN.validate_sarif(data)

    def test_bootstrap_refuses_changed_bytes(self):
        with tempfile.TemporaryDirectory() as temporary, \
             mock.patch.object(SCAN.platform, "system", return_value="Linux"), \
             mock.patch.object(SCAN.platform, "machine", return_value="x86_64"), \
             mock.patch.object(SCAN.urllib.request, "urlopen", return_value=io.BytesIO(b"changed")):
            with self.assertRaisesRegex(ValueError, "pinned SHA-256"):
                SCAN.install_binary(Path(temporary))
            self.assertFalse((Path(temporary) / "scorecard").exists())

    def test_bootstrap_never_extracts_a_link_as_its_executable(self):
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as tar:
            member = tarfile.TarInfo("scorecard")
            member.type, member.linkname = tarfile.SYMTYPE, "../../outside"
            tar.addfile(member)
        data = archive.getvalue()
        with tempfile.TemporaryDirectory() as temporary, \
             mock.patch.object(SCAN.platform, "system", return_value="Linux"), \
             mock.patch.object(SCAN.platform, "machine", return_value="x86_64"), \
             mock.patch.dict(SCAN.ASSETS, {"x86_64": ("amd64", hashlib.sha256(data).hexdigest())}), \
             mock.patch.object(SCAN.urllib.request, "urlopen", return_value=io.BytesIO(data)):
            with self.assertRaisesRegex(ValueError, "regular executable"):
                SCAN.install_binary(Path(temporary))
            self.assertFalse((Path(temporary) / "scorecard").exists())


if __name__ == "__main__":
    unittest.main()
