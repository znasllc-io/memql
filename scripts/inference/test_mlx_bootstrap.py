import importlib.util
from pathlib import Path
import unittest
from unittest.mock import Mock


spec = importlib.util.spec_from_file_location("bootstrap", Path(__file__).parent / "mlx-bootstrap/sitecustomize.py")
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)


class MemoryBootstrapTest(unittest.TestCase):
    def test_unconfigured_process_does_not_import_mlx(self):
        load = Mock()
        bootstrap.configure({}, load)
        load.assert_not_called()

    def test_bound_is_applied_before_clearing_existing_cache(self):
        core = Mock()
        bootstrap.configure({"MEMQL_MLX_CACHE_LIMIT_BYTES": str(1024**3)}, lambda: core)
        self.assertEqual(core.mock_calls, [unittest.mock.call.set_cache_limit(1024**3), unittest.mock.call.clear_cache()])

    def test_invalid_bounds_refuse_instead_of_silently_disabling_guard(self):
        for value in ["-1", "nan", "8589934592", ""]:
            with self.subTest(value=value), self.assertRaises(ValueError):
                bootstrap.configure({"MEMQL_MLX_CACHE_LIMIT_BYTES": value}, Mock())


if __name__ == "__main__":
    unittest.main()
