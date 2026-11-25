import { useTranslation } from 'react-i18next';
import { Card, CardContent, CardHeader } from './Card';
import { McpSample } from './McpSample';
import { ChevronDown, ExternalLink, Globe } from './icons/registry';

const serveCommands = 'tailscale serve --bg 8080\ntailscale serve status';
const startCommand = 'BAMBASTIC_PUBLIC_URL=https://machine.tailnet.ts.net just run';

export function McpNetworkHelp() {
  const { t } = useTranslation();
  const summaryStyle = 'flex cursor-pointer items-center justify-between gap-3 rounded py-3 font-medium text-white';
  const docStyle = 'inline-flex items-center gap-1 text-bambu-green underline underline-offset-2';

  return (
    <Card>
      <CardHeader>
        <h2 className="flex items-center gap-2 text-lg font-semibold text-white">
          <Globe className="h-5 w-5 text-bambu-green" />
          {t('mcpNetwork.title')}
        </h2>
      </CardHeader>
      <CardContent className="divide-y divide-bambu-dark-tertiary text-sm leading-relaxed text-bambu-gray-light">
        <details className="group" open>
          <summary className={summaryStyle}>
            {t('mcpNetwork.tailnet')} <ChevronDown className="size-4 shrink-0 group-open:rotate-180" />
          </summary>
          <div className="space-y-3 pb-3">
            <p>{t('mcpNetwork.serveHelp')}</p>
            <McpSample text={serveCommands} label="Tailscale CLI" />
            <p>{t('mcpNetwork.originHelp')}</p>
            <McpSample text={startCommand} label="Bambastic CLI" />
            <p>{t('mcpNetwork.connectHelp')}</p>
            <a className={docStyle} href="https://tailscale.com/docs/reference/tailscale-cli/serve" target="_blank" rel="noreferrer">
              Tailscale Serve <ExternalLink className="h-3.5 w-3.5" />
            </a>
          </div>
        </details>
        <details className="group">
          <summary className={summaryStyle}>
            {t('mcpNetwork.local')} <ChevronDown className="size-4 shrink-0 group-open:rotate-180" />
          </summary>
          <p className="pb-3">{t('mcpNetwork.localHelp')}</p>
        </details>
        <details className="group">
          <summary className={summaryStyle}>
            {t('mcpNetwork.hosted')} <ChevronDown className="size-4 shrink-0 group-open:rotate-180" />
          </summary>
          <div className="space-y-3 pb-3">
            <p>{t('mcpNetwork.hostedHelp')}</p>
            <p>{t('mcpNetwork.tunnelHelp')}</p>
            <a className={docStyle} href="https://developers.openai.com/api/docs/guides/secure-mcp-tunnels" target="_blank" rel="noreferrer">
              OpenAI Secure MCP Tunnel <ExternalLink className="h-3.5 w-3.5" />
            </a>
          </div>
        </details>
      </CardContent>
    </Card>
  );
}
