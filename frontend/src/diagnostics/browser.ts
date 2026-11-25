import type { Diagnostics, Fields } from './types';
import type { QueryClient } from '@tanstack/react-query';

type UIEvent = { seq: number; time: string; route: string; kind: string; fields: Fields };
type Packet = { session: string; crash: Record<string, unknown> };
const pendingKey = 'bambastic.dev.pendingCrashes';
const identifier = /^[\w.:-]{1,100}$/;

function pathOnly(value: string) {
  try {
    const url = new URL(value, location.href);
    if (url.origin !== location.origin) return '[external]';
    return url.pathname.replace(/(\/confirm\/)[^/]+/g, '$1[redacted]').replace(/\/[A-Za-z0-9_-]{32,}(?=\/|$)/g, '/[id]').slice(0, 220);
  } catch { return '[unknown]'; }
}

function clean(text: string, limit = 1600) {
  return text.slice(0, limit * 2)
    .replace(/(https?:\/\/)[^/@\s]+:[^/@\s]+@/gi, '$1')
    .replace(/(\/confirm\/)[^/\s]+/gi, '$1[redacted]')
    .replace(/[?#][^\s)]+/g, suffix => suffix.match(/:\d+:\d+$/)?.[0] ?? '')
    .replace(/\b(bearer|basic)\s+[\w.+=/-]+/gi, '$1 [redacted]')
    .replace(/(["']?(?:password|passphrase|authorization|cookie|token|access_token|refresh_token|api[_-]?key|access[_-]?code|secret)["']?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;}]+)/gi, '$1[redacted]')
    .replace(/\b(?:bb_[\w-]+|eyJ[\w-]+\.[\w-]+\.[\w-]+)\b/g, '[redacted]')
    .slice(0, limit);
}

function element(target: EventTarget | null) {
  if (!(target instanceof Element)) return '';
  const control = target.closest('button,a,input,select,textarea,[role],[data-testid]') ?? target;
  const id = control.getAttribute('data-testid') || control.id;
  if (id && identifier.test(id)) return control.localName + '#' + id;
  // Distinguish adjacent controls without collecting labels, link queries or input values.
  const parts = [];
  for (let node: Element | null = control; node && parts.length < 5; node = node.parentElement) {
    let index = 1;
    for (let sibling = node.previousElementSibling; sibling; sibling = sibling.previousElementSibling) {
      if (sibling.localName === node.localName) index++;
    }
    parts.unshift(node.localName + ':nth-of-type(' + index + ')');
  }
  return parts.join('>');
}

function errorDetails(error: unknown) {
  if (error instanceof Error) {
    return { name: clean(error.name), message: clean(error.message), stack: clean(error.stack ?? '', 7500) };
  }
  return { name: 'ThrownValue', message: typeof error === 'string' ? clean(error) : typeof error, stack: '' };
}

export function installDiagnostics() {
  if (!import.meta.env.DEV || window.bambasticDiagnostics) return;
  const session = crypto.randomUUID?.() ?? Date.now().toString(36) + Math.random().toString(36).slice(2);
  const send = window.fetch.bind(window);
  const recent: UIEvent[] = [];
  const queue: UIEvent[] = [];
  const pending: Packet[] = [];
  const state: Record<string, Fields> = {};
  const requests = new Map<number, Fields>();
  let queryClient: QueryClient | undefined;
  let seq = 0;
  let dropped = 0;
  let sending = false;
  let sendingCrashes = false;

  function event(kind: string, fields: Fields = {}) {
    const item = { seq: ++seq, time: new Date().toISOString(), route: pathOnly(location.href), kind, fields };
    recent.push(item);
    if (recent.length > 50) recent.shift();
    if (queue.length >= 1000) { queue.shift(); dropped++; }
    queue.push(item);
  }

  async function post(path: string, body: string) {
    try {
      const response = await send(path, { method: 'POST', headers: { 'Content-Type': 'application/json' },
        body, keepalive: new Blob([body]).size < 60_000, credentials: 'omit', redirect: 'error',
        signal: AbortSignal.timeout(10_000) });
      return response.ok;
    } catch { return false; }
  }

  async function flushEvents() {
    if (sending || !queue.length) return;
    sending = true;
    const batch = queue.splice(0, 64);
    const missed = dropped;
    dropped = 0;
    try {
      if (await post('/uie', JSON.stringify({ session, dropped: missed, events: batch }))) {
        if (queue.length >= 64) window.setTimeout(flushEvents, 0);
      } else {
        queue.unshift(...batch);
        const excess = Math.max(0, queue.length - 1000);
        queue.splice(0, excess);
        dropped += missed + excess;
      }
    } finally { sending = false; }
  }

  function persistCrashes() {
    try {
      if (pending.length) sessionStorage.setItem(pendingKey, JSON.stringify(pending));
      else sessionStorage.removeItem(pendingKey);
    } catch { /* The in-memory queue still works when storage is unavailable. */ }
  }

  async function flushCrashes() {
    if (sendingCrashes || !pending.length) return;
    sendingCrashes = true;
    try {
      while (pending.length) {
        const report = pending[0];
        if (!await post('/crashes', JSON.stringify(report))) break;
        const index = pending.indexOf(report);
        if (index >= 0) pending.splice(index, 1);
        persistCrashes();
      }
    } finally { sendingCrashes = false; }
  }

  try {
    const saved: unknown = JSON.parse(sessionStorage.getItem(pendingKey) ?? '[]');
    if (Array.isArray(saved)) pending.push(...saved.filter(p => p && typeof p.session === 'string' && p.crash).slice(-8));
  } catch { /* Ignore incomplete records from an earlier tab load. */ }

  function crash(source: string, error: unknown, componentStack?: string | null, fields: Fields = {}) {
    event('crash', { source });
    const queries = queryClient?.getQueryCache().getAll() ?? [];
    const colors = getComputedStyle(document.documentElement);
    const report = {
      id: session + '-' + seq, seq, time: new Date().toISOString(), source, ...errorDetails(error),
      cause: error instanceof Error && error.cause ? errorDetails(error.cause) : undefined,
      componentStack: clean(componentStack ?? '', 7500), fields,
      context: { ...state, route: pathOnly(location.href), mode: import.meta.env.MODE,
        themeClasses: document.documentElement.className.slice(0, 240),
        palette: { scheme: colors.colorScheme, background: colors.getPropertyValue('--bg-primary').trim(),
          foreground: colors.getPropertyValue('--text-primary').trim() },
        systemDark: window.matchMedia?.('(prefers-color-scheme: dark)').matches,
        requests: [...requests.values()].slice(-25),
        queries: { total: queries.length, fetching: queryClient?.isFetching(), mutating: queryClient?.isMutating(),
          failed: queries.filter(q => q.state.status === 'error').slice(-20).map(q => {
            const family = q.queryKey[0];
            return typeof family === 'string' && identifier.test(family) ? family : 'other';
          }) },
        viewport: [innerWidth, innerHeight], pixelRatio: devicePixelRatio, language: navigator.language,
        browser: navigator.userAgent, online: navigator.onLine, visibility: document.visibilityState,
        ready: document.readyState, focused: element(document.activeElement), timeOrigin: performance.timeOrigin,
        uptimeMs: Math.round(performance.now()), queuedEvents: queue.length, droppedEvents: dropped },
      recent: recent.slice(),
    };
    pending.push({ session, crash: report });
    if (pending.length > 8) pending.shift();
    persistCrashes();
    void flushCrashes();
    void flushEvents();
  }

  function context(name: string, fields: Fields) {
    state[name] = fields;
    event(name, fields);
  }

  const diagnostics: Diagnostics = { event, context, crash,
    queries(client) {
      queryClient = client;
      client.getQueryCache().subscribe(({ type, query }) => {
        const key = query.queryKey[0];
        event('query.' + type, { family: typeof key === 'string' && identifier.test(key) ? key : 'other',
          status: query.state.status, fetching: query.state.fetchStatus });
      });
      client.getMutationCache().subscribe(e => {
        if ('mutation' in e && e.mutation) event('mutation.' + e.type, { status: e.mutation.state.status });
      });
    },
    socket(ws) {
      const path = pathOnly(ws.url.replace(/^ws/, 'http'));
      context('websocket', { path, phase: 'connecting' });
      for (const phase of ['open', 'error', 'close']) ws.addEventListener(phase, e => {
        context('websocket', { path, phase, code: e instanceof CloseEvent ? e.code : 0 });
      });
    },
  };
  window.bambasticDiagnostics = diagnostics;
  window.addEventListener('error', (e: Event) => {
    if (e instanceof ErrorEvent) crash('window.error', e.error ?? e.message, null,
      { file: pathOnly(e.filename), line: e.lineno, column: e.colno });
    else if (e.target instanceof Element) {
      const path = pathOnly(e.target.getAttribute('src') ?? e.target.getAttribute('href') ?? '');
      event('resource.error', { target: element(e.target), path });
      if (e.target instanceof HTMLScriptElement) crash('script.load', 'Script failed to load', null, { path });
    }
  }, true);
  window.addEventListener('unhandledrejection', e => crash('unhandledrejection', e.reason));
  installActivity(event, context, send, requests);
  const flush = () => { void flushEvents(); void flushCrashes(); };
  window.setInterval(flush, 750);
  window.addEventListener('pagehide', flush);
  window.addEventListener('online', flush);
  document.addEventListener('visibilitychange', () => { if (document.hidden) flush(); });
  event('session.start', { mode: import.meta.env.MODE });
  flush();
}

function installActivity(event: Diagnostics['event'], context: Diagnostics['context'], send: typeof fetch, requests: Map<number, Fields>) {
  const counts: Record<string, number> = {};
  const frequent = new Set('deviceorientation devicemotion deviceorientationabsolute pointermove pointerrawupdate mousemove touchmove scroll wheel resize dragover drag mouseover mouseout mouseenter mouseleave pointerover pointerout pointerenter pointerleave timeupdate progress selectionchange animationiteration'.split(' '));
  const types = new Set('click dblclick contextmenu pointerdown pointerup pointercancel keydown keyup beforeinput input change submit reset invalid focusin focusout copy cut paste dragstart drop touchstart touchend toggle cancel close play pause ended fullscreenchange'.split(' '));
  // DOM event families vary across browsers, input devices and media elements.
  for (const object of [document, document.documentElement, window]) {
    for (const name in object) if (name.startsWith('on')) types.add(name.slice(2));
  }
  const separate = new Set('error unhandledrejection beforeunload unload visibilitychange'.split(' '));
  for (const type of types) {
    if (separate.has(type)) continue;
    if (frequent.has(type)) {
      window.addEventListener(type, () => { counts[type] = (counts[type] ?? 0) + 1; }, { capture: true, passive: true });
      continue;
    }
    window.addEventListener(type, e => {
      const fields: Fields = { target: element(e.target) };
      if (e instanceof KeyboardEvent) fields.key = ['Enter','Escape','Tab','ArrowDown','ArrowUp','ArrowLeft','ArrowRight'].includes(e.key) ? e.key : 'other';
      if (e instanceof MouseEvent) fields.button = e.button;
      event('ui.' + type, fields);
    }, { capture: true, passive: true });
  }
  const mutations = new MutationObserver(records => { counts.domMutations = (counts.domMutations ?? 0) + records.length; });
  mutations.observe(document.documentElement, { subtree: true, childList: true, attributes: true, characterData: true });
  window.setInterval(() => {
    if (Object.keys(counts).length) {
      event('ui.activity', { ...counts });
      for (const key of Object.keys(counts)) delete counts[key];
    }
  }, 1000);
  document.addEventListener('visibilitychange', () => event('visibility', { state: document.visibilityState }));
  const dispatch = window.dispatchEvent.bind(window);
  window.dispatchEvent = e => {
    if (e instanceof CustomEvent) event('app.event', { type: identifier.test(e.type) ? e.type : 'custom' });
    return dispatch(e);
  };
  let requestId = 0;
  window.fetch = async (input, init) => {
    const raw = input instanceof Request ? input.url : String(input);
    const fields = { requestId: ++requestId, path: pathOnly(raw), method: init?.method ?? (input instanceof Request ? input.method : 'GET') };
    const start = performance.now();
    requests.set(fields.requestId, fields);
    event('fetch.start', fields);
    try {
      const result = await send(input, init);
      event('fetch.end', { ...fields, status: result.status, ms: Math.round(performance.now() - start) });
      return result;
    } catch (error) {
      event('fetch.error', { ...fields, name: error instanceof Error ? error.name : 'Error' });
      throw error;
    } finally {
      requests.delete(fields.requestId);
    }
  };
  for (const level of ['debug','info','log','warn','error'] as const) {
    const original = console[level].bind(console);
    console[level] = (...args: unknown[]) => {
      original(...args);
      event('console.' + level, { message: typeof args[0] === 'string' ? clean(args[0], 300) : typeof args[0], arguments: args.length });
    };
  }
  if (typeof PerformanceObserver !== 'undefined') {
    for (const type of ['resource','navigation','paint','longtask','largest-contentful-paint','layout-shift']) {
      if (!PerformanceObserver.supportedEntryTypes?.includes(type)) continue;
      new PerformanceObserver(list => {
        for (const entry of list.getEntries()) {
          const path = pathOnly(entry.name);
          if (path === '/uie' || path === '/crashes') continue;
          if (type === 'resource') {
            const r = entry as PerformanceResourceTiming;
            event('resource', { path, initiator: r.initiatorType, ms: Math.round(r.duration), bytes: r.transferSize });
          } else event('performance.' + type, { ms: Math.round(entry.duration), start: Math.round(entry.startTime) });
        }
      }).observe({ type, buffered: true });
    }
  }
  import.meta.hot?.on('vite:beforeUpdate', update => event('vite.update', { count: update.updates.length }));
  import.meta.hot?.on('vite:beforeFullReload', () => event('vite.reload'));
  import.meta.hot?.on('vite:error', ({ err }) => window.bambasticDiagnostics?.crash('vite.error', err.message, err.stack));
  context('browser', { language: navigator.language, online: navigator.onLine, mode: import.meta.env.MODE });
}
