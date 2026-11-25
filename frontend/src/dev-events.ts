export {};
// Only the development entry point loads this module. Exclude query strings,
// fragments, input values and response bodies from every report.
const session = Array.from(crypto.getRandomValues(new Uint32Array(4))).join(
  '-',
);
let sequence = 0;
function record(kind: string, fields: Record<string, unknown> = {}) {
  return {
    seq: ++sequence,
    time: new Date().toISOString(),
    route: location.pathname.replace(/^(\/confirm)\/.*/, '$1'),
    kind,
    fields,
  };
}
type Entry = ReturnType<typeof record>;
let events: Entry[] = [];
let recent: Entry[] = [];
function send(path: string, body: object) {
  void fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session, ...body }),
    keepalive: true,
  }).catch(() => {});
}
function event(kind: string, fields?: Record<string, unknown>) {
  const entry = record(kind, fields);
  events.push(entry);
  recent = [...recent.slice(-15), entry];
  if (events.length >= 32) flush();
}
function flush() {
  if (events.length) {
    send('/uie', { events });
    events = [];
  }
}
function crash(source: string, error: unknown) {
  const entry = record('error');
  send('/crashes', {
    crash: {
      ...entry,
      source,
      message: error instanceof Error ? error.message : String(error),
      recent,
    },
  });
  flush();
}
event('navigation');
document.addEventListener('click', (e) => {
  const control = (e.target as Element | null)?.closest(
    'a,button,select,input',
  );
  if (control)
    event('interaction', {
      control: control.tagName.toLowerCase(),
      type: control.getAttribute('type'),
    });
});
window.addEventListener('error', (e) =>
  crash('window.error', e.error || e.message),
);
window.addEventListener('unhandledrejection', (e) =>
  crash('unhandledrejection', e.reason),
);
window.addEventListener('pagehide', flush);
window.setInterval(flush, 1500);

window.addEventListener('farm:diagnostic-event', (e) => {
  const { kind, fields } = (e as CustomEvent).detail;
  event(kind, fields);
});
