import { useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Account, type Identity } from './account';
import { Maintenance, Statistics } from './service';
import { Preview } from './preview';
import { Cameras } from './cameras';
import { Farm, farmLinks, isFarmRoute } from './farm';
import { Authorize, Mcp } from './mcp';
import { read, submit, RequestError, message } from './http';
import './ui.css';

type Gate = {
  mode: 'setup' | 'login' | 'ready';
  user?: Identity;
  secured: boolean;
};
async function check(): Promise<Gate> {
  const { auth_enabled, requires_setup } = await read<{
    auth_enabled: boolean;
    requires_setup: boolean;
  }>('/api/v1/auth/status');
  if (requires_setup) return { mode: 'setup', secured: false };
  if (!auth_enabled) return { mode: 'ready', secured: false };
  if (sessionStorage.getItem('bambastic.session')) {
    try {
      return {
        mode: 'ready',
        secured: true,
        user: await read<Identity>('/api/v1/auth/me'),
      };
    } catch (e) {
      if (!(e instanceof RequestError) || e.status !== 401) throw e;
      sessionStorage.removeItem('bambastic.session');
    }
  }
  return { mode: 'login', secured: true };
}

function App() {
  const [gate, setGate] = useState<Gate>();
  const [error, setError] = useState('');
  const [version, refresh] = useState(0);
  const retry = () => refresh((v) => v + 1);
  useEffect(() => {
    let active = true;
    setError('');
    setGate(undefined);
    check()
      .then((v) => {
        if (active) setGate(v);
      })
      .catch((e) => {
        if (active) setError(message(e));
      });
    return () => {
      active = false;
    };
  }, [version]);
  async function signOut() {
    try {
      await submit('/api/v1/auth/logout', {});
      sessionStorage.removeItem('bambastic.session');
      retry();
    } catch (e) {
      setError(message(e));
    }
  }
  const requested = location.pathname.replace(/\/$/, '') || '/';
  const route = ['/printers', '/setup', '/login'].includes(requested)
    ? '/'
    : requested;
  const cameraId = /^\/camera\/(\d+)$/.exec(route)?.[1];
  const displayToken = new URLSearchParams(location.search).get('token') ?? '';
  const publicCamera = (route === '/camwall' || !!cameraId) && !!displayToken;
  const consoleVisible =
    !publicCamera && gate?.mode === 'ready' && route !== '/authorize-mcp';
  return (
    <div className="workspace">
      <header className="mast">
        <a href="/" className="wordmark">
          Bambastic
        </a>
        {gate?.user && (
          <div className="account">
            <span>{gate.user.username}</span>
            <button onClick={signOut}>Sign out</button>
          </div>
        )}
      </header>
      {consoleVisible && (
        <nav aria-label="Farm navigation">
          {[
            ...farmLinks,
            { href: '/camwall', text: 'Cameras' },
            { href: '/maintenance', text: 'Maintenance' },
            { href: '/stats', text: 'Statistics' },
            { href: '/mcp-guide', text: 'MCP connections' },
          ].map((link) => (
            <a
              href={link.href}
              key={link.href}
              aria-current={route === link.href ? 'page' : undefined}
            >
              {link.text}
            </a>
          ))}
        </nav>
      )}
      <main>
        {error && (
          <p role="alert">
            {error} <button onClick={retry}>Retry</button>
          </p>
        )}
        {publicCamera ? (
          <Cameras
            id={cameraId ? Number(cameraId) : undefined}
            displayToken={displayToken}
            secured={true}
          />
        ) : !gate ? (
          !error && <p role="status">Connecting to the farm…</p>
        ) : gate.mode !== 'ready' ? (
          <Account
            key={gate.mode}
            setup={gate.mode === 'setup'}
            onReady={retry}
          />
        ) : route === '/camwall' || cameraId ? (
          <Cameras
            id={cameraId ? Number(cameraId) : undefined}
            secured={gate.secured}
          />
        ) : route === '/gcode-viewer' ? (
          <Preview
            id={Number(new URLSearchParams(location.search).get('id'))}
            source={
              new URLSearchParams(location.search).get('source') === 'library'
                ? 'library/files'
                : 'archives'
            }
          />
        ) : route === '/stats' ? (
          <Statistics />
        ) : route === '/maintenance' ? (
          <Maintenance
            canRecord={
              !gate.secured ||
              !!gate.user?.is_admin ||
              !!gate.user?.permissions.includes('maintenance:update')
            }
          />
        ) : route === '/authorize-mcp' ? (
          <Authorize />
        ) : route === '/mcp-guide' ? (
          <Mcp secured={gate.secured} />
        ) : isFarmRoute(route) ? (
          <Farm
            key={route}
            route={route}
            allowed={(permission) =>
              !gate.secured ||
              !!gate.user?.is_admin ||
              !!gate.user?.permissions.includes(permission)
            }
          />
        ) : (
          <section className="sheet">
            <h1>Page unavailable</h1>
            <a href="/">Return to the fleet</a>
          </section>
        )}
      </main>
    </div>
  );
}
createRoot(document.getElementById('app')!).render(<App />);
