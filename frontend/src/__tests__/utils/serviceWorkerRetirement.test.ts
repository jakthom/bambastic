import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';
import { describe, expect, it, vi } from 'vitest';

const origin = 'http://127.0.0.1:8080';
const readScript = (name: string) => readFileSync(resolve(__dirname, '../../../public', name), 'utf8');
const appCaches = ['bambastic-v1', 'bambastic-static-v1', 'bambuddy-v31', 'bambuddy-static-v30'];

function cacheStorage() {
  const remaining = new Set([...appCaches, 'some-other-cache', 'bambastic-editor-drafts']);
  return {
    remaining,
    keys: () => Promise.resolve([...remaining]),
    delete: (name: string) => Promise.resolve(remaining.delete(name)),
  };
}

function registration(script = '/sw.js', scope = '/') {
  return {
    scope: origin + scope,
    active: { scriptURL: origin + script },
    update: vi.fn().mockResolvedValue(undefined),
    unregister: vi.fn().mockResolvedValue(true),
  };
}

async function visit(existing?: ReturnType<typeof registration>) {
  const caches = cacheStorage();
  const serviceWorker = {
    getRegistration: vi.fn().mockResolvedValue(existing),
    register: vi.fn(),
  };
  await runInNewContext(readScript('sw-register.js'), {
    location: { origin }, window: { caches, addEventListener: vi.fn() }, navigator: { serviceWorker },
  });
  expect(serviceWorker.register).not.toHaveBeenCalled();
  expect([...caches.remaining]).toEqual(['some-other-cache', 'bambastic-editor-drafts']);
}

describe('offline shell retirement', () => {
  it('reloads a frozen history page, including in browsers without worker support', async () => {
    const reload = vi.fn();
    const addEventListener = vi.fn();
    await runInNewContext(readScript('sw-register.js'), {
      location: { origin, reload }, window: { addEventListener }, navigator: {},
    });
    const handle = addEventListener.mock.calls.find(([name]) => name === 'pageshow')![1];
    handle({ persisted: false });
    expect(reload).not.toHaveBeenCalled();
    handle({ persisted: true });
    expect(reload).toHaveBeenCalledOnce();
  });

  it('cleans up orphaned caches without installing a worker', async () => {
    await visit();
  });

  it('updates the old root worker and unregisters locally if the update cannot reach the server', async () => {
    const legacy = registration();
    await visit(legacy);
    expect(legacy.update).toHaveBeenCalledOnce();
    expect(legacy.unregister).not.toHaveBeenCalled();

    legacy.update.mockRejectedValueOnce(new Error('offline'));
    await visit(legacy);
    expect(legacy.unregister).toHaveBeenCalledOnce();
  });

  it('leaves unrelated worker scripts and scopes alone', async () => {
    for (const unrelated of [registration('/other.js'), registration('/sw.js', '/another/')]) {
      await visit(unrelated);
      expect(unrelated.update).not.toHaveBeenCalled();
      expect(unrelated.unregister).not.toHaveBeenCalled();
    }
  });

  it('replaces the caching worker, removes its saved responses and has no offline fetch handler', async () => {
    type Handler = (event: { waitUntil: (task: Promise<unknown>) => void }) => void;
    const handlers = new Map<string, Handler>();
    const caches = cacheStorage();
    const self = {
      addEventListener: (name: string, handler: Handler) => handlers.set(name, handler),
      skipWaiting: vi.fn().mockResolvedValue(undefined),
      clients: { claim: vi.fn().mockResolvedValue(undefined) },
      registration: registration(),
    };
    runInNewContext(readScript('sw.js'), { self, caches });
    expect(handlers.has('fetch')).toBe(false);
    for (const name of ['install', 'activate']) {
      const tasks: Promise<unknown>[] = [];
      handlers.get(name)!({ waitUntil: (task) => { tasks.push(task); } });
      await Promise.all(tasks);
    }
    expect(self.skipWaiting).toHaveBeenCalledOnce();
    expect(self.clients.claim).toHaveBeenCalledOnce();
    expect(self.registration.unregister).toHaveBeenCalledOnce();
    expect([...caches.remaining]).toEqual(['some-other-cache', 'bambastic-editor-drafts']);
  });
});
