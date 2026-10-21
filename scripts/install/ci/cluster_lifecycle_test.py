"""Exercise installer failure evidence without a Docker daemon or cluster."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class LifecycleDiagnosticsTests(unittest.TestCase):
    def run_diagnostics(self, logs, log_status=0, failed_install=False):
        source = Path(__file__).with_name('cluster-lifecycle.sh').read_text()
        diagnostics = source.split('# Cluster diagnostics\n', 1)[1].split(
            '# Tear the cluster down whatever happened\n', 1)[0]
        finish = source.split('finish() {\n', 1)[1].split('trap finish EXIT\n', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / '.memql/bin').mkdir(parents=True)
            (root / 'logs').write_text('\n'.join(logs) + '\n')
            kubectl = root / '.memql/bin/kubectl'
            kubectl.write_text('''#!/usr/bin/env bash
if [[ " $* " == *" logs "* ]]; then
  cat "$HOME/logs"
  exit "$LOG_STATUS"
fi
''')
            k3d = root / '.memql/bin/k3d'
            k3d.write_text('#!/usr/bin/env bash\nexit 0\n')
            kubectl.chmod(0o755)
            k3d.chmod(0o755)
            # The extracted production function still calls its bounded wrapper.
            # Replace only the external commands; no test can reach real kubectl.
            stubs = r'''
timeout() {
  printf '%s\n' "$*" >> "$HOME/calls"
  shift 3
  "$@"
}
sudo() { shift 2; "$@"; }
'''
            program = stubs + diagnostics
            if failed_install:
                program += r'''
handoff() { :; }
teardown() { printf 'finished\n' > "$HOME/teardown"; }
'''
                program += 'finish() {\n' + finish + '\ntrap finish EXIT\nexit 17\n'
            else:
                program += '\ndiagnostics\n'
            env = dict(os.environ, HOME=directory, MEMQL_INSTALL_EVIDENCE_DIR=directory,
                       LOG_STATUS=str(log_status), LEG='repair', REVISION='a' * 40)
            result = subprocess.run(['bash', '-c', program], env=env, text=True,
                                    capture_output=True, timeout=10)
            evidence = (root / 'diagnostics.txt').read_text()
            calls = (root / 'calls').read_text().splitlines()
            return result, evidence, calls, (root / 'teardown').exists()

    def test_failed_migration_is_retained_amid_unrelated_and_malformed_logs(self):
        failure = {'time': '2026-10-21T14:00:00Z', 'level': 'error',
                   'component': 'memoryNodesDB', 'msg': 'migration failed',
                   'error': 'cannot convert compressed hypertable', 'internal': 'omit'}
        logs = ['not JSON', 'null', '42', '"a string"', '[]',
                json.dumps({'component': 'other', 'msg': 'migration unrelated'}),
                json.dumps({'component': 'memoryNodesDB', 'msg': 'ready'}),
                json.dumps(failure)]
        result, evidence, calls, _ = self.run_diagnostics(logs)
        self.assertEqual(result.returncode, 0, result.stderr)
        retained = [json.loads(line) for line in evidence.splitlines() if line.startswith('{')]
        self.assertEqual(retained, [{key: failure[key] for key in ('time', 'level', 'msg', 'error')}])
        requests = [call for call in calls if ' logs ' in call]
        self.assertEqual(len(requests), 1)
        self.assertIn('--kill-after=5s 15s', requests[0])
        self.assertIn('--request-timeout=10s logs -n memql deploy/identity --tail=5000', requests[0])

    def test_unavailable_logs_preserve_original_failure_and_allow_teardown(self):
        result, evidence, calls, teardown = self.run_diagnostics([], log_status=124,
                                                               failed_install=True)
        self.assertEqual(result.returncode, 17, result.stderr)
        self.assertTrue(any(' logs ' in call for call in calls))
        self.assertIn('=== the memql-pipelines namespace ===', evidence)
        self.assertTrue(teardown)


if __name__ == '__main__':
    unittest.main()
