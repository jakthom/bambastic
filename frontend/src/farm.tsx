import { useEffect, useState } from 'react';
import { useFleetFeed, type DeviceState } from './live';
import { intake, IntakeForm, QueueFile, type IntakeRoute } from './intake';
import { message, read, submit } from './http';

type Row = Record<string, unknown> & { id: number };
const display = (row: Row, key: string) =>
  typeof row[key] === 'string' || typeof row[key] === 'number'
    ? String(row[key])
    : '';
const sections = {
  '/': {
    name: 'Printer fleet',
    endpoint: 'printers/',
    columns: [
      ['name', 'Printer'],
      ['model', 'Model'],
      ['location', 'Location'],
    ],
    empty: 'No printers are registered.',
  },
  '/queue': {
    name: 'Print queue',
    endpoint: 'queue/',
    columns: [
      ['archive_name', 'Print'],
      ['printer_name', 'Printer'],
      ['status', 'State'],
      ['position', 'Position'],
    ],
    empty: 'The print queue is empty.',
  },
  '/archives': {
    name: 'Archives',
    endpoint: 'archives/',
    columns: [
      ['filename', 'File'],
      ['print_name', 'Print'],
      ['status', 'State'],
      ['created_at', 'Archived'],
    ],
    empty: 'No archived prints.',
  },
  '/files': {
    name: 'File library',
    endpoint: 'library/files',
    columns: [
      ['filename', 'File'],
      ['file_size', 'Bytes'],
      ['created_at', 'Added'],
    ],
    empty: 'No files in the library.',
  },
  '/inventory': {
    name: 'Spool inventory',
    endpoint: 'inventory/spools',
    columns: [
      ['material', 'Material'],
      ['brand', 'Brand'],
      ['color_name', 'Color'],
      ['label_weight', 'Label weight (g)'],
    ],
    empty: 'No spools in inventory.',
  },
  '/projects': {
    name: 'Projects',
    endpoint: 'projects/',
    columns: [
      ['name', 'Project'],
      ['status', 'State'],
      ['description', 'Description'],
    ],
    empty: 'No projects yet.',
  },
  '/finance': {
    name: 'Cost centers',
    endpoint: 'finance/cost-centers/mine',
    columns: [
      ['name', 'Cost center'],
      ['code', 'Code'],
      ['total_balance', 'Balance'],
      ['budget_used', 'Budget used'],
      ['budget_available', 'Available budget'],
    ],
    empty: 'No cost centers available to this account.',
  },
  '/spoolbuddy': {
    name: 'SpoolBuddy stations',
    endpoint: 'spoolbuddy/devices',
    columns: [
      ['hostname', 'Station'],
      ['device_id', 'Device ID'],
      ['ip_address', 'IP address'],
      ['last_seen', 'Last seen'],
    ],
    empty: 'No SpoolBuddy stations have registered.',
  },
} as const;
export type FarmRoute = keyof typeof sections;
export const farmLinks = Object.entries(sections).map(([href, s]) => ({
  href,
  text: s.name,
}));
export const isFarmRoute = (route: string): route is FarmRoute =>
  Object.hasOwn(sections, route);

function Report({ id, live }: { id: number; live?: DeviceState }) {
  const [checks, setChecks] =
    useState<{ id: string; status: string; params?: { reason?: string } }[]>();
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState('');
  const [value, setValue] = useState<DeviceState>();
  const [error, setError] = useState('');
  useEffect(() => {
    if (live) return;
    const stop = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function refresh() {
      try {
        const v = await read<DeviceState>(`/api/v1/printers/${id}/status`, {
          signal: stop.signal,
        });
        setValue(v);
        setError('');
      } catch (e) {
        if (!stop.signal.aborted) setError(message(e));
      } finally {
        if (!stop.signal.aborted) timer = setTimeout(refresh, 10000);
      }
    }
    void refresh();
    return () => {
      stop.abort();
      clearTimeout(timer);
    };
  }, [id, live]);
  async function diagnose() {
    setChecking(true);
    setCheckError('');
    try {
      const result = await read<{ checks: NonNullable<typeof checks> }>(
        `/api/v1/printers/${id}/diagnostic`,
      );
      setChecks(result.checks);
      if (import.meta.env.DEV)
        for (const check of result.checks)
          window.dispatchEvent(
            new CustomEvent('farm:diagnostic-event', {
              detail: {
                kind: 'printer.diagnostic',
                fields: {
                  printerId: id,
                  check: check.id,
                  outcome: check.status,
                  reason: check.params?.reason,
                },
              },
            }),
          );
    } catch (e) {
      setCheckError(message(e));
    } finally {
      setChecking(false);
    }
  }
  const status = live ?? value;
  if (error && !live) return <span>{error}</span>;
  if (!status) return <span>Checking…</span>;
  return (
    <div>
      <span>
        <i className={status.connected ? 'dot on' : 'dot'} />
        {status.state}
        {status.connected && status.current_print
          ? ` · ${status.progress}% · ${status.current_print}`
          : ''}
      </span>
      {!status.connected && (
        <button className="text" disabled={checking} onClick={diagnose}>
          {checking ? 'Checking connection…' : 'Check connection'}
        </button>
      )}
      {checkError && <p role="alert">{checkError}</p>}
      {checks && (
        <dl>
          {checks.map((c) => (
            <div key={c.id}>
              <dt>{c.id.replaceAll('_', ' ')}</dt>
              <dd>
                {c.status} {c.params?.reason}
              </dd>
            </div>
          ))}
        </dl>
      )}
    </div>
  );
}

export function Farm({
  route,
  allowed,
}: {
  route: FarmRoute;
  allowed: (permission: string) => boolean;
}) {
  const live = useFleetFeed(route === '/' && allowed('websocket:connect'));
  const createRoute = Object.hasOwn(intake, route)
    ? (route as IntakeRoute)
    : undefined;
  const [queueFile, setQueueFile] = useState<{ id: number; name: string }>();
  const fileActions =
    (route === '/files' || route === '/archives') && allowed('queue:create');
  const queueActions =
    route === '/queue' &&
    (allowed('queue:update_all') ||
      allowed('queue:delete_all') ||
      allowed('queue:start_unreviewed') ||
      allowed('queue:delete_own'));
  const [working, setWorking] = useState(false);
  const [actionMessage, setActionMessage] = useState('');
  const config = sections[route];
  const [rows, setRows] = useState<Row[]>();
  const [error, setError] = useState('');
  const [filter, setFilter] = useState('');
  const [adding, setAdding] = useState(false);
  const [revision, reload] = useState(0);
  useEffect(() => {
    const stop = new AbortController();
    setError('');
    read<Row[]>('/api/v1/' + config.endpoint, { signal: stop.signal })
      .then((data) => {
        if (!Array.isArray(data))
          throw new Error('The server returned an unexpected list format.');
        setRows(data);
      })
      .catch((e) => {
        if (!stop.signal.aborted) setError(message(e));
      });
    return () => stop.abort();
  }, [config, revision]);
  const visible = (rows ?? []).filter((r) =>
    config.columns.some(([key]) =>
      display(r, key).toLowerCase().includes(filter.toLowerCase()),
    ),
  );
  async function changeJob(row: Row, action: 'start' | 'cancel') {
    if (
      !window.confirm(
        `${action === 'start' ? 'Release' : 'Cancel'} queue item ${row.id}?`,
      )
    )
      return;
    setWorking(true);
    setActionMessage('');
    try {
      await submit(`/api/v1/queue/${row.id}/${action}`, {});
      reload((v) => v + 1);
    } catch (e) {
      setActionMessage(message(e));
    } finally {
      setWorking(false);
    }
  }
  return (
    <>
      <header className="pagehead">
        <h1>{config.name}</h1>
        {createRoute && allowed(intake[createRoute].permission) && (
          <button className="primary" onClick={() => setAdding(!adding)}>
            {adding ? 'Close form' : intake[createRoute].action}
          </button>
        )}
      </header>
      {adding && createRoute && (
        <IntakeForm
          route={createRoute}
          done={() => {
            setAdding(false);
            reload((r) => r + 1);
          }}
        />
      )}
      {queueFile && (
        <QueueFile
          key={queueFile.id}
          source={route === '/files' ? 'library/files' : 'archives'}
          file={queueFile}
          cancel={() => setQueueFile(undefined)}
        />
      )}
      {actionMessage && <p role="alert">{actionMessage}</p>}
      <section className="sheet">
        <div className="tools">
          <input
            type="search"
            aria-label={'Filter ' + config.name}
            placeholder={'Filter ' + config.name.toLowerCase()}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
          <button onClick={() => reload((r) => r + 1)}>Refresh</button>
        </div>
        {error ? (
          <p role="alert">{error}</p>
        ) : rows === undefined ? (
          <p role="status">Loading…</p>
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  {config.columns.map(([key, name]) => (
                    <th key={key}>{name}</th>
                  ))}
                  {route === '/' && <th>Live status</th>}
                  {(fileActions || queueActions) && <th>Actions</th>}
                </tr>
              </thead>
              <tbody>
                {visible.map((row) => (
                  <tr key={row.id}>
                    {config.columns.map(([key]) => (
                      <td key={key}>
                        {key === 'filename' &&
                        /\.(gcode|3mf)$/i.test(display(row, key)) ? (
                          <a
                            href={
                              '/gcode-viewer?' +
                              new URLSearchParams({
                                source:
                                  route === '/files' ? 'library' : 'archive',
                                id: String(row.id),
                              })
                            }
                          >
                            {display(row, key)}
                          </a>
                        ) : (
                          display(row, key) || '—'
                        )}
                      </td>
                    ))}
                    {route === '/' && (
                      <td>
                        <Report id={row.id} live={live[row.id]} />
                      </td>
                    )}
                    {fileActions && (
                      <td>
                        <button
                          onClick={() =>
                            setQueueFile({
                              id: row.id,
                              name: display(row, 'filename'),
                            })
                          }
                        >
                          Queue file
                        </button>
                      </td>
                    )}
                    {queueActions && (
                      <td>
                        {row.status === 'pending' && (
                          <div className="actions">
                            {(allowed('queue:update_all') ||
                              allowed('queue:start_unreviewed')) && (
                              <button
                                disabled={working}
                                onClick={() => changeJob(row, 'start')}
                              >
                                Release
                              </button>
                            )}
                            {(allowed('queue:delete_all') ||
                              allowed('queue:delete_own')) && (
                              <button
                                disabled={working}
                                onClick={() => changeJob(row, 'cancel')}
                              >
                                Cancel
                              </button>
                            )}
                          </div>
                        )}
                      </td>
                    )}
                  </tr>
                ))}
              </tbody>
            </table>
            {!visible.length && (
              <p className="empty">
                {rows.length ? 'No matches.' : config.empty}
              </p>
            )}
          </div>
        )}
      </section>
    </>
  );
}
