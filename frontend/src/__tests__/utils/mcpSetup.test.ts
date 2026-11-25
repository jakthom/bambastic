import { describe, expect, it } from 'vitest';
import { getMcpConfig, getMcpEndpoint } from '../../utils/mcpSetup';

describe('MCP setup examples', () => {
  it('uses the installation origin and excludes page paths and credentials', () => {
    expect(getMcpEndpoint('https://operator:pass@farm.example:8443/mcp-guide?token=secret#setup'))
      .toBe('https://farm.example:8443/mcp');
    expect(getMcpEndpoint('http://[::1]:8080/')).toBe('http://[::1]:8080/mcp');
  });

  it('generates HTTP config with an optional bearer header', () => {
    const origin = 'https://farm.example:8443';
    const entry = { url: 'https://farm.example:8443/mcp' };
    expect(JSON.parse(getMcpConfig(origin, 'http'))).toEqual({ mcpServers: { bambastic: entry } });
    expect(JSON.parse(getMcpConfig(origin, 'http', true)).mcpServers.bambastic).toEqual({
      ...entry, headers: { Authorization: 'Bearer <YOUR_BAMBASTIC_API_KEY>' },
    });
  });

  it('passes the installation base to the stdio executable and sets an optional API-key environment variable', () => {
    const origin = 'http://127.0.0.1:8080/';
    const entry = {
      command: '/absolute/path/to/bambastic',
      args: ['mcp', '--stdio', '--server', 'http://127.0.0.1:8080'],
    };
    expect(JSON.parse(getMcpConfig(origin, 'stdio'))).toEqual({ mcpServers: { bambastic: entry } });
    expect(JSON.parse(getMcpConfig(origin, 'stdio', true)).mcpServers.bambastic).toEqual({
      ...entry, env: { BAMBASTIC_API_KEY: '<YOUR_BAMBASTIC_API_KEY>' },
    });
  });
});
