import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
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

    def test_repository_pin_is_exact(self):
        prepare.pin_from(Path(__file__).resolve().parents[2])

    def test_bad_parameters_emit_one_failure_envelope(self):
        result = subprocess.run([sys.executable, str(Path(__file__).with_name("prepare.py")), "--unknown"],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(json.loads(result.stdout)["error"]["code"], 2)


if __name__ == "__main__":
    unittest.main()
