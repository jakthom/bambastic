import { useEffect, useState } from 'react';
import { getAuthToken } from '../api/client';

interface Consent {
  client_name?: string;
  username?: string;
  redirect_uri: string;
}

async function requestConsent(decision: string): Promise<Consent> {
  const args = Object.fromEntries(new URLSearchParams(window.location.search));
  const response = await fetch('/api/v1/mcp/consent', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + (getAuthToken() ?? '') },
    body: JSON.stringify({ ...args, decision }),
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.detail || result.error || 'Cannot authorize this client');
  return result;
}

export function McpAuthorizePage() {
  const [request, setRequest] = useState<Consent | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  useEffect(() => { let active = true; requestConsent('inspect').then(v => { if (active) setRequest(v); }).catch(e => { if (active) setError(e.message); }); return () => { active = false; }; }, []);
  async function decide(value: string) {
    setBusy(true);
    try { window.location.assign((await requestConsent(value)).redirect_uri); }
    catch (e) { setError(e instanceof Error ? e.message : 'Authorization failed'); setBusy(false); }
  }
  return <main className="max-w-lg mx-auto p-8 space-y-5">
    <h1 className="text-2xl font-semibold">Connect an MCP client</h1>
    {error && <p role="alert">{error}</p>}
    {request ? <>
      <p><strong>{request.client_name || 'This client'}</strong> is requesting access to your printers and print workflows as <strong>{request.username}</strong>.</p>
      <p>It can read data and request actions within your Bambastic permissions. Your approval returns to <strong>{new URL(request.redirect_uri).origin}</strong>.</p>
      <div className="flex gap-4"><button className="rounded px-4 py-2 border" disabled={busy} onClick={() => decide('deny')}>Deny</button><button className="rounded bg-bambu-green text-white px-4 py-2" disabled={busy} onClick={() => decide('allow')}>Allow access</button></div>
    </> : !error && <p>Checking client…</p>}
  </main>;
}
