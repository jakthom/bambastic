import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import userEvent from '@testing-library/user-event';
import { fireEvent, render, screen, within } from '../utils';
import { McpGuidePage } from '../../pages/McpGuidePage';
import { server } from '../mocks/server';
import { getMcpConfig } from '../../utils/mcpSetup';

const metadataPath = '/.well-known/oauth-protected-resource/mcp';
beforeEach(() => server.use(http.get(metadataPath, () => HttpResponse.json({ resource: window.location.origin + '/mcp' }))));
afterEach(() => {
  window.history.replaceState({}, '', '/');
  vi.unstubAllGlobals();
});

describe('MCP guide', () => {
  it('opens with ChatGPT and lets an operator configure hosted or local clients', () => {
    window.history.replaceState({}, '', '/mcp-guide');
    render(<McpGuidePage />);
    const client = screen.getByRole('combobox', { name: 'MCP client' });
    expect(client).toHaveValue('chatgpt');
    expect(screen.getByRole('link', { name: 'Open ChatGPT Plugins' })).toHaveAttribute('href', 'https://chatgpt.com/plugins');
    fireEvent.change(client, { target: { value: 'grok' } });
    expect(screen.getByRole('link', { name: 'Open Grok Connectors' })).toHaveAttribute('href', 'https://grok.com/connectors');
    fireEvent.change(client, { target: { value: 'claude' } });
    expect(screen.getByText(/claude mcp add --transport http/)).toHaveTextContent(window.location.origin + '/mcp');
    fireEvent.change(client, { target: { value: 'codex' } });
    expect(screen.getByText(`codex mcp add bambastic --url '${window.location.origin}/mcp'`)).toHaveTextContent(window.location.origin + '/mcp');
    fireEvent.change(client, { target: { value: 'lm' } });
    const config = within(screen.getByRole('group', { name: 'mcp.json' })).getByText(/mcpServers/);
    expect(JSON.parse(config.textContent!).mcpServers.bambastic.url).toBe(window.location.origin + '/mcp');
  });

  it('uses the advertised tailnet origin in the connection details and copied commands', async () => {
    const endpoint = 'https://farm.example.ts.net/mcp';
    server.use(http.get(metadataPath, () => HttpResponse.json({ resource: endpoint })));
    render(<McpGuidePage />);
    expect(await within(screen.getByRole('region', { name: 'Connect an AI assistant' })).findByText(endpoint)).toBeVisible();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'codex' } });
    const add = `codex mcp add bambastic --url '${endpoint}'`;
    expect(screen.getByText(add)).toBeVisible();
    fireEvent.click(screen.getByText('API Keys', { selector: 'summary' }));
    expect(screen.getByText(`${add} --bearer-token-env-var BAMBASTIC_API_KEY`)).toBeVisible();
    fireEvent.click(screen.getByRole('radio', { name: 'Local executable (stdio)' }));
    const args = "'/absolute/path/to/bambastic' mcp --stdio --server 'https://farm.example.ts.net'";
    expect(screen.getByText(`codex mcp add bambastic -- ${args}`)).toBeVisible();

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'claudeCode' } });
    expect(screen.getByText(`claude mcp add --transport stdio bambastic -- ${args}`)).toBeVisible();
    fireEvent.click(screen.getByRole('radio', { name: 'Remote server (HTTP)' }));
    expect(screen.getByText(`claude mcp add --transport http bambastic '${endpoint}'`)).toBeVisible();
    expect(screen.getByRole('link', { name: 'Claude Code MCP guide' })).toHaveAttribute('href', 'https://code.claude.com/docs/en/mcp-quickstart');
  });

  it('copies the installation value when the operator clicks the code for each provider', async () => {
    const user = userEvent.setup();
    vi.stubGlobal('isSecureContext', true);
    const endpoint = 'https://farm.example.ts.net/mcp';
    server.use(http.get(metadataPath, () => HttpResponse.json({ resource: endpoint })));
    render(<McpGuidePage />);
    const setup = within(screen.getByRole('region', { name: 'Connect an AI assistant' }));
    await setup.findByText(endpoint);
    for (const [client, value] of [
      ['chatgpt', endpoint], ['claude', endpoint], ['grok', endpoint],
      ['codex', `codex mcp add bambastic --url '${endpoint}'`],
      ['claudeCode', `claude mcp add --transport http bambastic '${endpoint}'`],
      ['lm', getMcpConfig(endpoint, 'http')],
      ['goose', `goose session --with-streamable-http-extension '${endpoint}'`],
      ['stdio', getMcpConfig(endpoint, 'stdio')],
    ]) {
      fireEvent.change(setup.getByRole('combobox'), { target: { value: client } });
      const code = setup.getByText((_, element) => element?.tagName === 'CODE' && element.textContent === value);
      await navigator.clipboard.writeText('');
      await user.click(code);
      expect(await navigator.clipboard.readText()).toBe(value);
      expect(await within(code.closest('button')!).findByText('Copied!')).toBeVisible();
    }
  });

  it('passes the API key to the local executable on a protected installation', async () => {
    server.use(http.get('*/api/v1/auth/status', () => HttpResponse.json({ auth_enabled: true })));
    render(<McpGuidePage />);
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'codex' } });
    expect(await screen.findByText(/run codex mcp login bambastic/)).toBeVisible();
    fireEvent.click(screen.getByRole('radio', { name: 'Local executable (stdio)' }));
    const args = `--env BAMBASTIC_API_KEY="\${BAMBASTIC_API_KEY:?}" -- '/absolute/path/to/bambastic' mcp --stdio --server '${window.location.origin}'`;
    expect(screen.getByText(`codex mcp add bambastic ${args}`)).toBeVisible();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'claudeCode' } });
    expect(screen.getByText(`claude mcp add --transport stdio bambastic ${args}`)).toBeVisible();
  });

  it('has a deep link to the actual operation catalog and shows required arguments', () => {
    window.history.replaceState({}, '', '/mcp-guide?tab=tools');
    render(<McpGuidePage />);
    expect(screen.getByRole('link', { name: 'Tool reference', current: 'page' })).toBeInTheDocument();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'printers' } });
    const entry = screen.getByText('get_printer_status').closest('details')!;
    fireEvent.click(entry.querySelector('summary')!);
    expect(entry.open).toBe(true);
    expect(within(entry).getByText('printer_id')).toBeVisible();
    expect(within(entry).getByText('Required')).toBeVisible();
    expect(screen.queryByText('start_queue_item')).not.toBeInTheDocument();
  });

  it('opens the network instructions from the server connection card', () => {
    window.history.replaceState({}, '', '/mcp-guide');
    render(<McpGuidePage />);
    fireEvent.click(screen.getAllByRole('link', { name: 'Tailscale' }).at(-1)!);
    expect(screen.getByRole('link', { name: 'Tailscale', current: 'page' })).toBeInTheDocument();
    expect(screen.getByText(/tailscale serve --bg 8080/)).toBeInTheDocument();
  });
});
