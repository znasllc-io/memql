import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const args = process.argv.slice(2);
if (args.length === 1 && args[0] === '--help') {
  console.log('Usage: node editors/browser/build.mjs [--skip-deps]\nBuild both browser extensions and the hosted editor. No deployment.');
} else if (args.some(arg => arg !== '--skip-deps')) {
  console.log(JSON.stringify({ ok: false, error: 'Unknown parameter' }));
  process.exitCode = 2;
} else {
  try {
    for (const [directory, script] of [['sdk/ts', 'build'], ['sdk/ts-viewkit', 'build'], ['editors/vscode', 'compile'], ['editors/productivity', 'compile'], ['editors/browser', 'build']]) {
      for (const command of [...(args.includes('--skip-deps') ? [] : [['ci', '--no-audit', '--no-fund']]), ['run', script]]) {
        const result = spawnSync('npm', command, { cwd: path.join(root, directory), stdio: ['ignore', 2, 2] });
        if (result.error || result.status !== 0) throw new Error(`${directory}: npm ${command.join(' ')} failed (${result.status})`);
      }
    }
    console.log(JSON.stringify({ ok: true, output: path.join(root, 'editors/browser/dist') }));
  } catch (error) {
    console.error(error.message);
    console.log(JSON.stringify({ ok: false, error: error.message }));
    process.exitCode = 5;
  }
}
