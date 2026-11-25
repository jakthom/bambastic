import { readFileSync } from 'node:fs';
import { defineConfig } from 'vite';

const backend =
  process.env.BAMBASTIC_URL ||
  'http://127.0.0.1:' + (process.env.BACKEND_PORT || '8080');
export default defineConfig(({ mode }) => ({
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
    proxy:
      mode === 'integrated'
        ? undefined
        : {
            '/uie': backend,
            '/crashes': backend,
            '/api/': { target: backend, ws: true },
            '/.well-known/': backend,
            '/oauth/': backend,
            '^/mcp$': backend,
          },
  },
}));
