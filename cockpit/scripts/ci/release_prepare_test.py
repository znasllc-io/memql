"""Exercise release preparation against a local bare remote and a fake GitHub CLI."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class ReleasePreparationTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.remote = self.root / "remote.git"
        self.repo.mkdir()
        self.source = self.repo / "cockpit"
        self.source.mkdir()
        self.env = os.environ.copy()
        for key in list(self.env):
            if key.startswith("GIT_"):
                self.env.pop(key)
        self.env.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                        GIT_TERMINAL_PROMPT="0", GIT_ALLOW_PROTOCOL="file")
        self.run_git("init", "-q", "--initial-branch=main")
        self.run_git("config", "user.name", "Fixture")
        self.run_git("config", "user.email", "fixture@example.invalid")
        self.run_git("config", "core.hooksPath", os.devnull)
        subprocess.run(["git", "init", "--bare", "-q", str(self.remote)], check=True, env=self.env)
        for name, text in {
            "VERSION": "1.2.3\n",
            "cmd/memql/main.go": 'package main\nvar version = "1.2.3"\n',
            "internal/worker/connect.go": 'package worker\nvar cockpitVersionValue = "1.2.3"\n',
        }.items():
            file = self.source / name
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_text(text)
        self.script = self.source / "scripts/release/cut-release.sh"
        self.script.parent.mkdir(parents=True)
        shutil.copy(Path(__file__).resolve().parents[1] / "release/cut-release.sh", self.script)
        self.run_git("add", "cockpit/VERSION", "cockpit/cmd/memql/main.go", "cockpit/internal/worker/connect.go", "cockpit/scripts/release/cut-release.sh")
        self.run_git("commit", "-qm", "initial")
        self.run_git("tag", "cockpit/v1.2.3")
        self.run_git("remote", "add", "origin", str(self.remote))
        self.run_git("push", "-q", "origin", "main", "refs/tags/cockpit/v1.2.3")
        self.initial = self.run_git("rev-parse", "HEAD")
        tools = self.root / "tools"
        tools.mkdir()
        fake = tools / "gh"
        fake.write_text("""#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args=sys.argv[1:]
if args[:2]==['repo','view']:
    print('fixture/cockpit')
elif args[:2]==['pr','create']:
    body=Path(args[args.index('--body-file')+1]).read_text()
    Path(os.environ['FIXTURE_GH_RECEIPT']).write_text(json.dumps({'args':args,'body':body}))
    print('https://github.invalid/fixture/cockpit/pull/1')
else:
    raise SystemExit('unexpected GitHub mutation: '+repr(args))
""")
        fake.chmod(0o755)
        self.receipt = self.root / "gh.json"
        self.env.update(PATH=str(tools)+os.pathsep+self.env['PATH'], FIXTURE_GH_RECEIPT=str(self.receipt))

    def run_git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, env=self.env,
                                       stderr=subprocess.DEVNULL, text=True).strip()

    def invoke(self, *args):
        return subprocess.run(["bash", str(self.script), *args], cwd=self.repo,
                              env=self.env, capture_output=True, text=True)

    def assert_no_release_effect(self):
        self.assertEqual(self.run_git("rev-parse", "HEAD"), self.initial)
        self.assertEqual(self.run_git("branch", "--show-current"), "main")
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/heads/main").split()[0], self.initial)
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/heads/codex/release-cockpit-v1.2.4", "refs/tags/cockpit/v1.2.4"), "")
        self.assertFalse(self.receipt.exists())

    def test_prepares_only_three_version_files_on_a_pr_branch(self):
        result = self.invoke("--version=1.2.4", "--prepare", "--yes")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.run_git("branch", "--show-current"), "codex/release-cockpit-v1.2.4")
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/heads/main").split()[0], self.initial)
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/tags/cockpit/v1.2.4"), "")
        self.assertEqual(self.run_git("diff", "--name-only", self.initial, "HEAD").splitlines(),
                         ["cockpit/VERSION", "cockpit/cmd/memql/main.go", "cockpit/internal/worker/connect.go"])
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/heads/codex/release-cockpit-v1.2.4").split()[0],
                         self.run_git("rev-parse", "HEAD"))
        for name in ["VERSION", "cmd/memql/main.go", "internal/worker/connect.go"]:
            self.assertIn("1.2.4", (self.source / name).read_text())
        receipt = json.loads(self.receipt.read_text())
        self.assertIn("--body-file", receipt['args'])
        self.assertIn("--base", receipt['args'])
        self.assertIn("MemQL", receipt['body'])
        self.assertFalse(Path(receipt['args'][receipt['args'].index('--body-file')+1]).exists())

    def test_recommendation_has_no_writes(self):
        self.assertEqual(self.invoke().returncode, 0)
        self.assert_no_release_effect()

    def test_invalid_version_and_retired_cut_are_refused_before_writes(self):
        for args in [("--version=1.2.4;false", "--prepare", "--yes"),
                     ("--version=01.2.4", "--prepare", "--yes"), ("--cut", "--yes")]:
            with self.subTest(args=args):
                self.assertNotEqual(self.invoke(*args).returncode, 0)
                self.assert_no_release_effect()

    def test_dirty_tree_is_preserved_and_refused(self):
        (self.source / "VERSION").write_text("my change\n")
        self.assertNotEqual(self.invoke("--version=1.2.4", "--prepare", "--yes").returncode, 0)
        self.assertEqual((self.source / "VERSION").read_text(), "my change\n")
        self.assert_no_release_effect()

    def test_main_ahead_of_remote_is_refused(self):
        self.run_git("commit", "--allow-empty", "-qm", "unreviewed")
        ahead = self.run_git("rev-parse", "HEAD")
        self.assertNotEqual(self.invoke("--version=1.2.4", "--prepare", "--yes").returncode, 0)
        self.assertEqual(self.run_git("rev-parse", "HEAD"), ahead)
        self.assertEqual(self.run_git("ls-remote", "origin", "refs/heads/main").split()[0], self.initial)
        self.assertFalse(self.receipt.exists())

    def test_unfetched_remote_tag_is_refused(self):
        self.run_git("push", "origin", "HEAD:refs/tags/cockpit/v1.2.4")
        result = self.invoke("--version=1.2.4", "--prepare", "--yes")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("remote tag", result.stderr)
        self.assertEqual(self.run_git("rev-parse", "HEAD"), self.initial)
        self.assertFalse(self.receipt.exists())

    def test_existing_release_branch_is_not_overwritten(self):
        self.run_git("push", "origin", "HEAD:refs/heads/codex/release-cockpit-v1.2.4")
        result = self.invoke("--version=1.2.4", "--prepare", "--yes")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("resume its PR", result.stderr)
        self.assertEqual(self.run_git("rev-parse", "HEAD"), self.initial)
        self.assertFalse(self.receipt.exists())

    def test_missing_stamp_anchor_leaves_tree_unchanged(self):
        (self.source / "cmd/memql/main.go").write_text('package main\nconst version = "1.2.3"\n')
        self.run_git("add", "cockpit/cmd/memql/main.go")
        self.run_git("commit", "-qm", "missing stamp target")
        self.run_git("push", "-q", "origin", "main")
        self.initial = self.run_git("rev-parse", "HEAD")
        self.assertNotEqual(self.invoke("--version=1.2.4", "--prepare", "--yes").returncode, 0)
        self.assert_no_release_effect()
        self.assertEqual(self.run_git("status", "--porcelain"), "")


if __name__ == "__main__":
    unittest.main()
