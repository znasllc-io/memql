import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("native-env.sh")


class NativeEnvironmentTest(unittest.TestCase):
    def test_service_path_finds_existing_go_and_keeps_caches_in_attempt(self):
        with tempfile.TemporaryDirectory(prefix="native environment ") as directory:
            root = Path(directory).resolve()
            owner = root / "owner home"
            tools = owner / ".local/go/bin"
            tools.mkdir(parents=True)
            go = tools / "go"
            go.write_text('#!/bin/sh\nprintf "%s\\n" existing-local-go\n')
            go.chmod(0o755)
            attempt = root / "attempt checkout"
            attempt.mkdir()
            result = subprocess.run(
                ["/bin/sh", "-ec", '. "$1"; go; printf "%s\\n" "$GOPATH" "$GOCACHE"',
                 "native-env-test", str(SCRIPT)],
                cwd=attempt, env={**os.environ, "HOME": str(owner), "PATH": "/usr/bin:/bin"},
                capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout.splitlines(), [
                "existing-local-go", str(attempt / ".memql-native/go"),
                str(attempt / ".memql-native/go-build")])
            self.assertFalse((owner / ".cache").exists())
            self.assertTrue((attempt / ".memql-native/go-build").is_dir())

    def test_explicit_service_tool_precedes_discovered_installations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            tool = root / "go"
            tool.write_text('#!/bin/sh\nprintf "%s\\n" configured-go\n')
            tool.chmod(0o755)
            result = subprocess.run(
                ["/bin/sh", "-ec", '. "$1"; go', "native-env-test", str(SCRIPT)],
                cwd=root, env={**os.environ, "PATH": f"{root}:/usr/bin:/bin"},
                capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout.strip(), "configured-go")


if __name__ == "__main__":
    unittest.main()
