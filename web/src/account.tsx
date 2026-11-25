import { useEffect, useState, type FormEvent } from 'react';
import { message, read, submit } from './http';

export interface Identity {
  username: string;
  is_admin: boolean;
  permissions: string[];
}
interface Login {
  user?: Identity;
  access_token?: string;
  requires_2fa?: boolean;
  pre_auth_token?: string;
  two_fa_methods?: string[];
}

export function Account({
  setup,
  onReady,
}: {
  setup: boolean;
  onReady: () => void;
}) {
  const [challenge, setChallenge] = useState<Login>();
  const [method, setMethod] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [providers, setProviders] = useState<{ id: number; name: string }[]>(
    [],
  );
  const [localEnabled, setLocalEnabled] = useState(true);
  function finish(result: Login) {
    if (result.requires_2fa && result.pre_auth_token) {
      setChallenge(result);
      setMethod(result.two_fa_methods?.[0] ?? 'totp');
    } else if (result.access_token) {
      sessionStorage.setItem('bambastic.session', result.access_token);
      const returnPath = sessionStorage.getItem('bambastic.sso.return');
      sessionStorage.removeItem('bambastic.sso.return');
      if (location.pathname === '/login' && returnPath) {
        const target = new URL(returnPath, location.origin);
        if (target.origin === location.origin) {
          location.replace(target.pathname + target.search);
          return;
        }
      }
      onReady();
    } else throw new Error('The server did not return a session.');
  }
  async function sso(id: number) {
    setBusy(true);
    setError('');
    try {
      const { auth_url } = await read<{ auth_url: string }>(
        '/api/v1/auth/oidc/authorize/' + id,
      );
      sessionStorage.setItem(
        'bambastic.sso.return',
        location.pathname + location.search,
      );
      location.assign(auth_url);
    } catch (e) {
      setError(message(e));
      setBusy(false);
    }
  }
  useEffect(() => {
    if (setup) return;
    const token = new URLSearchParams(location.hash.slice(1)).get('oidc_token');
    if (token && location.pathname === '/login') {
      history.replaceState(null, '', location.pathname + location.search);
      setBusy(true);
      submit<Login>('/api/v1/auth/oidc/exchange', { oidc_token: token })
        .then(finish)
        .catch((e) => setError(message(e)))
        .finally(() => setBusy(false));
      return;
    }
    let active = true;
    const fallback =
      new URLSearchParams(location.search).get('fallback') === 'local';
    Promise.all([
      read<{ id: number; name: string }[]>('/api/v1/auth/oidc/providers'),
      read<{
        local_login_enabled: boolean;
        autologin_provider_id: number | null;
      }>('/api/v1/auth/advanced-auth/status'),
    ])
      .then(([list, settings]) => {
        if (!active) return;
        setProviders(list);
        setLocalEnabled(fallback || settings.local_login_enabled);
        if (
          !fallback &&
          settings.autologin_provider_id &&
          !new URLSearchParams(location.search).has('oidc_error')
        )
          void sso(settings.autologin_provider_id);
      })
      .catch((e) => {
        if (active) setError(message(e));
      });
    return () => {
      active = false;
    };
  }, [setup]);
  async function proceed(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = Object.fromEntries(new FormData(event.currentTarget));
    setBusy(true);
    setError('');
    try {
      if (setup) {
        await submit('/api/v1/auth/setup', {
          auth_enabled: true,
          admin_username: data.username,
          admin_password: data.password,
        });
        onReady();
        return;
      }
      const result = challenge
        ? await submit<Login>('/api/v1/auth/2fa/verify', {
            code: data.code,
            method,
            pre_auth_token: challenge.pre_auth_token,
          })
        : await submit<Login>('/api/v1/auth/login', data);
      finish(result);
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  }
  async function sendEmail() {
    setBusy(true);
    setError('');
    try {
      const result = await submit<Login>('/api/v1/auth/2fa/email/send', {
        pre_auth_token: challenge?.pre_auth_token,
      });
      if (result.pre_auth_token)
        setChallenge({ ...challenge, pre_auth_token: result.pre_auth_token });
      setNotice('Check your email for a verification code.');
    } catch (err) {
      setError(message(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="sheet narrow">
      <h1>
        {setup
          ? 'Set up your farm'
          : challenge
            ? 'Verify your sign-in'
            : 'Sign in to Bambastic'}
      </h1>
      {!setup &&
        !challenge &&
        providers.map((p) => (
          <button
            key={p.id}
            type="button"
            disabled={busy}
            onClick={() => sso(p.id)}
          >
            Sign in with {p.name}
          </button>
        ))}
      {error && <p role="alert">{error}</p>}
      {(setup || challenge || localEnabled) && (
        <form onSubmit={proceed}>
          {challenge ? (
            <>
              <label>
                Verification method
                <select
                  value={method}
                  onChange={(e) => setMethod(e.target.value)}
                >
                  {challenge.two_fa_methods?.map((m) => (
                    <option key={m} value={m}>
                      {{
                        totp: 'Authenticator app',
                        backup: 'Backup code',
                        email: 'Email',
                      }[m] ?? m}
                    </option>
                  ))}
                </select>
              </label>
              {method === 'email' && (
                <button type="button" disabled={busy} onClick={sendEmail}>
                  Send code
                </button>
              )}
              {notice && <p role="status">{notice}</p>}
              <label>
                Verification code
                <input name="code" autoComplete="one-time-code" required />
              </label>
            </>
          ) : (
            <>
              <label>
                Username
                <input name="username" autoComplete="username" required />
              </label>
              <label>
                Password
                <input
                  name="password"
                  type="password"
                  autoComplete={setup ? 'new-password' : 'current-password'}
                  required
                />
              </label>
              {setup && (
                <p>
                  Use at least eight characters with uppercase, lowercase, a
                  number and a symbol.
                </p>
              )}
            </>
          )}
          <button className="primary" disabled={busy}>
            {setup ? 'Create administrator' : challenge ? 'Verify' : 'Sign in'}
          </button>
        </form>
      )}
      {setup && (
        <button
          className="text"
          disabled={busy}
          onClick={async () => {
            if (
              !window.confirm(
                'Allow anyone who can reach this installation to operate the farm?',
              )
            )
              return;
            setBusy(true);
            try {
              await submit('/api/v1/auth/setup', { auth_enabled: false });
              onReady();
            } catch (e) {
              setError(message(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          Continue without accounts
        </button>
      )}
    </section>
  );
}
