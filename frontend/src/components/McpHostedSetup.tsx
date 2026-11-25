import { useTranslation } from 'react-i18next';
import { McpSample } from './McpSample';
import { shellArgument } from '../utils/mcpSetup';
import { ExternalLink } from './icons/registry';

const providers = {
  chatgpt: {
    steps: ['chatgptAdd', 'chatgptConnect', 'chatgptUse'],
    links: [
      ['openChatgpt', 'https://chatgpt.com/plugins'],
      ['connectorDocs', 'https://developers.openai.com/api/docs/guides/custom-mcp-server'],
      ['tunnelDocs', 'https://developers.openai.com/api/docs/guides/secure-mcp-tunnels'],
    ],
  },
  claude: {
    steps: ['claudeAdd', 'claudeConnect', 'claudeUse'],
    links: [
      ['connectorDocs', 'https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp'],
      ['codeDocs', 'https://code.claude.com/docs/en/mcp-quickstart'],
    ],
  },
  grok: {
    steps: ['grokAdd', 'grokConnect', 'grokUse'],
    links: [
      ['openGrok', 'https://grok.com/connectors'],
      ['connectorDocs', 'https://docs.x.ai/grok/connectors'],
      ['tunnelDocs', 'https://docs.x.ai/grok/connectors/custom-mcp-tunneling'],
    ],
  },
} as const;

export function McpHostedSetup({ provider, endpoint }: { provider: keyof typeof providers; endpoint: string }) {
  const { t } = useTranslation();
  const { steps, links } = providers[provider];
  const shellUrl = shellArgument(endpoint);

  return (
    <div className="space-y-4 text-bambu-gray-light">
      <ol className="list-decimal space-y-3 pl-5 marker:font-semibold">
        {steps.map((key, index) => (
          <li key={key} className="pl-1 leading-relaxed">
            {t(`mcpHosted.${key}`)}
            {index === 1 && (
              <div className="mt-2">
                <McpSample text={endpoint} label={t('mcpGuide.address')} />
              </div>
            )}
          </li>
        ))}
      </ol>
      {provider === 'claude' && (
        <details>
          <summary className="cursor-pointer font-medium text-white">{t('mcpHosted.claudePrivate')}</summary>
          <p className="mt-2 leading-relaxed">{t('mcpHosted.claudeCommand')}</p>
          <McpSample text={`claude mcp add --transport http bambastic ${shellUrl}`} label="Claude Code" />
        </details>
      )}
      <div className="flex flex-wrap gap-x-5 gap-y-2 text-sm">
        {links.map(([label, href]) => (
          <a key={href} href={href} target="_blank" rel="noopener noreferrer" className="inline-flex items-center gap-1 font-medium text-bambu-green hover:underline">
            {t(`mcpHosted.${label}`)} <ExternalLink className="h-3.5 w-3.5" />
          </a>
        ))}
      </div>
    </div>
  );
}
