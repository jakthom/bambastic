export type McpTransport = 'http' | 'stdio';

export function getMcpEndpoint(origin: string): string {
  return `${new URL(origin).origin}/mcp`;
}

export async function fetchMcpEndpoint(signal: AbortSignal): Promise<string> {
  const response = await fetch('/.well-known/oauth-protected-resource/mcp', { signal });
  if (!response.ok) throw new Error('MCP resource metadata unavailable');
  const { resource } = await response.json();
  const url = new URL(resource);
  if (!['https:', 'http:'].includes(url.protocol) || url.href !== getMcpEndpoint(url.origin)) {
    throw new Error('Invalid MCP resource address');
  }
  return url.href;
}

export function shellArgument(value: string): string {
  return "'" + value.replaceAll("'", "'\\''") + "'";
}

/** LM Studio's mcp.json format. A user fills in secrets on the client. */
export function getMcpConfig(origin: string, transport: McpTransport, withApiKey = false): string {
  const keyPlaceholder = '<YOUR_BAMBASTIC_API_KEY>';
  const server = transport === 'http'
    ? {
        url: getMcpEndpoint(origin),
        ...(withApiKey && { headers: { Authorization: `Bearer ${keyPlaceholder}` } }),
      }
    : {
        command: '/absolute/path/to/bambastic',
        args: ['mcp', '--stdio', '--server', new URL(origin).origin],
        ...(withApiKey && { env: { BAMBASTIC_API_KEY: keyPlaceholder } }),
      };

  return JSON.stringify({ mcpServers: { bambastic: server } }, null, 2);
}
