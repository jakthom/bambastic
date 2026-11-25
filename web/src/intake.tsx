import { useEffect, useRef, useState, type FormEvent } from 'react';
import { message, read, submit } from './http';

export const intake = {
  '/': {
    action: 'Add printer',
    permission: 'printers:create',
    endpoint: 'printers/',
    fields: [
      ['name', 'Printer name', 'text'],
      ['model', 'Model', 'text'],
      ['ip_address', 'IP address', 'text'],
      ['serial_number', 'Serial number', 'text'],
      ['access_code', 'LAN access code', 'password'],
    ],
  },
  '/inventory': {
    action: 'Add spool',
    permission: 'inventory:update',
    endpoint: 'inventory/spools',
    fields: [
      ['material', 'Material', 'text'],
      ['brand', 'Brand (optional)', 'optional'],
      ['color_name', 'Color (optional)', 'optional'],
      ['label_weight', 'Label weight (g)', 'number'],
    ],
  },
  '/projects': {
    action: 'Create project',
    permission: 'projects:create',
    endpoint: 'projects/',
    fields: [
      ['name', 'Project name', 'text'],
      ['description', 'Description (optional)', 'optional'],
    ],
  },
  '/archives': {
    action: 'Upload archive',
    permission: 'archives:create',
    endpoint: 'archives/upload',
    fields: [['file', '3MF or G-code file', 'file']],
  },
  '/files': {
    action: 'Upload file',
    permission: 'library:upload',
    endpoint: 'library/files',
    fields: [['file', 'File', 'file']],
  },
  '/finance': {
    action: 'Create cost center',
    permission: 'cost_centers:create',
    endpoint: 'finance/cost-centers',
    fields: [
      ['name', 'Cost center name', 'text'],
      ['description', 'Description (optional)', 'optional'],
    ],
  },
} as const;
export type IntakeRoute = keyof typeof intake;

export function IntakeForm({
  route,
  done,
}: {
  route: IntakeRoute;
  done: () => void;
}) {
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const { fields, action, endpoint } = intake[route];
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const data = new FormData(event.currentTarget);
    setBusy(true);
    setError('');
    try {
      const path = '/api/v1/' + endpoint;
      if (data.has('file')) await read(path, { method: 'POST', body: data });
      else {
        const input: Record<string, string | number> = {};
        for (const [name, , type] of fields) {
          const value = String(data.get(name) ?? '');
          if (value) input[name] = type === 'number' ? Number(value) : value;
        }
        await submit(path, input);
      }
      done();
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <form className="sheet narrow" onSubmit={save}>
      <h2>{action}</h2>
      {fields.map(([name, label, type]) => (
        <label key={name}>
          {label}
          <input
            type={type === 'optional' ? 'text' : type}
            name={name}
            required={type !== 'optional'}
            min={type === 'number' ? 0 : undefined}
            accept={route === '/archives' ? '.3mf,.gcode' : undefined}
            autoComplete="off"
          />
        </label>
      ))}
      {error && <p role="alert">{error}</p>}
      <button className="primary" disabled={busy}>
        Save
      </button>
    </form>
  );
}

interface Plate {
  index: number;
  name: string;
  has_gcode: boolean;
}
interface Printer {
  id: number;
  name: string;
  is_active: boolean;
}
export function QueueFile({
  file,
  source,
  cancel,
}: {
  file: { id: number; name: string };
  source: 'archives' | 'library/files';
  cancel: () => void;
}) {
  const [choices, setChoices] = useState<{
    plates: Plate[];
    printers: Printer[];
  }>();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const intent = useRef({ body: '', key: '' });
  useEffect(() => {
    const stop = new AbortController();
    Promise.all([
      read<{ plates: Plate[] }>(`/api/v1/${source}/${file.id}/plates`, {
        signal: stop.signal,
      }),
      read<Printer[]>('/api/v1/printers/', { signal: stop.signal }),
    ])
      .then(([details, printers]) =>
        setChoices({
          plates: details.plates.filter((p) => p.has_gcode),
          printers: printers.filter((p) => p.is_active),
        }),
      )
      .catch((e) => {
        if (!stop.signal.aborted) setError(message(e));
      });
    return () => stop.abort();
  }, [source, file.id]);
  async function queue(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError('');
    const form = new FormData(event.currentTarget);
    const body = JSON.stringify({
      [source === 'archives' ? 'archive_id' : 'library_file_id']: file.id,
      printer_id: Number(form.get('printer')),
      plate_id: Number(form.get('plate')),
      manual_start: true,
    });
    if (intent.current.body !== body)
      intent.current = {
        body,
        key: Array.from(crypto.getRandomValues(new Uint32Array(4))).join('-'),
      };
    try {
      await read('/api/v1/queue/', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Idempotency-Key': intent.current.key,
        },
        body,
      });
      location.assign('/queue');
    } catch (e) {
      setError(message(e));
      setBusy(false);
    }
  }
  return (
    <form className="sheet narrow" onSubmit={queue}>
      <h2>Queue {file.name}</h2>
      {error && <p role="alert">{error}</p>}
      {choices ? (
        <>
          {choices.plates.length && choices.printers.length ? (
            <>
              <label>
                Plate
                <select name="plate">
                  {choices.plates.map((p) => (
                    <option key={p.index} value={p.index}>
                      {p.name || 'Plate ' + p.index}
                    </option>
                  ))}
                </select>
              </label>
              <label>
                Printer
                <select name="printer">
                  {choices.printers.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </label>
              <p>The job waits in the queue for manual release.</p>
              <button className="primary" disabled={busy}>
                Add to queue
              </button>
            </>
          ) : (
            <p>
              {choices.plates.length
                ? 'Register an active printer to queue this job.'
                : 'Slice this file before adding it to the queue.'}
            </p>
          )}
        </>
      ) : (
        !error && <p>Loading plates and printers…</p>
      )}
      <button type="button" onClick={cancel} disabled={busy}>
        Close
      </button>
    </form>
  );
}
