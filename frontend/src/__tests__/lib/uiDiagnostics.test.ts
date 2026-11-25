import { expect, it, vi } from 'vitest';
import { installDiagnostics } from '../../diagnostics/browser';

it('correlates input activity and crash context, retries a failed crash delivery and omits secrets', async () => {
  vi.useFakeTimers();
  const delivered: { url: string; body: string }[] = [];
  let failCrash = true;
  const transport = vi.spyOn(window, 'fetch').mockImplementation(async (input, init) => {
    const url = String(input);
    if (url === '/crashes' && failCrash) {
      failCrash = false;
      return new Response(null, { status: 503 });
    }
    delivered.push({ url, body: String(init?.body) });
    return new Response(null, { status: 204 });
  });
  try {
    installDiagnostics();
    window.history.replaceState(null, '', '/confirm/fixture-secret/accept?token=fixture-secret');
    const input = document.createElement('input');
    input.type = 'password';
    input.value = 'fixture-secret';
    document.body.append(input);
    input.dispatchEvent(new Event('input', { bubbles: true }));
    window.bambasticDiagnostics!.context('theme', { mode: 'dark', resolvedMode: 'dark' });
    window.bambasticDiagnostics!.crash('react.boundary', new Error('Failed password=fixture-secret'), 'at Layout');
    await vi.advanceTimersByTimeAsync(1600);
    const events = delivered.filter(r => r.url === '/uie').map(r => JSON.parse(r.body));
    const crash = JSON.parse(delivered.find(r => r.url === '/crashes')!.body);
    expect(crash.session).toBe(events[0].session);
    expect(crash.crash).toMatchObject({ source: 'react.boundary', componentStack: 'at Layout',
      context: { theme: { mode: 'dark' } }, message: 'Failed password=[redacted]' });
    expect(crash.crash.stack).toContain('Failed');
    expect(events.flatMap(r => r.events).some(e => e.kind === 'ui.input')).toBe(true);
    expect(crash.crash.recent.some((e: { kind: string }) => e.kind === 'ui.input')).toBe(true);
    expect(JSON.stringify(delivered)).not.toContain('fixture-secret');
    expect(events.flatMap(r => r.events).some(e => e.kind === 'fetch.start')).toBe(false);
    input.remove();
  } finally {
    vi.clearAllTimers();
    vi.useRealTimers();
    transport.mockRestore();
    window.history.replaceState(null, '', '/');
  }
});
