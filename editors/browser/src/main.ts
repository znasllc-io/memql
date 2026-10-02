import { requestedResource, checkTools } from './handoff.mjs';
import { BrowserSecrets } from './secrets';
import { initialize } from '@codingame/monaco-vscode-api';
import { registerExtension, ExtensionHostKind, type IExtensionManifest } from '@codingame/monaco-vscode-api/extensions';
import getWorkbench from '@codingame/monaco-vscode-workbench-service-override';
import getExtensions from '@codingame/monaco-vscode-extensions-service-override';
import getConfiguration from '@codingame/monaco-vscode-configuration-service-override';
import getDialogs from '@codingame/monaco-vscode-dialogs-service-override';
import getModel from '@codingame/monaco-vscode-model-service-override';
import getNotifications from '@codingame/monaco-vscode-notifications-service-override';
import getTextmate from '@codingame/monaco-vscode-textmate-service-override';
import getTheme from '@codingame/monaco-vscode-theme-service-override';
import getLanguages from '@codingame/monaco-vscode-languages-service-override';
import getPreferences from '@codingame/monaco-vscode-preferences-service-override';
import getStorage from '@codingame/monaco-vscode-storage-service-override';
import getSecrets from '@codingame/monaco-vscode-secret-storage-service-override';
import getLifecycle from '@codingame/monaco-vscode-lifecycle-service-override';
import getWorkingCopy from '@codingame/monaco-vscode-working-copy-service-override';
import getTrust from '@codingame/monaco-vscode-workspace-trust-service-override';
import getExplorer from '@codingame/monaco-vscode-explorer-service-override';
import getOutput from '@codingame/monaco-vscode-output-service-override';
import getGallery from '@codingame/monaco-vscode-extension-gallery-service-override';
import { createIndexedDBProviders, registerHTMLFileSystemProvider } from '@codingame/monaco-vscode-files-service-override';
import '@codingame/monaco-vscode-theme-defaults-default-extension';
import '@codingame/monaco-vscode-json-default-extension';
import '@codingame/monaco-vscode-markdown-basics-default-extension';
import '@codingame/monaco-vscode-typescript-basics-default-extension';
import '@codingame/monaco-vscode-html-default-extension';
import '@codingame/monaco-vscode-css-default-extension';
import 'vscode/localExtensionHost';
import bundledExtensions from './bundled-extensions.json';

// Vite needs statically declared Worker constructors to bundle worker modules.
const workerFactories = {
  editorWorkerService: () => new Worker(new URL('monaco-editor/esm/vs/editor/editor.worker.js', import.meta.url), { type: 'module' }),
  extensionHostWorkerMain: () => new Worker(new URL('@codingame/monaco-vscode-api/workers/extensionHost.worker', import.meta.url), { type: 'module' }),
  TextMateWorker: () => new Worker(new URL('@codingame/monaco-vscode-textmate-service-override/worker', import.meta.url), { type: 'module' }),
  OutputLinkDetectionWorker: () => new Worker(new URL('@codingame/monaco-vscode-output-service-override/worker', import.meta.url), { type: 'module' }),
};
// Extension host asks for a URL because its isolated frame owns the worker.
import extensionHostUrl from '@codingame/monaco-vscode-api/workers/extensionHost.worker?worker&url';
window.MonacoEnvironment = {
  getWorker: (_module: string, label: keyof typeof workerFactories) => workerFactories[label](),
  getWorkerUrl: (_module: string, label: string) => label === 'extensionHostWorkerMain' ? new URL(extensionHostUrl, location.href).href : undefined,
  getWorkerOptions: () => ({ type: 'module' }),
};
export async function start(isBasic: () => boolean, ready: () => void) {
  await createIndexedDBProviders();
  registerHTMLFileSystemProvider();
  for (const bundle of (isBasic() ? [] : bundledExtensions)) {
    const extension = registerExtension(bundle.manifest as unknown as IExtensionManifest, ExtensionHostKind.LocalWebWorker, { system: true });
    for (const file of bundle.files) extension.registerFileUrl(file, new URL(`extensions/${bundle.name}/${file}`, document.baseURI).href);
  }
  const host = registerExtension({ name: 'editor-host', publisher: 'znasllc', version: '0.1.0', engines: { vscode: '*' } }, ExtensionHostKind.LocalProcess);
  const editorHost = location.hostname;
  const dot = editorHost.indexOf('.');
  const isolatedOrigin = `https://${editorHost.slice(0, dot)}--assets--{{uuid}}${editorHost.slice(dot)}`;
  await initialize({
    ...getWorkbench(undefined, isolatedOrigin), ...getExtensions({ enableWorkerExtensionHost: true }),
    ...getConfiguration(), ...getDialogs(), ...getModel(), ...getNotifications(),
    ...getTextmate(), ...getTheme(), ...getLanguages(), ...getPreferences(),
    ...getStorage(), ...getSecrets(), ...getLifecycle(), ...getWorkingCopy(),
    ...getTrust(), ...getExplorer(), ...getOutput(), ...getGallery({ webOnly: true }),
  }, document.querySelector<HTMLElement>('#workbench')!, {
    workspaceProvider: { workspace: undefined, trusted: true, async open() { return false; } },
    enableWorkspaceTrust: true,
    secretStorageProvider: new BrowserSecrets(),
    configurationDefaults: { 'workbench.startupEditor': 'none', 'telemetry.telemetryLevel': 'off', 'window.title': '${dirty}${activeEditorShort}${separator}MemQL Editor' },
    productConfiguration: { nameShort: 'MemQL Editor', nameLong: 'MemQL Editor', enableTelemetry: false },
  });
  const api = await host.getApi();
  if (isBasic()) { ready(); return; }
  await checkTools((command: string) => api.commands.executeCommand(command));
  if (isBasic()) { ready(); return; }
  api.commands.registerCommand('memql.editor.showTools', () => api.commands.executeCommand('workbench.extensions.search', '@builtin memql'));
  const toolsStatus = api.window.createStatusBarItem('memql.editor.tools', api.StatusBarAlignment.Left, 100);
  toolsStatus.text = '$(check) MemQL tools active';
  toolsStatus.tooltip = 'MemQL and MemQL Productivity Tools are included and active. Click to see both extensions.';
  toolsStatus.command = 'memql.editor.showTools';
  toolsStatus.show();
  const resource = requestedResource(location.href);
  ready();
  if (resource) {
    try { await api.commands.executeCommand('vscode.open', api.Uri.parse(resource)); }
    catch (error) { await api.window.showErrorMessage(`Could not open the MemQL file: ${error instanceof Error ? error.message : String(error)}`); }
  }
}
