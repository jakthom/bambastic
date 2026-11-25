import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createServer as createHttpServer } from 'node:http';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';

// Go owns the public address and forwards only browser requests to Vite.
const httpServer = createHttpServer();
const allowedHosts = [];
if (process.env.BAMBASTIC_PUBLIC_URL) allowedHosts.push(new URL(process.env.BAMBASTIC_PUBLIC_URL).hostname);
const addr = process.env.BAMBASTIC_ADDR ?? '';
const listenHost = addr.slice(0, addr.lastIndexOf(':'));
if (listenHost) allowedHosts.push(listenHost);
const vite = await createServer({
  root: fileURLToPath(new URL('../', import.meta.url)),
  mode: 'integrated',
  clearScreen: false,
  server: { middlewareMode: true, ws: { server: httpServer }, allowedHosts },
});

try {
  httpServer.on('request', vite.middlewares);
  httpServer.listen(0, '127.0.0.1');
  await once(httpServer, 'listening');
  const port = httpServer.address().port;
  const air = spawn(process.argv[2], ['-c', '.air.toml'], {
    env: { ...process.env, BAMBASTIC_VITE_URL: `http://127.0.0.1:${port}` },
    stdio: 'inherit',
  });
  const stop = () => air.kill('SIGINT');
  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);
  const [code, signal] = await once(air, 'exit');
  process.exitCode = code ?? (signal === 'SIGINT' ? 0 : 1);
} finally {
  httpServer.close();
  await vite.close();
}
