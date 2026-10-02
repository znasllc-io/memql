import { defineConfig } from 'vite';
export default defineConfig({
  base: '/editor/',
  build: { target: 'es2022', assetsInlineLimit: 0, rollupOptions: { output: { assetFileNames: asset => asset.names?.includes('webWorkerExtensionHostIframe.html') ? 'assets/extension-host.html' : 'assets/[name]-[hash][extname]' } } },
  worker: { format: 'es' },
  optimizeDeps: { noDiscovery: true, include: [] },
});
