import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("pipeline_editor_hosts", Path(__file__).with_name("pipeline-editor-hosts.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class EditorHosts(unittest.TestCase):
    def test_covers_the_supported_host_matrix(self):
        core = runner.lanes("core", "linux/amd64")
        self.assertCountEqual([lane[1] for lane in core], ["1.91.0", "stable"])
        observed = []
        for platform, os in (("linux/amd64", "ubuntu-latest"), ("darwin/arm64", "macos-latest")):
            for _, version, _, command in runner.lanes("productivity", platform):
                host = "desktop" if "--desktop" in command else "web"
                observed.append({"os": os, "host": host, "version": version})
                self.assertEqual(command[0] == "xvfb-run", host == "desktop" and platform.startswith("linux/"))
        self.assertCountEqual(observed, [
            {"os": os, "host": host, "version": version}
            for os in ("ubuntu-latest", "macos-latest")
            for host, version in (("desktop", "1.91.0"), ("desktop", "stable"), ("web", "stable"))
        ])

    def run_fixture(self, result):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            output = root / "results"
            output.mkdir()
            with patch.object(runner, "native_platform", return_value="linux/amd64"), \
                 patch.object(runner.subprocess, "check_output", side_effect=["a" * 40, ""]), \
                 patch.object(runner, "execute", side_effect=result) as execute:
                code = runner.run(root, output, "linux/amd64", "all")
            return code, json.loads((output / "summary.json").read_text()), execute.call_args_list

    def test_one_failed_host_keeps_other_results_and_refuses_green(self):
        code, result, calls = self.run_fixture(lambda command, cwd, env, log: 7 if log.name == "core-minimum.log" else 0)
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "failed")
        self.assertEqual(len(result["lanes"]), 5)
        self.assertEqual([lane["name"] for lane in result["lanes"] if lane["status"] == "failed"], ["core-minimum"])
        self.assertEqual(len(calls), 12)

    def test_preparation_failure_starts_no_hosts(self):
        code, result, calls = self.run_fixture(lambda *args: 9)
        self.assertEqual(code, 1)
        self.assertEqual(result["lanes"], [])
        self.assertEqual(len(calls), 1)
        self.assertIn("workspace exited 9", result["error"])

    def test_spawn_failure_retains_failed_summary(self):
        code, result, _ = self.run_fixture(OSError("executable unavailable"))
        self.assertEqual(code, 1)
        self.assertEqual(result["status"], "failed")
        self.assertIn("unavailable", result["error"])

    def test_success_requires_every_host_and_ignores_ambient_host_overrides(self):
        with patch.dict(runner.os.environ, {"MEMQL_VSCODE_EXECUTABLE": "/fake/code", "MEMQL_HOST_SMOKE_CLUSTERS_FILE": "/personal/clusters", "GOOS": "windows"}):
            code, result, calls = self.run_fixture(lambda *args: 0)
        self.assertEqual(code, 0)
        self.assertEqual(result["status"], "passed")
        self.assertEqual(len(result["lanes"]), 5)
        for call in calls:
            env = call.args[2]
            self.assertNotIn("MEMQL_VSCODE_EXECUTABLE", env)
            self.assertNotIn("MEMQL_HOST_SMOKE_CLUSTERS_FILE", env)
            self.assertEqual(env["GOOS"], "linux")
            self.assertEqual(env["GOARCH"], "amd64")

    def test_cannot_claim_mac_execution_from_a_linux_worker(self):
        with patch.object(runner, "native_platform", return_value="linux/amd64"), patch.object(runner, "execute") as execute:
            with self.assertRaisesRegex(ValueError, "cannot be cross-compiled"):
                runner.run(Path("."), Path("."), "darwin/arm64", "all")
            execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
