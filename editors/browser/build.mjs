import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const args = process.argv.slice(2);
// `npm ci` is the only network operation here, and npm does not retry a
// registry read that times out partway through a tarball. The local installer
// builds this inside the edge image, so one dropped read failed a whole cluster
// install. `npm ci` starts from a clean node_modules, so repeating it is safe;
// a build script failing is a code fault and is never repeated.
const ciAttempts = Math.max(1, Number(process.env.MEMQL_NPM_CI_ATTEMPTS ?? 3) || 1);
const retryDelaySeconds = Math.max(0, Number(process.env.MEMQL_NPM_RETRY_DELAY_SECONDS ?? 10) || 0);
function npm(directory, command) {
  const attempts = command[0] === 'ci' ? ciAttempts : 1;
  for (let attempt = 1; ; attempt++) {
    const result = spawnSync('npm', command, { cwd: path.join(root, directory), stdio: ['ignore', 2, 2] });
    if (!result.error && result.status === 0) {
      if (attempt > 1) console.error(`${directory}: npm ${command.join(' ')} succeeded on attempt ${attempt} of ${attempts}.`);
      return;
    }
    const failure = `${directory}: npm ${command.join(' ')} failed (${result.status})`;
    if (attempt >= attempts) throw new Error(attempts > 1 ? `${failure} after ${attempts} attempts` : failure);
    const delay = retryDelaySeconds * attempt;
    console.error(`${failure}; retrying in ${delay}s (attempt ${attempt + 1} of ${attempts}).`);
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, delay * 1000);
  }
}
if (args.length === 1 && args[0] === '--help') {
  console.log('Usage: node editors/browser/build.mjs [--skip-deps]\nBuild both browser extensions and the hosted editor. No deployment.');
} else if (args.some(arg => arg !== '--skip-deps')) {
  console.log(JSON.stringify({ ok: false, error: 'Unknown parameter' }));
  process.exitCode = 2;
} else {
  try {
    for (const [directory, script] of [['sdk/ts', 'build'], ['sdk/ts-viewkit', 'build'], ['editors/vscode', 'compile'], ['editors/productivity', 'compile'], ['editors/browser', 'build']]) {
      for (const command of [...(args.includes('--skip-deps') ? [] : [['ci', '--no-audit', '--no-fund']]), ['run', script]]) npm(directory, command);
    }
    console.log(JSON.stringify({ ok: true, output: path.join(root, 'editors/browser/dist') }));
  } catch (error) {
    console.error(error.message);
    console.log(JSON.stringify({ ok: false, error: error.message }));
    process.exitCode = 5;
  }
}
