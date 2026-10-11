import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
const build = fileURLToPath(new URL('../build.mjs', import.meta.url));
// A fake npm on PATH: `ci` fails until the shared counter reaches FAIL_CI, and
// `run` fails only when FAIL_RUN names the script. Every call is logged.
function fakeNpm(failCi, failRun = '') {
  const dir = mkdtempSync(path.join(tmpdir(), 'memql-npm-'));
  writeFileSync(path.join(dir, 'npm'), `#!/bin/sh
echo "$PWD $*" >> "${dir}/calls"
if [ "$1" = ci ]; then n=$(cat "${dir}/ci" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "${dir}/ci"; [ $n -gt ${failCi} ] || exit 146; fi
if [ "$1" = run ] && [ "$2" = "${failRun}" ]; then exit 1; fi
exit 0
`);
  chmodSync(path.join(dir, 'npm'), 0o755);
  return dir;
}
function run(dir) {
  const result = spawnSync(process.execPath, [build], { encoding: 'utf8', env: { ...process.env, PATH: `${dir}${path.delimiter}${process.env.PATH}`, MEMQL_NPM_RETRY_DELAY_SECONDS: '0' } });
  const calls = readFileSync(path.join(dir, 'calls'), 'utf8').trim().split('\n');
  return { ...result, calls, envelope: JSON.parse(result.stdout.trim().split('\n').pop()) };
}
test('a dependency download that fails is retried and the build completes', () => {
  const { status, calls, envelope, stderr } = run(fakeNpm(2));
  assert.equal(status, 0);
  assert.equal(envelope.ok, true);
  assert.equal(calls.filter(call => call.endsWith(' ci --no-audit --no-fund')).length, 7);
  assert.match(stderr, /sdk\/ts: npm ci --no-audit --no-fund failed \(146\); retrying in 0s \(attempt 2 of 3\)/);
  assert.match(stderr, /succeeded on attempt 3 of 3/);
});
test('a download that keeps failing stops after three attempts with an error envelope', () => {
  const { status, calls, envelope } = run(fakeNpm(99));
  assert.equal(status, 5);
  assert.equal(envelope.ok, false);
  assert.equal(envelope.error, 'sdk/ts: npm ci --no-audit --no-fund failed (146) after 3 attempts');
  assert.equal(calls.length, 3);
});
test('a failing build script is a code fault and is not repeated', () => {
  const { status, calls, envelope } = run(fakeNpm(0, 'compile'));
  assert.equal(status, 5);
  assert.equal(envelope.error, 'editors/vscode: npm run compile failed (1)');
  assert.equal(calls.filter(call => call.endsWith(' run compile')).length, 1);
});
