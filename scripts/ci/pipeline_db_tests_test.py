import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import yaml

spec = importlib.util.spec_from_file_location("pipeline_db_tests", Path(__file__).with_name("pipeline-db-tests.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class DatabaseBudgets(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).resolve().parents[2]
        self.classes = runner.read_classes(yaml.safe_load((root / ".github/workflows/ci.yml").read_text()))

    def test_real_table_preserves_every_selected_package_and_budget(self):
        packages = ["component/memql", "component/packages", "integrations/work", "integrations/library", "component/campaigns", "component/pipelinerun", "component/memql/sense"]
        selected = [runner.MODULE + p for p in packages]
        commands = runner.commands(selected, self.classes)
        self.assertCountEqual([p for _, command in commands for p in command[5:]], selected)
        by_class = dict(commands)
        for name, timeout in [("memql", "600s"), ("packages", "300s"), ("work", "180s"), ("library", "180s"), ("campaigns", "600s"), ("default", "180s")]:
            self.assertEqual(by_class[name][:5], ["go", "test", "-count=1", "-p=1", "-timeout=" + timeout])

    def test_test_failure_stops_without_retry_or_false_success(self):
        with patch.object(runner.subprocess, "run", return_value=subprocess.CompletedProcess([], 7)) as execute:
            self.assertEqual(runner.run(["./component/memql", "./component/pipelinerun"], self.classes), 7)
            self.assertEqual(execute.call_count, 1)

    def test_refuses_empty_duplicate_or_expanding_selection(self):
        for packages in [[], ["./a", "./a"], ["./..."], ["-run=TestOne"], ["./a/../b"], ["foreign/module"]]:
            with self.subTest(packages=packages), self.assertRaises(ValueError):
                runner.commands(packages, self.classes)

    def test_db_cannot_silently_skip(self):
        with patch.dict(runner.os.environ, {}, clear=True), self.assertRaises(ValueError):
            runner.main()


if __name__ == "__main__":
    unittest.main()
