import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';

const root = fileURLToPath(new URL('../', import.meta.url));
const publicHost = process.env.BAMBASTIC_PUBLIC_URL;
const vite = await createServer({
  root,
  mode: 'integrated',
  server: {
    host: '127.0.0.1',
    port: 0,
    allowedHosts: publicHost ? [new URL(publicHost).hostname] : [],
  },
});
try {
  await vite.listen();
  const viteOrigin = 'http://127.0.0.1:' + vite.httpServer.address().port;
  const backend = spawn(process.argv[2], ['-c', '.air.toml'], {
    env: { ...process.env, BAMBASTIC_VITE_URL: viteOrigin },
    stdio: 'inherit',
  });
  const stop = () => backend.kill('SIGINT');
  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);
  const [code] = await once(backend, 'exit');
  process.exitCode = code ?? 0;
} finally {
  await vite.close();
}
