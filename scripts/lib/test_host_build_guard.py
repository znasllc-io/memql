import argparse
import contextlib
import fcntl
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import host_build_guard as guard


class HostBuildGuardTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.patch = mock.patch.object(guard, "capacity_root", return_value=self.root)
        self.patch.start()
        self.addCleanup(self.patch.stop)
        self.env = mock.patch.dict(os.environ, {}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        os.environ.pop(guard.LOCK_FD_ENV, None)
        os.environ.pop(guard.TOKEN_ENV, None)

    def test_mutual_exclusion_preserves_shared_inode(self):
        with guard.host_build_guard():
            inode = (self.root / "pipeline.lock").stat().st_ino
            with self.assertRaises(guard.GuardError):
                with guard.host_build_guard():
                    self.fail("a competing build entered")
        with guard.host_build_guard():
            self.assertEqual(inode, (self.root / "pipeline.lock").stat().st_ino)

    def test_inherited_guard_does_not_unlock_parent(self):
        with guard.host_build_guard() as fd:
            marker = {"kind": "local-build-v1", "token": "fixture"}
            guard.write_marker(self.root, marker)
            with mock.patch.dict(os.environ, {guard.LOCK_FD_ENV: str(fd), guard.TOKEN_ENV: "fixture"}):
                with guard.host_build_guard():
                    pass
            contender = os.open(self.root / "pipeline.lock", os.O_RDWR)
            try:
                with self.assertRaises(BlockingIOError):
                    fcntl.flock(contender, fcntl.LOCK_EX | fcntl.LOCK_NB)
            finally:
                os.close(contender)
            self.assertEqual(marker, guard.read_marker(self.root))

    def test_dirty_pipeline_is_never_cleared(self):
        path = self.root / "pipeline.lock"
        original = b'{"execution":"native","workspace":"/retained"}'
        path.write_bytes(original)
        inode = path.stat().st_ino
        with self.assertRaises(guard.GuardError):
            with guard.host_build_guard():
                pass
        self.assertEqual(original, path.read_bytes())
        self.assertEqual(inode, path.stat().st_ino)

    def test_symlink_lock_is_refused(self):
        target = self.root / "unrelated"
        target.write_text("keep")
        (self.root / "pipeline.lock").symlink_to(target)
        with self.assertRaises(guard.GuardError):
            with guard.host_build_guard():
                pass
        self.assertEqual("keep", target.read_text())

    def test_interrupted_local_and_workbench_records_are_retained(self):
        for name in ("local-build.json", "workbench-0.lock", "inference-active.json"):
            with self.subTest(name=name):
                path = self.root / name
                path.write_text("unknown retained operation")
                with self.assertRaises(guard.GuardError):
                    with guard.host_build_guard():
                        pass
                self.assertEqual("unknown retained operation", path.read_text())
                path.unlink()

    def test_empty_or_symlinked_inference_witness_is_not_completion(self):
        path = self.root / "inference-active.json"
        for symlink in (False, True):
            with self.subTest(symlink=symlink):
                if symlink:
                    path.symlink_to(self.root / "missing-operation")
                else:
                    path.touch()
                with self.assertRaises(guard.GuardError):
                    with guard.host_build_guard():
                        self.fail("uncertain inference admitted a local build")
                self.assertTrue(path.is_symlink() if symlink else path.exists())
                path.unlink()

    def write_witness(self, record, *, age_seconds=0):
        path = self.root / "inference-active.json"
        path.write_text(record if isinstance(record, str) else json.dumps(record))
        if age_seconds:
            written = time.time() - age_seconds
            os.utime(path, (written, written))
        return path

    @staticmethod
    def rfc3339(seconds):
        # The Cockpit writes a fence's end as Go's RFC3339Nano, in UTC.
        return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(seconds)) + ".123456789Z"

    def test_inference_witness_fences_builds_until_the_time_it_names(self):
        path = self.write_witness({"version": 2, "attempt": "a" * 32, "until": self.rfc3339(time.time() + 600)})
        with self.assertRaises(guard.GuardError) as raised:
            with guard.host_build_guard():
                self.fail("a build was admitted beside a model call that may still be running")
        self.assertIn("may still be running", str(raised.exception))
        self.assertTrue(path.exists())

    def test_inference_witness_past_its_end_admits_a_build_and_is_kept(self):
        path = self.write_witness({"version": 2, "attempt": "a" * 32, "until": self.rfc3339(time.time() - 60)})
        with guard.host_build_guard():
            pass
        self.assertTrue(path.exists(), "the guard removes no record; the Cockpit's own readers do")

    def test_inference_witness_with_no_time_ends_the_bound_after_it_was_written(self):
        bound = guard.INFERENCE_FENCE_BOUND_SECONDS
        records = (
            ("an older Cockpit's", {"version": 1, "attempt": "a" * 32}),
            ("empty", ""),
            ("a partial write", '{"version": 2, "att'),
            ("an unreadable time", {"version": 2, "attempt": "a" * 32, "until": "soon"}),
        )
        for name, record in records:
            with self.subTest(record=name):
                path = self.write_witness(record, age_seconds=bound - 60)
                with self.assertRaises(guard.GuardError):
                    with guard.host_build_guard():
                        self.fail("a record that names no time admitted a build inside the bound")
                path = self.write_witness(record, age_seconds=bound + 60)
                with guard.host_build_guard():
                    pass
                path.unlink()

    def test_a_directory_in_place_of_the_inference_witness_never_ends(self):
        path = self.root / "inference-active.json"
        path.mkdir()
        old = time.time() - 2 * guard.INFERENCE_FENCE_BOUND_SECONDS
        os.utime(path, (old, old))
        with self.assertRaises(guard.GuardError):
            with guard.host_build_guard():
                self.fail("a record that is not a regular file admitted a build")
        self.assertTrue(path.is_dir())

    def test_inference_witness_time_is_read_as_go_writes_it(self):
        self.assertEqual(0, guard.rfc3339_seconds("1970-01-01T00:00:00Z"))
        self.assertEqual(-7200, guard.rfc3339_seconds("1970-01-01T00:00:00+02:00"))
        self.assertAlmostEqual(1.5, guard.rfc3339_seconds("1970-01-01T00:00:01.5Z"))
        self.assertAlmostEqual(1.123456789, guard.rfc3339_seconds("1970-01-01T00:00:01.123456789Z"))
        for text in ("soon", "1970-13-01T00:00:00Z", "1970-01-01 00:00:00Z", "", None, 5):
            with self.subTest(text=text):
                self.assertIsNone(guard.rfc3339_seconds(text))

    def test_non_object_or_unknown_marker_cannot_admit_cleanup(self):
        for content in ('null', '[]', '{}', '{"kind":"unknown","token":"test"}'):
            with self.subTest(content=content):
                path = self.root / guard.MARKER_NAME
                path.write_text(content)
                with self.assertRaises(guard.GuardError):
                    with guard.host_build_guard():
                        self.fail("invalid marker admitted maintenance")
                self.assertEqual(content, path.read_text())

    def test_recovery_requires_exact_record_confirmation_and_inactive_group(self):
        marker = {"kind": "local-build-v1", "token": "fixture", "pid": 12345, "processGroup": 12346}
        guard.write_marker(self.root, marker)
        args = argparse.Namespace(record=guard.marker_digest(marker), confirm="reconciled-local-build")
        with mock.patch.object(os, "kill", side_effect=ProcessLookupError), mock.patch.object(guard, "group_alive", return_value=False):
            for record, confirm in (("changed", args.confirm), (args.record, "")):
                with self.assertRaises(guard.GuardError):
                    guard.recover(argparse.Namespace(record=record, confirm=confirm))
                self.assertEqual(marker, guard.read_marker(self.root))
            with mock.patch.object(guard, "group_alive", return_value=True):
                with self.assertRaises(guard.GuardError):
                    guard.recover(args)
            with guard.host_build_guard(_completed_record=marker):
                with self.assertRaises(guard.GuardError):
                    guard.recover(args)
            result = guard.recover(args)
        self.assertTrue(result["ok"])
        self.assertFalse((self.root / guard.MARKER_NAME).exists())
        self.assertEqual(b"", (self.root / "pipeline.lock").read_bytes())

    def test_inherited_wrong_inode_or_wrong_token_is_refused(self):
        other = os.open(self.root / "other", os.O_CREAT | os.O_RDWR, 0o600)
        try:
            with self.assertRaises(guard.GuardError):
                with guard.host_build_guard(other):
                    pass
        finally:
            os.close(other)
        with guard.host_build_guard() as fd:
            guard.write_marker(self.root, {"kind": "local-build-v1", "token": "original"})
            with mock.patch.dict(os.environ, {guard.TOKEN_ENV: "wrong"}):
                with self.assertRaises(guard.GuardError):
                    with guard.host_build_guard(fd):
                        pass

    def test_disk_measurement_fails_closed(self):
        disk = argparse.Namespace(f_bavail=20480, f_frsize=1024 * 1024)
        with mock.patch.object(os, "statvfs", return_value=disk):
            self.assertEqual(20480 * 1024 * 1024, guard.disk_preflight(self.root))
            with self.assertRaises(guard.GuardError):
                guard.disk_preflight(self.root, 20481)
        with mock.patch.object(os, "statvfs", side_effect=OSError("unreadable")):
            with self.assertRaises(guard.GuardError):
                guard.disk_preflight(self.root)

    def run_supervisor(self, code):
        args = argparse.Namespace(workspace=str(self.root), min_free_disk_mib=1024,
                                  command=[sys.executable, "-c", code])
        output = io.StringIO()
        with mock.patch.object(guard, "disk_preflight", return_value=30 * 1024**3), contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
            status = guard.supervise(args)
        return status, json.loads(output.getvalue())

    def test_success_clears_only_local_marker(self):
        status, result = self.run_supervisor('print(\'{"ok":true,"capability":"k3d.dev","changed":true,"result":{}}\')')
        self.assertEqual(0, status)
        self.assertTrue(result["ok"])
        self.assertFalse((self.root / "local-build.json").exists())
        self.assertEqual(b"", (self.root / "pipeline.lock").read_bytes())

    def test_failed_or_unknown_child_retains_recovery_record(self):
        for code in ('print("unknown")', 'import sys; print(\'{"ok":false,"capability":"k3d.dev","result":{}}\'); sys.exit(5)'):
            with self.subTest(code=code):
                status, result = self.run_supervisor(code)
                self.assertEqual(5, status)
                self.assertFalse(result["ok"])
                self.assertIn("--inspect", result["error"]["message"])
                self.assertIn("processGroup", guard.read_marker(self.root))
                (self.root / "local-build.json").unlink()

    def test_detached_fd_holder_prevents_completion(self):
        child_pid = self.root / "detached.pid"
        code = f'''import os,time
pid = os.fork()
if pid == 0:
    os.setsid()
    os.close(1)
    open({str(child_pid)!r}, "w").write(str(os.getpid()))
    time.sleep(15)
    os._exit(0)
while not os.path.exists({str(child_pid)!r}): time.sleep(0.01)
print('{{"ok":true,"capability":"k3d.dev","changed":true,"result":{{}}}}')
'''
        try:
            status, result = self.run_supervisor(code)
            self.assertEqual(5, status)
            self.assertFalse(result["ok"])
            self.assertTrue((self.root / "local-build.json").exists())
        finally:
            if child_pid.exists():
                try:
                    os.kill(int(child_pid.read_text()), signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def test_signal_retains_marker_and_returns_one_failure(self):
        helper = str(Path(guard.__file__).parent)
        bootstrap = f'import sys; sys.path.insert(0,{helper!r}); import host_build_guard as g; from pathlib import Path; g.capacity_root=lambda:Path({str(self.root)!r}); g.disk_preflight=lambda *a:30*1024**3; sys.exit(g.main())'
        process = subprocess.Popen([sys.executable, "-c", bootstrap, "--", sys.executable, "-c", "import time; time.sleep(30)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                marker = guard.read_marker(self.root)
                if marker and marker.get("processGroup"):
                    break
                time.sleep(0.01)
            else:
                self.fail("child never started")
            process.send_signal(signal.SIGTERM)
            output, _ = process.communicate(timeout=7)
            self.assertEqual(5, process.returncode)
            self.assertFalse(json.loads(output)["ok"])
            self.assertTrue((self.root / "local-build.json").exists())
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()

    def test_cancellation_kills_term_ignoring_child_and_keeps_marker(self):
        helper = str(Path(guard.__file__).parent)
        ready = self.root / "ready"
        child_code = f'import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); open({str(ready)!r},"w").close(); time.sleep(30)'
        bootstrap = f'import sys; sys.path.insert(0,{helper!r}); import host_build_guard as g; from pathlib import Path; g.capacity_root=lambda:Path({str(self.root)!r}); g.disk_preflight=lambda *a:30*1024**3; g.CANCEL_GRACE_SECONDS=0.1; sys.exit(g.main())'
        process = subprocess.Popen([sys.executable, "-c", bootstrap, "--", sys.executable, "-c", child_code], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            deadline = time.monotonic() + 5
            while not ready.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(ready.exists())
            process.send_signal(signal.SIGTERM)
            output, _ = process.communicate(timeout=4)
            self.assertEqual(5, process.returncode)
            self.assertFalse(json.loads(output)["ok"])
            marker = guard.read_marker(self.root)
            self.assertFalse(guard.group_alive(marker["processGroup"]))
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate()


if __name__ == "__main__":
    unittest.main()
