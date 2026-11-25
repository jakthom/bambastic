import { expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../mocks/server';

vi.mock('../../pages/LoginPage', () => ({
  LoginPage() { throw new Error('Simulated login render failure'); },
}));

it('sends the real React boundary failure with its component stack before sign-in', async () => {
  const reports: Record<string, unknown>[] = [];
  server.use(
    http.post('*/uie', () => new HttpResponse(null, { status: 204 })),
    http.post('*/crashes', async ({ request }) => {
      const body = await request.json() as { crash: Record<string, unknown> };
      reports.push(body.crash);
      return new HttpResponse(null, { status: 204 });
    }),
    http.get('*/api/v1/auth/status', () => HttpResponse.json({ auth_enabled: true, requires_setup: false })),
  );
  const root = document.createElement('div');
  root.id = 'root';
  document.body.append(root);
  window.history.replaceState(null, '', '/login');
  await import('../../entry');
  expect(await screen.findByRole('heading', { name: 'UI Crash' }, { timeout: 8000 })).toBeInTheDocument();
  await waitFor(() => expect(reports).toContainEqual(expect.objectContaining({
    source: 'react.boundary', message: 'Simulated login render failure',
    componentStack: expect.stringContaining('LoginPage'),
    context: expect.objectContaining({ route: '/login' }),
  })));
});
