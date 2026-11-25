import { useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useQuery } from '@tanstack/react-query';
import { Card, CardContent, CardHeader } from '../components/Card';
import { McpSample } from '../components/McpSample';
import { Book, ChevronDown, Code, ExternalLink, Globe, Plug } from '../components/icons/registry';
import { McpCliSetup } from '../components/McpCliSetup';
import { McpHostedSetup } from '../components/McpHostedSetup';
import { McpNetworkHelp } from '../components/McpNetworkHelp';
import { useAuth } from '../contexts/AuthContext';
import { mcpCategories, mcpOperations } from '../data/mcpCatalog';
import { fetchMcpEndpoint, getMcpConfig, getMcpEndpoint, shellArgument } from '../utils/mcpSetup';

const heading = 'text-lg font-semibold text-white';
const bodyText = 'text-bambu-gray-light leading-relaxed';
const textLink = 'inline-flex items-center gap-1.5 text-bambu-green underline underline-offset-4';
const selectStyle = 'min-h-10 w-full rounded-lg border border-bambu-dark-tertiary bg-bambu-dark px-3 py-2 text-white sm:w-auto';
const clients = {
  chatgpt: 'ChatGPT',
  claude: 'Claude',
  claudeCode: 'Claude Code',
  grok: 'Grok',
  codex: 'Codex CLI',
  lm: 'LM Studio',
  goose: 'Goose + Ollama',
  stdio: 'stdio',
};
type Client = keyof typeof clients;

function Setup() {
  const { t } = useTranslation();
  const { authEnabled, hasPermission } = useAuth();
  const [client, setClient] = useState<Client>('chatgpt');
  const { data: advertisedEndpoint } = useQuery({
    queryKey: ['mcp-resource-endpoint'],
    queryFn: ({ signal }) => fetchMcpEndpoint(signal),
    staleTime: Infinity,
    retry: false,
  });
  const endpoint = advertisedEndpoint ?? getMcpEndpoint(window.location.origin);
  const hosted = client === 'chatgpt' || client === 'claude' || client === 'grok';
  const prompt = t('mcpGuide.prompt');
  return (
    <div className="grid items-start gap-5 xl:grid-cols-[minmax(0,1.45fr)_minmax(18rem,1fr)]">
      <div className="min-w-0 space-y-5">
        <Card role="region" aria-labelledby="mcp-client-setup">
          <CardHeader><h2 id="mcp-client-setup" className={heading}>{t('mcpGuide.connect')}</h2></CardHeader>
          <CardContent className="space-y-5">
            <label className="flex flex-col items-start gap-2 font-medium text-white">
              {t('mcpGuide.client')}
              <select value={client} onChange={e => setClient(e.target.value as Client)} className={selectStyle}>
                {Object.entries(clients).map(([id, name]) =>
                  <option key={id} value={id}>{id === 'stdio' ? t('mcpGuide.stdioClient') : name}</option>)}
              </select>
            </label>
            {client === 'lm' || client === 'goose' || client === 'stdio' ? (
              <>
                <ol className={`list-decimal space-y-3 ps-5 marker:font-semibold ${bodyText}`}>
                  {(client === 'lm' ? ['lmModel', 'lmConfig', 'lmStart']
                    : client === 'goose' ? ['gooseModel', 'gooseProvider', 'gooseStart']
                    : ['stdioInstall', 'stdioConfig', 'stdioStart']).map(key => <li key={key}>{t(`mcpGuide.${key}`)}</li>)}
                </ol>
                <McpSample label={client === 'goose' ? 'Goose CLI' : 'mcp.json'} text={client === 'goose'
                  ? `goose session --with-streamable-http-extension ${shellArgument(endpoint)}`
                  : getMcpConfig(endpoint, client === 'stdio' ? 'stdio' : 'http', authEnabled)} />
                {authEnabled && client !== 'goose' && <p className={bodyText}>{t('mcpGuide.protectedConfig')}</p>}
                {client !== 'stdio' && <a className={textLink} href={client === 'lm' ? 'https://lmstudio.ai/docs/app/mcp' : 'https://goose-docs.ai/docs/getting-started/providers/'} target="_blank" rel="noreferrer">
                  {t('mcpGuide.docs')}<ExternalLink className="size-4" />
                </a>}
              </>
            ) : client === 'codex' || client === 'claudeCode' ? (
              <McpCliSetup client={client} endpoint={endpoint} authEnabled={authEnabled} />
            ) : <McpHostedSetup provider={client} endpoint={endpoint} />}
          </CardContent>
        </Card>
        <Card>
          <CardHeader><h2 className={heading}>{t('mcpGuide.tryTitle')}</h2></CardHeader>
          <CardContent className="space-y-3">
            <McpSample text={prompt} label={t('mcpGuide.tryTitle')} prose />
            <p className={bodyText}>{t('mcpGuide.expected')}</p>
          </CardContent>
        </Card>
      </div>
      <Card className="min-w-0">
        <CardHeader><h2 className={heading}>{t('mcpGuide.server')}</h2></CardHeader>
        <CardContent className="space-y-5">
          <div className="space-y-2">
            <h3 className="font-medium text-white">{t('mcpGuide.address')}</h3>
            <McpSample text={endpoint} label={t('mcpGuide.address')} />
            <p className={bodyText}>{t(hosted ? 'mcpNetwork.hostedHelp' : 'mcpGuide.reachable')}</p>
            <Link className={textLink} to="?tab=network">Tailscale</Link>
          </div>
          <div className="space-y-2 border-t border-bambu-dark-tertiary pt-4">
            <h3 className="font-medium text-white">{t('mcpGuide.access')}</h3>
            <p className={bodyText}>{t(authEnabled ? 'mcpGuide.authOn' : 'mcpGuide.authOff')}</p>
            {authEnabled && hasPermission('settings:read') && hasPermission('api_keys:read') && (
              <Link className={textLink} to="/settings?tab=apikeys#card-createapi">{t('settings.tabs.apiKeys')}</Link>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

const categoryLabels: Record<string, string> = {
  archives: 'nav.archives', materials: 'nav.inventory', printers: 'nav.printers', projects: 'nav.projects', queue: 'nav.queue',
};
const tools = [
  ['list_categories', 'list'], ['search_tools', 'search'], ['execute_tool', 'execute'],
] as const;

function Reference() {
  const { t } = useTranslation();
  const [category, setCategory] = useState('');
  const operations = mcpOperations.filter(op => !category || op.category === category);
  return (
    <div className="max-w-4xl space-y-5">
      <Card>
        <CardHeader><h2 className={heading}>{t('mcpGuide.discovery')}</h2></CardHeader>
        <CardContent className="space-y-5">
          <dl className="space-y-3">
            {tools.map(([name, key]) => <div key={name} className="grid gap-x-4 sm:grid-cols-[10rem_1fr]">
              <dt className="font-mono font-semibold text-white">{name}</dt><dd className={bodyText}>{t(`mcpGuide.${key}`)}</dd>
            </div>)}
          </dl>
          <div className="space-y-2">
            <h3 className="font-medium text-white">{t('mcpGuide.example')}</h3>
            <McpSample label={t('mcpGuide.example')} text={'{"name": "list_printers", "arguments": {}}'} />
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 className={heading}>{t('mcpGuide.catalog')}</h2>
            <p className={bodyText}>{t('mcpGuide.catalogIntro', { count: mcpOperations.length })}</p>
          </div>
          <select aria-label={t('mcpGuide.catalog')} className={selectStyle} value={category} onChange={e => setCategory(e.target.value)}>
            <option value="">{t('common.all')}</option>
            {mcpCategories.map(c => <option key={c} value={c}>{categoryLabels[c] ? t(categoryLabels[c]) : c}</option>)}
          </select>
        </CardHeader>
        <CardContent className="divide-y divide-bambu-dark-tertiary">
          {operations.map(op => (
            <details key={op.name} className="group py-1">
              <summary className="flex cursor-pointer items-center justify-between gap-3 rounded py-2">
                <span className="min-w-0">
                  <code className="wrap-anywhere font-semibold text-white">{op.name}</code>
                  <span className="block text-sm text-bambu-gray-light" lang="en">{op.description}</span>
                </span>
                <span className="flex shrink-0 items-center gap-2 text-sm text-bambu-gray">
                  {op.readOnly ? t('fileManager.readOnly') : t('mcpGuide.action')}
                  <ChevronDown className="size-4 group-open:rotate-180" />
                </span>
              </summary>
              <div className="space-y-2 rounded-lg bg-bambu-dark p-3">
                <h3 className="font-medium text-white">{t('mcpGuide.arguments')}</h3>
                {Object.keys(op.inputSchema.properties).length ? (
                  <dl className="grid gap-1">
                    {Object.keys(op.inputSchema.properties).map(arg =>
                      <div key={arg} className="flex flex-wrap items-baseline gap-x-3">
                        <dt className="wrap-anywhere font-mono text-sm text-white">{arg}</dt>
                        <dd className="text-sm text-bambu-gray">{t(op.inputSchema.required.includes(arg) ? 'common.required' : 'common.optional')}</dd>
                      </div>)}
                  </dl>
                ) : <p className={bodyText}>{t('mcpGuide.noArgs')}</p>}
                {op.name === 'submit_queue_item' && <p className={bodyText}>{t('mcpGuide.queueSource')}</p>}
              </div>
            </details>
          ))}
        </CardContent>
      </Card>
    </div>
  );
}

export function McpGuidePage() {
  const { t } = useTranslation();
  const [params] = useSearchParams();
  const tab = params.get('tab');
  const active = tab === 'tools' || tab === 'network' ? tab : 'setup';
  const sections = [
    { id: 'setup', label: t('mcpGuide.setup'), icon: Plug },
    { id: 'tools', label: t('mcpGuide.reference'), icon: Code },
    { id: 'network', label: 'Tailscale', icon: Globe },
  ];
  return (
    <div className="max-w-screen-xl space-y-6 p-4 md:p-8">
      <header>
        <h1 className="flex items-center gap-3 text-2xl font-bold text-white">
          <Book className="size-7 text-bambu-green" />{t('nav.mcpGuide')}
        </h1>
        <p className="mt-1 max-w-prose text-bambu-gray">{t('mcpGuide.intro')}</p>
      </header>
      <nav aria-label={t('nav.mcpGuide')} className="flex flex-wrap border-b border-bambu-dark-tertiary">
        {sections.map(({ id, label, icon: Icon }) => (
          <Link key={id} to={id === 'setup' ? '/mcp-guide' : `?tab=${id}`} aria-current={id === active ? 'page' : undefined}
            className={`-mb-px inline-flex items-center gap-2 border-b-2 px-3 py-3 text-sm font-medium sm:px-4 ${id === active
              ? 'border-bambu-green text-bambu-green' : 'border-transparent text-bambu-gray hover:text-white'}`}>
            <Icon className="size-4" />{label}
          </Link>
        ))}
      </nav>
      {active === 'tools' ? <Reference /> : active === 'network' ? <div className="max-w-4xl"><McpNetworkHelp /></div> : <Setup />}
    </div>
  );
}
