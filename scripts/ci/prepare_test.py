import importlib.util
import json
import os
from pathlib import Path
import shutil
import shlex
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import sys

spec = importlib.util.spec_from_file_location("prepare", Path(__file__).with_name("prepare.py"))
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class PrepareTest(unittest.TestCase):
    def repository(self, path, contents):
        path.mkdir()
        subprocess.run(["git", "init", "-q", str(path)], check=True)
        for name, value in contents.items():
            target = path / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(value)
        subprocess.run(["git", "add", *contents], cwd=path, check=True)
        subprocess.run(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                        "-c", "core.hooksPath=/dev/null", "commit", "-qm", "fixture"], cwd=path, check=True)
        return subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=path, text=True).strip()

    def test_exact_owned_siblings_and_no_stale_reuse(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pin = self.repository(root / "engine", {"marker": "pinned engine"})
            sha = self.repository(root / "cockpit", {".github/memql-pin": pin + "\n", "marker": "candidate"})
            # An operator's unstaged source/pin edits must not enter a run of HEAD.
            (root / "cockpit/.github/memql-pin").write_text("f" * 40 + "\n")
            (root / "cockpit/marker").write_text("uncommitted")
            result = prepare.prepare(root / "cockpit", root / "engine")
            self.assertEqual(result["engine"], pin)
            self.assertEqual(result["candidate"], sha)
            owned = root / "cockpit/.memql-ci"
            self.assertEqual((owned / "memql/marker").read_text(), "pinned engine")
            self.assertEqual((owned / "memql-cockpit/marker").read_text(), "candidate")
            with self.assertRaises(FileExistsError):
                prepare.prepare(root / "cockpit", root / "engine")
            self.assertFalse((root / "memql").exists())

    def test_rejects_ambiguous_pin(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".github").mkdir()
            for value in ["main", "a" * 40 + "\n" + "b" * 40, "", "-bad"]:
                (root / ".github/memql-pin").write_text(value)
                with self.assertRaises(ValueError):
                    prepare.pin_from(root)

    def test_local_fetch_from_another_owners_checkout(self):
        # Exercise Git's actual ownership gate even on a single-user runner.
        # Its test switch applies to upload-pack as well as the parent fetch.
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pin = self.repository(root / "engine", {"marker": "engine"})
            self.repository(root / "cockpit", {".github/memql-pin": pin + "\n"})
            real_git = shutil.which("git")
            wrapper = root / "tools"
            wrapper.mkdir()
            executable = wrapper / "git"
            executable.write_text("#!/bin/sh\nGIT_TEST_ASSUME_DIFFERENT_OWNER=true exec " +
                                  shlex.quote(real_git) + ' "$@"\n')
            executable.chmod(0o755)
            with patch.dict(os.environ, {"PATH": str(wrapper) + os.pathsep + os.environ["PATH"]}):
                refused = subprocess.run(["git", "status", "--porcelain"], cwd=root / "engine",
                                         capture_output=True, text=True)
                self.assertNotEqual(refused.returncode, 0, "the ownership gate must be exercised")
                self.assertIn("dubious ownership", refused.stderr)
                result = prepare.prepare(root / "cockpit", root / "engine")
                self.assertEqual(result["engine"], pin)
                self.assertEqual((root / "cockpit/.memql-ci/memql/marker").read_text(), "engine")

    def test_repository_pin_is_exact(self):
        prepare.pin_from(Path(__file__).resolve().parents[2])

    def test_bad_parameters_emit_one_failure_envelope(self):
        result = subprocess.run([sys.executable, str(Path(__file__).with_name("prepare.py")), "--unknown"],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stdout)["error"]["code"], 2)


if __name__ == "__main__":
    unittest.main()
