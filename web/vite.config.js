import { readFileSync } from 'node:fs';
import { defineConfig } from 'vite';

const backend = process.env.BAMBASTIC_URL || 'http://127.0.0.1:8080';
export default defineConfig({
  plugins: [
    {
      name: 'console-license',
      generateBundle() {
        this.emitFile({
          type: 'asset',
          fileName: 'LICENSE.txt',
          source: readFileSync(new URL('./LICENSE', import.meta.url), 'utf8'),
        });
      },
    },
  ],
  build: {
    target: 'safari16',
    license: { fileName: 'third-party-licenses.md' },
  },
  server: {
    proxy: {
      '/api/': { target: backend, ws: true },
      '/.well-known/': backend,
      '/oauth/': backend,
      '^/mcp$': backend,
    },
  },
});
