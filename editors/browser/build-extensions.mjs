import { cp, mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
const root = path.dirname(fileURLToPath(import.meta.url));
const output = path.join(root, 'public/extensions');
await rm(output, { recursive: true, force: true });
await mkdir(output, { recursive: true });
const extensions = [];
for (const name of ['vscode', 'productivity']) {
  const source = path.resolve(root, '..', name);
  const manifest = JSON.parse(await readFile(path.join(source, 'package.json'), 'utf8'));
  await readFile(path.join(source, manifest.browser));
  // The browser entry is the same bundle distributed in each VSIX.
  const folders = name === 'vscode' ? ['out', 'icons', 'syntaxes', 'themes', 'snippets'] : ['out', 'media', 'icons'];
  const files = ['package.json', 'README.md', 'language-configuration.json', ...folders];
  const entries = await readdir(source);
  for (const file of files.filter(file => entries.includes(file))) {
    await cp(path.join(source, file), path.join(output, name, file), { recursive: true,
      filter: filename => !filename.endsWith('.map') && !filename.endsWith('-meta.json') });
  }
  await cp(path.resolve(root, '../../LICENSE'), path.join(output, name, 'LICENSE'));
  async function walk(dir, prefix = '') {
    const result = [];
    for (const entry of await readdir(dir, { withFileTypes: true })) {
      const relative = prefix + entry.name;
      if (entry.isDirectory()) result.push(...await walk(path.join(dir, entry.name), relative + '/'));
      else result.push(relative);
    }
    return result;
  }
  extensions.push({ name, manifest, files: await walk(path.join(output, name)) });
}
await writeFile(path.join(root, 'src/bundled-extensions.json'), JSON.stringify(extensions));

// Preserve licenses alongside the shipped assets. Some API packages omit the
// upstream license file from npm, so their two canonical MIT licenses live in
// licenses/ with the source of this build.
const licenseOutput = path.join(root, 'public/licenses');
await rm(licenseOutput, { recursive: true, force: true });
await cp(path.join(root, 'licenses'), licenseOutput, { recursive: true });
const packageDirectories = [];
for (const entry of await readdir(path.join(root, 'node_modules'), { withFileTypes: true })) {
  if (!entry.isDirectory() || entry.name.startsWith('.')) continue;
  if (entry.name.startsWith('@')) {
    for (const child of await readdir(path.join(root, 'node_modules', entry.name))) packageDirectories.push(path.join(root, 'node_modules', entry.name, child));
  } else packageDirectories.push(path.join(root, 'node_modules', entry.name));
}
const notices = [];
for (const directory of packageDirectories) {
  let manifest;
  try { manifest = JSON.parse(await readFile(path.join(directory, 'package.json'), 'utf8')); } catch { continue; }
  notices.push({ name: manifest.name, version: manifest.version, license: manifest.license });
  for (const file of await readdir(directory)) {
    if (!/^(license|notice|thirdpartynotices)([.-]|$)/i.test(file)) continue;
    await cp(path.join(directory, file), path.join(licenseOutput, manifest.name.replaceAll('/', '-') + '-' + file), { recursive: true });
  }
}
await writeFile(path.join(licenseOutput, 'packages.json'), JSON.stringify(notices, null, 2) + '\n');
