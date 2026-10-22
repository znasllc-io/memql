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
    def test_empty_report_after_transient_github_query_is_retried_with_evidence(self):
        transient = b"Skipping github.com/acme/widget: run: internal error: githubv4.Query: Something went wrong while executing your query"
        raw = {"version": "2.1.0", "runs": [{"tool": {"driver": {
            "name": "Scorecard", "semanticVersion": "v5.5.0"}}, "results": []}]}
        secret = "fixture-read-only-token"
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA,
                                              "MEMQL_GITHUB_READ_TOKEN": secret}), \
                 mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                 mock.patch.object(SCAN, "head", return_value=SHA), \
                 mock.patch.object(SCAN.time, "sleep") as sleep, \
                 mock.patch.object(SCAN, "run_bounded", side_effect=[
                     (0, json.dumps(report()).encode(), b""),
                     (0, b"", transient + b" " + secret.encode()),
                     (0, json.dumps(raw).encode(), b"")]) as run:
                self.assertEqual(SCAN.main(), 0)
            self.assertEqual(run.call_count, 3)
            sleep.assert_called_once_with(5)
            saved = output / "attempts" / "sarif-1.log"
            self.assertIn("Something went wrong", saved.read_text())
            self.assertNotIn(secret, saved.read_text())
            self.assertEqual(json.loads((output / "summary.json").read_text())["status"], "analyzed")

    def test_transient_query_retries_are_bounded_and_never_accept_an_empty_report(self):
        transient = b"Skipping github.com/acme/widget: run: internal error: githubv4.Query: Something went wrong while executing your query"
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA,
                                              "MEMQL_GITHUB_READ_TOKEN": "fixture"}), \
                 mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                 mock.patch.object(SCAN, "head", return_value=SHA), \
                 mock.patch.object(SCAN.time, "sleep") as sleep, \
                 mock.patch.object(SCAN, "run_bounded", return_value=(0, b"", transient)) as run:
                self.assertEqual(SCAN.main(), 1)
            self.assertEqual(run.call_count, 3)
            self.assertEqual(sleep.call_count, 2)
            self.assertEqual((output / "json.log").read_bytes(), transient)
            self.assertFalse((output / "results.raw.sarif").exists())

    def test_transient_retry_rechecks_source_and_keeps_original_execution_bound(self):
        transient = b"Skipping github.com/acme/widget: run: internal error: githubv4.Query: Something went wrong while executing your query"
        for mode in ("source moved", "time exhausted", "smaller remaining budget"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "output"
                heads = [SHA, "b" * 40] if mode == "source moved" else [SHA] * 5
                times = [0, 0, 596] if mode == "time exhausted" else [0, 0, 20, 25]
                with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA,
                                                  "MEMQL_GITHUB_READ_TOKEN": "fixture"}), \
                     mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                     mock.patch.object(SCAN, "head", side_effect=heads), \
                     mock.patch.object(SCAN.time, "monotonic", side_effect=times), \
                     mock.patch.object(SCAN.time, "sleep"), \
                     mock.patch.object(SCAN, "run_bounded", side_effect=[(0, b"", transient), (1, b"", b"permanent failure")]) as run:
                    self.assertEqual(SCAN.main(), 1)
                if mode == "smaller remaining budget":
                    self.assertEqual(run.call_count, 2)
                    self.assertEqual(run.call_args.kwargs["timeout"], 575)
                else:
                    self.assertEqual(run.call_count, 1)

    def test_other_empty_reports_and_permission_failures_are_not_retried(self):
        for stderr in (b"", b"Resource not accessible by integration", b"Bad credentials"):
            with self.subTest(stderr=stderr), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / "output"
                with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA,
                                                  "MEMQL_GITHUB_READ_TOKEN": "fixture"}), \
                     mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                     mock.patch.object(SCAN, "head", return_value=SHA), \
                     mock.patch.object(SCAN.time, "sleep") as sleep, \
                     mock.patch.object(SCAN, "run_bounded", return_value=(0, b"", stderr)) as run:
                    self.assertEqual(SCAN.main(), 1)
                self.assertEqual(run.call_count, 1)
                sleep.assert_not_called()

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

    def test_sarif_normalization_preserves_findings_and_existing_escapes(self):
        uris = ["no file associated with this alert", "src/already%20escaped.go",
                "src/100% complete.go", "src/caf\u00e9.go"]
        findings = [{"ruleId": "CodeReviewID", "level": "warning",
                     "message": {"text": "Keep this finding unchanged."},
                     "locations": [{"physicalLocation": {
                         "artifactLocation": {"uri": uri, "uriBaseId": "%SRCROOT%"},
                         "region": {"startLine": 1}}}]} for uri in uris]
        data = {"version": "2.1.0", "runs": [{
            "automationDetails": {"id": "supply-chain/online-scm/run"},
            "tool": {"driver": {"name": "Scorecard", "semanticVersion": "v5.5.0",
                                 "rules": [{"id": "CodeReviewID"}]}},
            "results": findings}]}
        expected = copy.deepcopy(data)
        expected["runs"][0]["tool"]["driver"]["semanticVersion"] = "5.5.0"
        encoded = ["no%20file%20associated%20with%20this%20alert", "src/already%20escaped.go",
                   "src/100%25%20complete.go", "src/caf%C3%A9.go"]
        for finding, uri in zip(expected["runs"][0]["results"], encoded):
            finding["locations"][0]["physicalLocation"]["artifactLocation"]["uri"] = uri
        self.assertEqual(SCAN.normalize_sarif(data), {"artifactURIs": 3, "semanticVersions": 1})
        self.assertEqual(data, expected)

    def test_success_retains_raw_report_and_hashes_both_renditions(self):
        raw = {"version": "2.1.0", "runs": [{
            "tool": {"driver": {"name": "Scorecard", "semanticVersion": "v5.5.0"}},
            "results": [{"ruleId": "BranchProtectionID", "locations": [{
                "physicalLocation": {"artifactLocation": {
                    "uri": "no file associated with this alert", "uriBaseId": "%SRCROOT%"}}}]}]}]}
        raw_bytes = json.dumps(raw).encode()
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            with mock.patch.dict(os.environ, {"MEMQL_REPOSITORY": "acme/widget", "MEMQL_SHA": SHA,
                                              "MEMQL_GITHUB_READ_TOKEN": "fixture-read-only-token"}), \
                 mock.patch.object(sys, "argv", ["scorecard-scan", "--output-dir", str(output), "--scorecard", "/unused"]), \
                 mock.patch.object(SCAN, "head", return_value=SHA), \
                 mock.patch.object(SCAN, "run_bounded", side_effect=[
                     (0, json.dumps(report()).encode(), b""), (0, raw_bytes, b"")]):
                self.assertEqual(SCAN.main(), 0)
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual(summary["findings"], 1)
            self.assertEqual(summary["checkCount"], 18)
            self.assertEqual((output / "results.raw.sarif").read_bytes(), raw_bytes)
            self.assertEqual(summary["rawSarifSHA256"], hashlib.sha256(raw_bytes).hexdigest())
            self.assertEqual(summary["sarifSHA256"], hashlib.sha256((output / "results.sarif").read_bytes()).hexdigest())
            self.assertNotEqual(summary["rawSarifSHA256"], summary["sarifSHA256"])
            normalized = json.loads((output / "results.sarif").read_text())
            self.assertEqual(normalized["runs"][0]["tool"]["driver"]["semanticVersion"], "5.5.0")

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
