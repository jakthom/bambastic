import { useEffect, useState } from 'react';
import { message, read, submit } from './http';

function Copy({ value }: { value: string }) {
  const [status, feedback] = useState('');
  async function copy() {
    try {
      if (navigator.clipboard && window.isSecureContext)
        await navigator.clipboard.writeText(value);
      else {
        const field = document.createElement('textarea');
        field.value = value;
        field.style.position = 'fixed';
        field.style.opacity = '0';
        document.body.appendChild(field);
        try {
          field.select();
          if (!document.execCommand('copy')) throw Error();
        } finally {
          field.remove();
        }
      }
      feedback('Copied');
    } catch {
      feedback('Select the text to copy');
    }
  }
  return (
    <button className="snippet" onClick={copy}>
      <code>{value}</code>
      <span aria-live="polite">{status || 'Copy'}</span>
    </button>
  );
}

const clients = [
  'Codex',
  'Claude Code',
  'ChatGPT',
  'Claude',
  'Grok',
  'LM Studio',
  'Goose + Ollama',
  'Other stdio client',
];
const quote = (s: string) => "'" + s.replaceAll("'", "'\\''") + "'";
export function Mcp({ secured }: { secured: boolean }) {
  const [url, setUrl] = useState(location.origin + '/mcp');
  const [client, select] = useState(clients[0]);
  const [local, setLocal] = useState(false);
  const [key, setKey] = useState(false);
  useEffect(() => {
    const stop = new AbortController();
    read<{ resource: string }>('/.well-known/oauth-protected-resource/mcp', {
      signal: stop.signal,
    })
      .then(({ resource }) => {
        const address = new URL(resource);
        if (
          ['https:', 'http:'].includes(address.protocol) &&
          resource === address.origin + '/mcp'
        )
          setUrl(resource);
      })
      .catch(() => {});
    return () => stop.abort();
  }, []);
  const origin = new URL(url).origin;
  const cli = client === 'Codex' || client === 'Claude Code';
  const stdio = (cli && local) || client === 'Other stdio client';
  const command = '/absolute/path/to/bambastic';
  const args = ['mcp', '--stdio', '--server', origin];
  const commandArgs = `-- ${quote(command)} mcp --stdio --server ${quote(origin)}`;
  const env = secured
    ? ' --env BAMBASTIC_API_KEY="${BAMBASTIC_API_KEY:?}"'
    : '';
  let snippet = url;
  let help =
    'Paste this URL into the provider’s custom MCP connector. The provider must be able to reach it over HTTPS.';
  if (cli) {
    if (stdio) {
      snippet =
        (client === 'Codex'
          ? 'codex mcp add bambastic'
          : 'claude mcp add --transport stdio bambastic') +
        env +
        ' ' +
        commandArgs;
      help =
        'Keep the farm server running. Replace the executable path with the full path on your computer.' +
        (secured
          ? ' Export BAMBASTIC_API_KEY before registration. The client stores that key.'
          : '');
    } else {
      snippet =
        client === 'Codex'
          ? `codex mcp add bambastic --url ${quote(url)}${key ? ' --bearer-token-env-var BAMBASTIC_API_KEY' : ''}`
          : `claude mcp add --transport http bambastic ${quote(url)}`;
      help =
        client === 'Codex' && key
          ? 'Export your scoped key as BAMBASTIC_API_KEY where you start Codex.'
          : 'Run the command on the computer where you use the CLI.';
    }
  } else if (client === 'Goose + Ollama') {
    snippet = 'goose session --with-streamable-http-extension ' + quote(url);
    help = 'Configure the Ollama provider in Goose, then run this command.';
  } else if (client === 'LM Studio' || stdio) {
    const server = stdio
      ? {
          command,
          args,
          ...(secured && { env: { BAMBASTIC_API_KEY: '<API key>' } }),
        }
      : {
          url,
          ...(secured && { headers: { Authorization: 'Bearer <API key>' } }),
        };
    snippet = JSON.stringify({ mcpServers: { bambastic: server } }, null, 2);
    help =
      'Add this server to your MCP configuration.' +
      (stdio ? ' Replace the executable path with the full local path.' : '') +
      (secured ? ' Replace the key placeholder with your scoped key.' : '');
  }
  return (
    <>
      <h1>MCP connections</h1>
      <section className="sheet">
        <label>
          Client
          <select
            aria-label="Client"
            value={client}
            onChange={(e) => select(e.target.value)}
          >
            {clients.map((c) => (
              <option key={c}>{c}</option>
            ))}
          </select>
        </label>
        {cli && (
          <label>
            Connection
            <select
              value={local ? 'local' : 'http'}
              onChange={(e) => setLocal(e.target.value === 'local')}
            >
              <option value="http">Server URL (HTTP)</option>
              <option value="local">Local executable (stdio)</option>
            </select>
          </label>
        )}
        {client === 'Codex' && !local && (
          <label className="inline">
            <input
              type="checkbox"
              checked={key}
              onChange={(e) => setKey(e.target.checked)}
            />
            Use a bearer token
          </label>
        )}
        <p>{help}</p>
        <Copy key={snippet} value={snippet} />
        {client === 'Codex' && !local && !key && secured && (
          <>
            <p>Sign in and approve access to finish connecting.</p>
            <Copy value="codex mcp login bambastic" />
          </>
        )}
        {client === 'Claude Code' && (
          <p>
            Open <code>/mcp</code> to check the connection.{' '}
            <a href="https://code.claude.com/docs/en/mcp-quickstart">
              Claude Code quickstart
            </a>
          </p>
        )}
      </section>
    </>
  );
}

interface Consent {
  client_name: string;
  username: string;
  redirect_uri: string;
}
export function Authorize() {
  const [request, setRequest] = useState<Consent>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const args = Object.fromEntries(new URLSearchParams(location.search));
  useEffect(() => {
    let current = true;
    submit<Consent>('/api/v1/mcp/consent', {
      ...Object.fromEntries(new URLSearchParams(location.search)),
      decision: 'inspect',
    })
      .then((value) => {
        if (current) setRequest(value);
      })
      .catch((e) => {
        if (current) setError(message(e));
      });
    return () => {
      current = false;
    };
  }, []);
  async function decide(decision: string) {
    setBusy(true);
    try {
      const { redirect_uri } = await submit<Consent>('/api/v1/mcp/consent', {
        ...args,
        decision,
      });
      location.assign(redirect_uri);
    } catch (e) {
      setError(message(e));
      setBusy(false);
    }
  }
  return (
    <section className="sheet narrow">
      <h1>Authorize connection</h1>
      {error && <p role="alert">{error}</p>}
      {request ? (
        <>
          <p>
            <strong>{request.client_name || 'MCP client'}</strong> requests
            access to this farm with the permissions of{' '}
            <strong>{request.username}</strong>.
          </p>
          <p>
            Your browser will return to{' '}
            <code>{new URL(request.redirect_uri).origin}</code>.
          </p>
          <div className="actions">
            <button disabled={busy} onClick={() => decide('deny')}>
              Deny
            </button>
            <button
              className="primary"
              disabled={busy}
              onClick={() => decide('allow')}
            >
              Allow access
            </button>
          </div>
        </>
      ) : (
        !error && <p role="status">Checking authorization request…</p>
      )}
    </section>
  );
}
