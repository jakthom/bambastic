import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { ExternalLink } from './icons/registry';
import { McpSample } from './McpSample';
import { shellArgument, type McpTransport } from '../utils/mcpSetup';

interface Props {
  client: 'codex' | 'claudeCode';
  endpoint: string;
  authEnabled: boolean;
}

export function McpCliSetup({ client, endpoint, authEnabled }: Props) {
  const { t } = useTranslation();
  const [transport, setTransport] = useState<McpTransport>('http');
  const codex = client === 'codex';
  const local = transport === 'stdio';
  const name = codex ? 'Codex CLI' : 'Claude Code';
  const add = codex ? 'codex mcp add bambastic' : `claude mcp add --transport ${transport} bambastic`;
  const httpCommand = `${add}${codex ? ' --url' : ''} ${shellArgument(endpoint)}`;
  const env = authEnabled ? ' --env BAMBASTIC_API_KEY="${BAMBASTIC_API_KEY:?}"' : '';
  const stdioCommand = `${add}${env} -- '/absolute/path/to/bambastic' mcp --stdio --server ${shellArgument(new URL(endpoint).origin)}`;

  return (
    <div className="space-y-4 text-bambu-gray-light">
      <fieldset className="flex flex-wrap gap-x-5 gap-y-2">
        <legend className="sr-only">{t('mcpGuide.server')}</legend>
        {(['http', 'stdio'] as const).map(value => (
          <label key={value} className="inline-flex cursor-pointer items-center gap-2 text-white">
            <input type="radio" name="mcp-transport" checked={transport === value}
              onChange={() => setTransport(value)} className="accent-bambu-green" />
            {t(value === 'http' ? 'mcpGuide.remoteServer' : 'mcpGuide.localServer')}
          </label>
        ))}
      </fieldset>
      <p>{local ? t('mcpGuide.cliLocal') : t(codex ? 'mcpHosted.codexAdd' : 'mcpHosted.claudeCommand')}</p>
      {local && authEnabled && <p>{t('mcpGuide.cliLocalKey')}</p>}
      <McpSample text={local ? stdioCommand : httpCommand} label={name} />
      {!local && codex && (
        <>
          {authEnabled && <p>{t('mcpHosted.codexLogin')}</p>}
          <details>
            <summary className="cursor-pointer font-medium text-white">{t('settings.tabs.apiKeys')}</summary>
            <div className="mt-2 space-y-2">
              <p>{t('mcpGuide.cliRemoteKey')}</p>
              <McpSample text={`${httpCommand} --bearer-token-env-var BAMBASTIC_API_KEY`} label="Codex CLI" />
            </div>
          </details>
        </>
      )}
      {codex ? <p>{t('mcpHosted.codexUse')}</p> : <McpSample text="claude mcp get bambastic" label="Claude Code" />}
      <a className="inline-flex items-center gap-1 text-sm font-medium text-bambu-green hover:underline"
        href={codex ? 'https://developers.openai.com/codex/mcp' : 'https://code.claude.com/docs/en/mcp-quickstart'}
        target="_blank" rel="noreferrer">
        {t(codex ? 'mcpHosted.codexDocs' : 'mcpHosted.codeDocs')} <ExternalLink className="size-3.5" />
      </a>
    </div>
  );
}
