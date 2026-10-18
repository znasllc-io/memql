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
        self.dsn = "postgres://memql:test@localhost:5432/memql?sslmode=disable"
        self.environment = patch.dict(runner.os.environ, MEMQL_DATABASE_DSN=self.dsn, MEMQL_REQUIRE_DB="1")
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def test_real_table_preserves_every_selected_package_and_budget(self):
        packages = ["component/memql", "component/packages", "integrations/work", "integrations/library", "component/campaigns", "component/pipelinerun", "component/memql/sense"]
        selected = [runner.MODULE + p for p in packages]
        commands = runner.commands(selected, self.classes)
        self.assertCountEqual([p for _, command in commands for p in command[5:]], selected)
        by_class = dict(commands)
        for name, timeout in [("memql", "600s"), ("packages", "300s"), ("work", "300s"), ("library", "180s"), ("campaigns", "600s"), ("default", "180s")]:
            self.assertEqual(by_class[name][:5], ["go", "test", "-count=1", "-p=1", "-timeout=" + timeout])

    def test_test_failure_stops_without_retry_or_false_success(self):
        with patch.object(runner, "sql") as sql, patch.object(runner.subprocess, "run", return_value=subprocess.CompletedProcess([], 7)) as execute:
            self.assertEqual(runner.run(["./component/memql", "./component/pipelinerun"], self.classes), 7)
            self.assertEqual(execute.call_count, 1)
            self.assertTrue(sql.call_args_list[-1].args[1].startswith("DROP DATABASE memql_pipeline_"))

    def test_classes_receive_distinct_databases_without_changing_selected_tests(self):
        with patch.object(runner, "sql") as sql, patch.object(runner.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)) as execute:
            selected = ["./component/memql", "./integrations/library", "./component/pipelinerun"]
            self.assertEqual(runner.run(selected, self.classes), 0)
            calls = execute.call_args_list
            self.assertEqual([call.args[0] for call in calls], [command for _, command in runner.commands(selected, self.classes)])
            databases = [call.kwargs["env"]["MEMQL_DATABASE_DSN"] for call in calls]
            self.assertEqual(len(set(databases)), 3)
            self.assertNotIn(self.dsn, databases)
            for call, dsn in zip(calls, databases):
                self.assertEqual(call.kwargs["env"]["MEMQL_REQUIRE_DB"], "1")
                self.assertTrue(dsn.startswith("postgres://memql:test@localhost:5432/memql_pipeline_"))
                self.assertTrue(dsn.endswith("?sslmode=disable"))
            creates = [call.args[1].removeprefix("CREATE DATABASE ") for call in sql.call_args_list if call.args[1].startswith("CREATE DATABASE ")]
            drops = [call.args[1].removeprefix("DROP DATABASE ").removesuffix(" WITH (FORCE)") for call in sql.call_args_list if call.args[1].startswith("DROP DATABASE ")]
            self.assertEqual(creates, drops)

    def test_failed_database_creation_never_drops_an_existing_database(self):
        with patch.object(runner, "sql", side_effect=OSError("create failed")) as sql, self.assertRaises(OSError):
            runner.run(["./integrations/library"], self.classes)
        self.assertEqual(sql.call_count, 1)

    def test_failed_extension_setup_still_cleans_up_and_runs_no_tests(self):
        with patch.object(runner, "sql", side_effect=[None, OSError("extensions failed"), None]) as sql, patch.object(runner.subprocess, "run") as execute, self.assertRaises(OSError):
            runner.run(["./integrations/library"], self.classes)
        execute.assert_not_called()
        self.assertTrue(sql.call_args_list[-1].args[1].startswith("DROP DATABASE memql_pipeline_"))

    def test_cleanup_failure_cannot_report_success(self):
        with patch.object(runner, "sql", side_effect=[None, None, OSError("drop failed")]), patch.object(runner.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)), self.assertRaises(OSError):
            runner.run(["./integrations/library"], self.classes)

    def test_nonlocal_or_ambiguous_service_uri_is_refused_before_effects(self):
        for dsn in ["", "postgres://remote/memql", "postgres://localhost/", "postgres://localhost/memql?host=remote", "postgres://localhost/memql?hostaddr=10.0.0.1", "postgres://localhost/memql?service=remote", "postgres://localhost/memql?host=", "host=localhost dbname=memql"]:
            with self.subTest(dsn=dsn), patch.dict(runner.os.environ, MEMQL_DATABASE_DSN=dsn), patch.object(runner, "sql") as sql, self.assertRaises(ValueError):
                runner.run(["./integrations/library"], self.classes)
            sql.assert_not_called()

    def test_libpq_environment_cannot_redirect_the_loopback_uri(self):
        with patch.dict(runner.os.environ, PGHOSTADDR="10.0.0.1", PGSERVICE="remote", PGPORT="9999"), patch.object(runner.subprocess, "run") as execute:
            runner.sql(self.dsn, "SELECT 1")
        call = execute.call_args
        self.assertNotIn(self.dsn, call.args[0])
        self.assertTrue(call.kwargs["check"])
        self.assertEqual(call.kwargs["timeout"], 45)
        env = call.kwargs["env"]
        self.assertEqual(env["PGDATABASE"], "memql")
        self.assertEqual(env["PGHOST"], "localhost")
        self.assertEqual(env["PGPORT"], "5432")
        self.assertEqual(env["PGUSER"], "memql")
        self.assertEqual(env["PGPASSWORD"], "test")
        self.assertEqual(env["PGOPTIONS"], "-c statement_timeout=30000")
        for key in ("PGHOSTADDR", "PGSERVICE", "PGSERVICEFILE"):
            self.assertNotIn(key, env)

    def test_refuses_empty_duplicate_or_expanding_selection(self):
        for packages in [[], ["./a", "./a"], ["./..."], ["-run=TestOne"], ["./a/../b"], ["foreign/module"]]:
            with self.subTest(packages=packages), self.assertRaises(ValueError):
                runner.commands(packages, self.classes)

    def test_db_cannot_silently_skip(self):
        with patch.dict(runner.os.environ, {}, clear=True), self.assertRaises(ValueError):
            runner.main()


if __name__ == "__main__":
    unittest.main()
