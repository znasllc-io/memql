#!/usr/bin/env python3
"""The fixture runner inherits one budget and reaps timed-out process trees."""

import contextlib
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from fixture_process import DEADLINE_ENV, FixtureProcesses, SUITE_PROCESSES, main


class FixtureProcessTest(unittest.TestCase):
    def test_inherited_deadline_reserves_cleanup_without_resetting_budget(self):
        with patch.dict(os.environ, {DEADLINE_ENV: "160000"}), \
                patch("fixture_process.time.time", return_value=100), \
                patch("fixture_process.time.monotonic", return_value=20):
            processes = FixtureProcesses.from_environment()
        self.assertEqual(processes.deadline, 75)  # 60 seconds less 5 for cleanup.
        with patch("fixture_process.time.monotonic", return_value=30):
            self.assertEqual(processes.remaining(), 45)
        with patch("fixture_process.time.monotonic", return_value=60):
            self.assertEqual(processes.remaining(), 15)

    def test_short_inherited_budget_keeps_a_cleanup_margin(self):
        with patch.dict(os.environ, {DEADLINE_ENV: "101000"}), \
                patch("fixture_process.time.time", return_value=100), \
                patch("fixture_process.time.monotonic", return_value=20):
            self.assertAlmostEqual(FixtureProcesses.from_environment().deadline, 20.5)

    def test_standalone_suite_is_bounded_and_expired_budget_cannot_spawn(self):
        with patch.dict(os.environ, {}, clear=True), \
                patch("fixture_process.time.monotonic", return_value=20):
            processes = FixtureProcesses.from_environment()
        self.assertEqual(processes.deadline, 615)
        self.assertEqual(processes.cleanup_deadline, 620)
        with patch("fixture_process.time.monotonic", return_value=620), \
                patch("fixture_process.subprocess.Popen") as spawn:
            with self.assertRaisesRegex(AssertionError, "deadline exhausted"):
                processes.run(["never-start-this"])
        spawn.assert_not_called()

    def test_only_absent_or_zombie_groups_can_ignore_signal_permission_error(self):
        for states, allowed in (("999 S\n", True), ("123 Z\n999 S\n", True),
                                ("123 Z\n123 S\n", False), ("invalid output", False)):
            with self.subTest(states=states):
                denied = PermissionError("signal denied")
                with patch("fixture_process.os.killpg", side_effect=denied), \
                        patch("fixture_process.subprocess.run",
                              return_value=subprocess.CompletedProcess([], 0, states, "")):
                    if allowed:
                        FixtureProcesses.kill_group(SimpleNamespace(pid=123))
                    else:
                        with self.assertRaises(PermissionError) as raised:
                            FixtureProcesses.kill_group(SimpleNamespace(pid=123))
                        self.assertIs(raised.exception, denied)

    def test_group_inspection_failure_preserves_signal_denial(self):
        denied = PermissionError("signal denied")
        for failure in (subprocess.TimeoutExpired("ps", 0), subprocess.CalledProcessError(1, "ps")):
            with self.subTest(failure=failure), \
                    patch("fixture_process.os.killpg", side_effect=denied), \
                    patch("fixture_process.subprocess.run", side_effect=failure):
                with self.assertRaises(PermissionError) as raised:
                    FixtureProcesses.kill_group(SimpleNamespace(pid=123))
                self.assertIs(raised.exception, denied)

    def test_cleanup_operations_spend_one_reserved_budget(self):
        processes = FixtureProcesses(105, cleanup_deadline=108)
        with patch("fixture_process.time.monotonic", return_value=104):
            cleanup = processes.cleanup_limit()
        self.assertEqual(cleanup, 108)
        with patch("fixture_process.time.monotonic", return_value=107):
            self.assertEqual(processes.cleanup_remaining(cleanup), 1)
        with patch("fixture_process.time.monotonic", return_value=109):
            self.assertEqual(processes.cleanup_remaining(cleanup), 0)

    def test_healthy_command_retains_available_budget_beyond_old_child_cap(self):
        processes = FixtureProcesses(120)
        with patch("fixture_process.time.monotonic", return_value=80), \
                patch("fixture_process.subprocess.Popen") as spawn:
            proc = spawn.return_value.__enter__.return_value
            proc.communicate.return_value = ("completed", "")
            proc.returncode = 0
            result = processes.run(["fixture"])
            proc.communicate.assert_called_once_with(input=None, timeout=40)
        self.assertEqual(result.stdout, "completed")
        self.assertEqual(result.returncode, 0)

    def test_timeout_kills_pipe_holding_descendant_and_preserves_output(self):
        for leader_running in (False, True):
            with self.subTest(leader_running=leader_running):
                self.assert_timeout_cleans_descendant(leader_running)

    def assert_timeout_cleans_descendant(self, leader_running):
        with tempfile.TemporaryDirectory(prefix="memql-fixture-process-") as directory:
            ready = Path(directory) / "ready"
            child = ("import os,sys,time; from pathlib import Path; "
                     "print('child stdout',flush=True); "
                     "print('child stderr',file=sys.stderr,flush=True); "
                     "p=Path(sys.argv[1]+'.tmp'); p.write_text(str(os.getpid())); "
                     "p.rename(sys.argv[1]); time.sleep(600)")
            # Cover both a blocked leader and one that already exited. Its
            # child keeps both capture pipes; killing only the leader would
            # leave communicate blocked forever in either case.
            command = [sys.executable, "-c",
                       "import subprocess,sys,time; subprocess.Popen([sys.executable,'-c',sys.argv[1],sys.argv[2]])"
                       + ("; time.sleep(600)" if leader_running else ""),
                       child, str(ready)]
            processes = FixtureProcesses(SUITE_PROCESSES.deadline)
            calls = 0
            started = []
            real_popen = subprocess.Popen

            def start(*args, **kwargs):
                proc = real_popen(*args, **kwargs)
                started.append(proc)
                return proc

            def remaining():
                nonlocal calls
                calls += 1
                if calls == 1:
                    return SUITE_PROCESSES.remaining()
                # Synchronize on a running descendant before injecting a short
                # deadline, so startup speed cannot make this regression flaky.
                while not ready.exists():
                    SUITE_PROCESSES.remaining()
                    time.sleep(.01)
                if not leader_running:
                    # Prove the exact orphan-pipe shape: the leader is already
                    # reaped before communicate reaches its short deadline.
                    while started[0].poll() is None:
                        SUITE_PROCESSES.remaining()
                        time.sleep(.01)
                return .01

            with patch.object(processes, "remaining", side_effect=remaining), \
                    patch("fixture_process.subprocess.Popen", side_effect=start):
                with self.assertRaises(AssertionError) as raised:
                    processes.run(command)
            self.assertIn("shared suite deadline", str(raised.exception))
            self.assertIn("child stdout", str(raised.exception))
            self.assertIn("child stderr", str(raised.exception))
            pid = ready.read_text()
            self.assertTrue(pid.isdigit(), pid)
            # Linux can briefly retain an adopted zombie; it is terminated and
            # no longer owns pipes. macOS normally reaps it before this query.
            # Signal delivery can precede the child's transition to zombie.
            # Allow that transition within the existing cleanup reserve.
            deadline = min(SUITE_PROCESSES.deadline, SUITE_PROCESSES.cleanup_limit())
            state = "running"
            while time.monotonic() < deadline:
                state = subprocess.run(["ps", "-p", pid, "-o", "stat="],
                                       capture_output=True, text=True,
                                       timeout=deadline - time.monotonic()).stdout.strip()
                if not state or state.startswith("Z"):
                    break
                time.sleep(min(.01, max(0, deadline - time.monotonic())))
            self.assertTrue(not state or state.startswith("Z"), state)

    def test_shell_entrypoint_preserves_output_and_failure_status(self):
        with tempfile.TemporaryDirectory(prefix="memql suite ") as directory:
            script = Path(directory) / "failure.sh"
            script.write_text("printf 'fixture stdout\\n'\nprintf 'fixture stderr\\n' >&2\nexit 7\n")
            stdout, stderr = io.StringIO(), io.StringIO()
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = main(["--program", "bash", "--script", str(script)])
            self.assertEqual(code, 7)
            self.assertEqual(stdout.getvalue(), "fixture stdout\n")
            self.assertEqual(stderr.getvalue(), "fixture stderr\n")

    def test_shell_entrypoint_reports_timeout_diagnostics_as_failure(self):
        stderr = io.StringIO()
        with patch.object(SUITE_PROCESSES, "run", side_effect=AssertionError(
                "shared suite deadline\nstdout:\nlast passing assertion\nstderr:\nfixture error")), \
                contextlib.redirect_stderr(stderr):
            code = main(["--program", "bash", "--script", "fixture.sh"])
        self.assertEqual(code, 1)
        self.assertIn("last passing assertion", stderr.getvalue())
        self.assertIn("fixture error", stderr.getvalue())


if __name__ == "__main__":
    unittest.main()
