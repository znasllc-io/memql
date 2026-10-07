import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("history", Path(__file__).with_name("gitleaks-history.py"))
history = importlib.util.module_from_spec(spec)
spec.loader.exec_module(history)


class HistoryTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        (self.repo / "file").write_text("initial\n")
        self.git("add", "file")
        self.git("commit", "-qm", "initial")
        self.initial = self.git("rev-parse", "HEAD")
        self.git("checkout", "-qb", "tag-only")
        (self.repo / "file").write_text("tag ancestry\n")
        self.git("commit", "-qam", "tag-only")
        self.tag_commit = self.git("rev-parse", "HEAD")
        self.git("tag", "vfixture")
        self.git("checkout", "-q", "main")
        (self.repo / "file").write_text("candidate\n")
        self.git("commit", "-qam", "candidate")
        self.head = self.git("rev-parse", "HEAD")
        self.config = self.root / "config.toml"
        self.config.write_text("[extend]\nuseDefault = true\n")
        self.output = self.root / "evidence"
        self.output.mkdir()
        self.scanner = self.root / "gitleaks"
        self.scanner.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys, time
if sys.argv[1] == "version":
    print("8.30.1")
    sys.exit(0)
args = dict(arg.split("=", 1) for arg in sys.argv[2:] if arg.startswith("--") and "=" in arg)
opts = args["--log-opts"].split()
assert all(flag in opts for flag in ("--no-walk=unsorted", "--root", "--diff-merges=first-parent", "--no-ext-diff", "--no-textconv"))
commits = [arg for arg in opts if not arg.startswith("--")]
fault = os.getenv("SCANNER_FAULT", "")
if fault == "crash": sys.exit(137)
if fault == "mixed":
    if commits[0] == os.getenv("SCANNER_FAIL_COMMIT"): sys.exit(137)
    time.sleep(30)
if fault == "missing": sys.exit(0)
if fault == "hang": time.sleep(30)
report = [{"Commit": commits[0], "Secret": "REDACTED"}] if fault in ("findings", "partial") else []
if fault == "outside": report = [{"Commit": "0" * 40, "Secret": "REDACTED"}]
pathlib.Path(args["--report-path"]).write_text(json.dumps(report))
if fault == "partial": sys.exit(1)
sys.exit(int(args["--exit-code"]) if fault in ("findings", "disagreement", "outside") else 0)
''')
        self.scanner.chmod(0o700)

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], text=True).strip()

    def run_scan(self, jobs=1):
        return history.scan(self.repo, self.head, self.output, self.config, str(self.scanner), 1, 20, jobs)

    def test_complete_inventory_includes_root_and_tag_only_ancestry(self):
        result = self.run_scan()
        self.assertEqual(result["status"], "clean")
        self.assertEqual(result["completedCommits"], 3)
        self.assertEqual(set((self.output / "commits.txt").read_text().splitlines()),
                         {self.initial, self.tag_commit, self.head})
        self.assertEqual(len(result["batches"]), 3)
        self.assertTrue(all(len(batch["reportSHA256"]) == 64 for batch in result["batches"]))

    def test_findings_are_complete_but_never_clean(self):
        with patch.dict(os.environ, SCANNER_FAULT="findings"):
            result = self.run_scan()
        self.assertEqual(result["status"], "findings")
        self.assertEqual(result["completedCommits"], 3)
        self.assertEqual(len(json.loads((self.output / "findings.json").read_text())), 3)

    def test_crash_missing_report_and_exit_disagreement_are_incomplete(self):
        for fault in ("crash", "missing", "disagreement", "partial", "outside"):
            with self.subTest(fault=fault), patch.dict(os.environ, SCANNER_FAULT=fault):
                self.output = self.root / fault
                self.output.mkdir()
                result = self.run_scan()
                self.assertEqual(result["status"], "incomplete")
                self.assertEqual(result["completedCommits"], 0)

    def test_parallel_scanners_preserve_inventory_and_bound_active_processes(self):
        original = history.run_scanner
        lock = threading.Lock()
        active = peak = 0
        completion_order = []
        barrier = threading.Barrier(2)
        later_finished = threading.Event()

        def measured(command, log, remaining, cancelled):
            nonlocal active, peak
            with lock:
                active += 1
                peak = max(peak, active)
            report = next(arg.split("=", 1)[1] for arg in command if arg.startswith("--report-path="))
            number = int(Path(report).stem.split("-")[1])
            try:
                if number < 2:
                    barrier.wait(timeout=5)
                if number == 0 and not later_finished.wait(timeout=5):
                    raise RuntimeError("later batch did not finish concurrently")
                code = original(command, log, remaining, cancelled)
                with lock:
                    completion_order.append(number)
                if number == 1:
                    later_finished.set()
                return code
            finally:
                with lock:
                    active -= 1

        with patch.object(history, "run_scanner", measured):
            result = self.run_scan(jobs=2)
        self.assertEqual(result["status"], "clean")
        self.assertEqual(result["completedCommits"], 3)
        self.assertEqual(peak, 2)
        self.assertEqual(active, 0)
        self.assertEqual(completion_order[0], 1)
        self.assertEqual([b["firstIndex"] for b in result["batches"]], [0, 1, 2])
        self.assertEqual(len(list(self.output.glob("batch-*.json"))), 3)

    def test_parallel_failure_cancels_other_scanners_before_deadline(self):
        started = time.monotonic()
        with patch.dict(os.environ, SCANNER_FAULT="mixed", SCANNER_FAIL_COMMIT=self.head):
            result = self.run_scan(jobs=2)
        self.assertEqual(result["status"], "incomplete")
        self.assertEqual(result["completedCommits"], 0)
        self.assertLess(time.monotonic() - started, 10)  # Overall deadline is 20s.

    def test_parallel_report_failure_never_completes_coverage(self):
        for fault in ("missing", "disagreement", "partial", "outside"):
            with self.subTest(fault=fault), patch.dict(os.environ, SCANNER_FAULT=fault):
                self.output = self.root / ("parallel-" + fault)
                self.output.mkdir()
                result = self.run_scan(jobs=2)
                self.assertEqual(result["status"], "incomplete")
                self.assertEqual(result["completedCommits"], 0)

    def test_shallow_history_is_refused_before_scanning(self):
        shallow = self.root / "shallow"
        subprocess.run(["git", "clone", "-q", "--depth=1", self.repo.as_uri(), str(shallow)], check=True)
        self.repo = shallow
        result = self.run_scan()
        self.assertEqual(result["status"], "incomplete")
        self.assertIn("complete Git history", result["error"])
        self.assertEqual(result["totalCommits"], 0)

    def test_timeout_cannot_acknowledge_a_batch(self):
        with patch.dict(os.environ, SCANNER_FAULT="hang"):
            result = history.scan(self.repo, self.head, self.output, self.config, str(self.scanner), 1, 1)
        self.assertEqual(result["status"], "incomplete")
        self.assertEqual(result["completedCommits"], 0)

    def test_invalid_cli_and_failed_evidence_write_have_one_error_envelope(self):
        for args, code in (([], 2), (["--output-dir", str(self.repo)], 2),
                           (["--output-dir", str(self.output), "--jobs=33"], 2),
                           (["--output-dir", str(self.repo / "file" / "evidence")], 5)):
            with self.subTest(args=args):
                result = subprocess.run([sys.executable, history.__file__, *args],
                                        text=True, capture_output=True)
                self.assertEqual(result.returncode, code)
                envelope = json.loads(result.stdout)
                self.assertFalse(envelope["ok"])
                self.assertEqual(envelope["error"]["code"], code)

    @unittest.skipUnless(os.getenv("MEMQL_GITLEAKS_TEST_BIN"), "requires pinned real Gitleaks")
    def test_real_scanner_finds_secret_introduced_only_at_merge(self):
        self.git("merge", "--no-ff", "--no-commit", "-s", "ours", "tag-only")
        (self.repo / "file").write_text("SYNTHETIC_CREDENTIAL_" + "Z" * 16 + "\n")
        self.git("commit", "-qam", "synthetic merge control")
        introduced = self.git("rev-parse", "HEAD")
        self.assertEqual(len(self.git("rev-list", "--parents", "-n", "1", introduced).split()), 3)
        (self.repo / "file").write_text("removed\n")
        self.git("commit", "-qam", "remove merge control")
        self.head = self.git("rev-parse", "HEAD")
        self.config.write_text("[[rules]]\nid = 'synthetic-control'\nregex = '''SYNTHETIC_CREDENTIAL_[A-Z]{16}'''\n")
        self.scanner = Path(os.environ["MEMQL_GITLEAKS_TEST_BIN"])
        result = self.run_scan()
        self.assertEqual(result["status"], "findings")
        self.assertEqual(result["completedCommits"], 5)
        findings = json.loads((self.output / "findings.json").read_text())
        self.assertTrue(any(item["Commit"] == introduced for item in findings))
        self.assertNotIn("Z" * 16, (self.output / "findings.json").read_text())
        serial = sorted(item["Fingerprint"] for item in findings)
        self.output = self.root / "parallel-evidence"
        self.output.mkdir()
        parallel = self.run_scan(jobs=2)
        self.assertEqual(parallel["status"], "findings")
        self.assertEqual(parallel["completedCommits"], 5)
        self.assertEqual(sorted(item["Fingerprint"] for item in json.loads(
            (self.output / "findings.json").read_text())), serial)

    @unittest.skipUnless(os.getenv("MEMQL_GITLEAKS_TEST_BIN"), "requires pinned real Gitleaks")
    def test_real_scanner_finds_removed_secret_on_tag_only_branch(self):
        self.git("checkout", "-q", "tag-only")
        (self.repo / "file").write_text("SYNTHETIC_CREDENTIAL_" + "Z" * 16 + "\n")
        self.git("commit", "-qam", "synthetic scanner control")
        introduced = self.git("rev-parse", "HEAD")
        (self.repo / "file").write_text("removed\n")
        self.git("commit", "-qam", "remove synthetic control")
        self.git("tag", "vremoved")
        self.git("checkout", "-q", "main")
        self.config.write_text("[[rules]]\nid = 'synthetic-control'\nregex = '''SYNTHETIC_CREDENTIAL_[A-Z]{16}'''\n")
        self.scanner = Path(os.environ["MEMQL_GITLEAKS_TEST_BIN"])
        result = self.run_scan()
        self.assertEqual(result["status"], "findings")
        self.assertEqual(result["completedCommits"], 5)
        findings = json.loads((self.output / "findings.json").read_text())
        self.assertTrue(any(item["Commit"] == introduced for item in findings))
        self.assertNotIn("Z" * 16, (self.output / "findings.json").read_text())


if __name__ == "__main__":
    unittest.main()
