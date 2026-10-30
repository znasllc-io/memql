import contextlib
import importlib.util
import io
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch
from unittest.mock import Mock


SPEC = importlib.util.spec_from_file_location("native_go_platform", Path(__file__).with_name("native_go_platform.py"))
PLATFORM = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLATFORM)


class NativeGoPlatformTest(unittest.TestCase):
    def assert_stopped(self, pid):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            try:
                os.kill(pid, 0)
            except ProcessLookupError:
                return
            # A minimal Linux container may not reap an orphan immediately.
            # A zombie cannot continue the download; no external ps is needed.
            stat = Path(f"/proc/{pid}/stat")
            try:
                if stat.read_text().rsplit(")", 1)[1].split()[0] == "Z":
                    return
            except (FileNotFoundError, ProcessLookupError):
                pass
            time.sleep(0.02)
        self.fail("toolchain descendant is still running")

    def test_a_process_exiting_during_the_stat_read_counts_as_stopped(self):
        # Linux answers ESRCH when a process exits between opening and reading its
        # stat file (measured: about 1 run in 100 on Python 3.12 in a container).
        with patch.object(os, "kill", side_effect=[None, ProcessLookupError]), \
                patch.object(Path, "read_text", side_effect=ProcessLookupError):
            self.assert_stopped(12345)

    def prepare(self, outputs):
        with patch.object(PLATFORM, "read_platform", side_effect=outputs) as read, \
                patch.object(PLATFORM.time, "sleep") as sleep, \
                contextlib.redirect_stderr(io.StringIO()) as log:
            ok = PLATFORM.prepare_platform("darwin", "arm64")
        return ok, read.call_count, sleep.call_count, log.getvalue()

    def test_preparation_recovers_but_reports_the_failed_attempt(self):
        ok, calls, waits, log = self.prepare([None, b"darwin\narm64\n"])
        self.assertTrue(ok)
        self.assertEqual((calls, waits), (2, 1))
        self.assertIn("failed on attempt 1", log)
        self.assertIn("succeeded on attempt 2", log)

    def test_permanent_preparation_failure_is_bounded(self):
        ok, calls, waits, log = self.prepare([None] * 3)
        self.assertFalse(ok)
        self.assertEqual((calls, waits), (3, 2))
        self.assertIn("failed after 3 attempts", log)

    def test_wrong_or_malformed_success_is_not_retried(self):
        for output in [b"linux\narm64\n", b"darwin\namd64\n", b"darwin\n", b"", b"darwin\narm64\nextra\n"]:
            with self.subTest(output=output):
                ok, calls, waits, _ = self.prepare([output])
                self.assertFalse(ok)
                self.assertEqual((calls, waits), (1, 0))

    def test_failed_command_discards_even_correct_platform_output(self):
        with tempfile.TemporaryDirectory() as directory:
            go = Path(directory) / "go"
            go.write_text('#!/bin/sh\nprintf "darwin\\narm64\\n"\nexit 1\n')
            go.chmod(0o700)
            with patch.dict(os.environ, {"PATH": directory}):
                self.assertIsNone(PLATFORM.read_platform(time.monotonic() + 30))

    def test_attempts_share_one_preparation_budget(self):
        clock = [10.0]
        processes = []
        for duration, output, status in [(8, b"", 1), (8, b"", 1), (3, b"darwin\narm64\n", 0)]:
            process = Mock(returncode=status)
            def complete(timeout, duration=duration, output=output):
                self.assertLess(duration, timeout)
                clock[0] += duration
                return output, None
            process.communicate.side_effect = complete
            processes.append(process)
        def wait(seconds):
            clock[0] += seconds
        with patch.object(PLATFORM, "PREPARATION_TIMEOUT_SECONDS", 30), \
                patch.object(PLATFORM.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(PLATFORM.time, "sleep", side_effect=wait), \
                patch.object(PLATFORM.subprocess, "Popen", side_effect=processes), \
                contextlib.redirect_stderr(io.StringIO()):
            self.assertTrue(PLATFORM.prepare_platform("darwin", "arm64"))
        self.assertEqual([p.communicate.call_args.kwargs["timeout"] for p in processes], [25, 15, 5])
        self.assertEqual(clock[0], 33)

    def test_backoff_and_cleanup_cannot_extend_the_preparation_budget(self):
        clock = [0.0]
        process = Mock(returncode=1)
        def fail(timeout):
            self.assertEqual(timeout, 5)
            clock[0] += 4
            return b"", None
        process.communicate.side_effect = fail
        with patch.object(PLATFORM, "PREPARATION_TIMEOUT_SECONDS", 10), \
                patch.object(PLATFORM.time, "monotonic", side_effect=lambda: clock[0]), \
                patch.object(PLATFORM.time, "sleep") as sleep, \
                patch.object(PLATFORM.subprocess, "Popen", return_value=process) as launch:
            with self.assertRaises(PLATFORM.PreparationTimeout):
                PLATFORM.prepare_platform("darwin", "arm64")
        self.assertEqual(launch.call_count, 1)
        sleep.assert_not_called()

    def run_worker_shutdown(self, final_command, cancel=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pid_file = root / "child.pid"
            go = root / "go"
            go.write_text('#!/bin/sh\n/bin/sleep 30 &\nprintf "%s" "$!" > "$PID_FILE"\n' + final_command + '\n')
            go.chmod(0o700)
            environment = dict(os.environ, PATH=directory, PID_FILE=str(pid_file))
            # Use the same group ownership as the native worker. Only the test
            # deadline is shortened; the actual helper and subprocess path run.
            invoke = "import importlib.util,sys; s=importlib.util.spec_from_file_location('platform',sys.argv.pop(1)); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); m.PREPARATION_TIMEOUT_SECONDS=1; m.CLEANUP_TIMEOUT_SECONDS=0.1; sys.exit(m.main())"
            shell = '"$@"\nstatus=$?\nif test "$status" = 0; then printf "TEST_RAN\\n"; fi\nexit "$status"\n'
            with subprocess.Popen(["/bin/sh", "-c", shell, "fixture-worker", sys.executable,
                                   "-c", invoke, str(Path(__file__).with_name("native_go_platform.py")),
                                   "--os=darwin", "--arch=arm64"], env=environment,
                                  stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                  start_new_session=True) as worker:
                try:
                    deadline = time.monotonic() + 3
                    while not pid_file.exists() and time.monotonic() < deadline:
                        time.sleep(0.02)
                    self.assertTrue(pid_file.exists(), "toolchain child did not start")
                    child_pid = int(pid_file.read_text())
                    self.assertEqual(os.getpgid(child_pid), worker.pid,
                                     "toolchain child escaped the native worker group")
                    if cancel:
                        os.killpg(worker.pid, signal.SIGTERM)
                    started = time.monotonic()
                    worker.wait(timeout=4)
                    self.assertLess(time.monotonic() - started, 3)
                finally:
                    # runGroup kills the original group as soon as its shell
                    # exits, even if the helper has not handled SIGTERM yet.
                    try:
                        os.killpg(worker.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    stdout, stderr = worker.communicate(timeout=3)
                self.assertNotIn(b"TEST_RAN", stdout)
                self.assertNotIn(b"retrying", stderr)
                if cancel:
                    self.assertNotEqual(worker.returncode, 0)
                else:
                    self.assertEqual(worker.returncode, 5)
                    self.assertIn(b"timed out", stderr)
            self.assert_stopped(int(pid_file.read_text()))

    def test_timeout_returns_to_worker_for_complete_group_cleanup(self):
        for final_command in ["wait", "exit 0"]:
            with self.subTest(launcher=final_command):
                self.run_worker_shutdown(final_command)

    def test_cancellation_keeps_toolchain_child_in_worker_group(self):
        self.run_worker_shutdown("wait", cancel=True)


if __name__ == "__main__":
    unittest.main()
